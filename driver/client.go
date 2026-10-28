package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-wasmcontainers/engine/api"
)

// Client speaks the engine's API.
type Client struct {
	hc   *http.Client
	base string
}

// NewClient talks to base through hc: a unix-socket transport natively,
// the engine's in-process transport in a browser. base may or may not end
// with an API version ("http://wctr" or "http://wctr/v1.47", which is what
// the engine's api.Client.HTTP returns): the version is never doubled, and
// requests always go to this client's API version.
func NewClient(hc *http.Client, base string) *Client {
	return &Client{hc: hc, base: StripVersion(base) + "/v" + api.APIVersion}
}

var versionSuffix = regexp.MustCompile(`/v[0-9]+\.[0-9]+/*$`)

// StripVersion removes a trailing "/vMAJOR.MINOR" (and slashes) from base.
func StripVersion(base string) string {
	return strings.TrimRight(versionSuffix.ReplaceAllString(strings.TrimRight(base, "/"), ""), "/")
}

// Dial connects to "unix:///path/to/socket", wctr's host syntax.
func Dial(host string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "unix" || u.Path == "" {
		return nil, fmt.Errorf("unsupported engine host %q: only unix:///path is supported", host)
	}
	sock := u.Path
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableCompression: true,
	}
	return NewClient(&http.Client{Transport: tr}, "http://wctr"), nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, ctype string) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the engine: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e api.ErrorResponse
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		if msg == "" {
			msg = resp.Status
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s %s: %s", ErrNotFound, method, path, msg)
		}
		return nil, fmt.Errorf("engine: %s %s: %s", method, path, msg)
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, path, q, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// imageInspect is the part of GET /images/{name}/json the builder reads.
type imageInspect struct {
	ID           string      `json:"Id"`
	Os           string      `json:"Os"`
	Architecture string      `json:"Architecture"`
	Author       string      `json:"Author"`
	Config       ImageConfig `json:"Config"`
	RootFS       struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

func imagePath(ref string) string {
	// Names keep their slashes: the route is /images/{name...}/json.
	return "/images/" + strings.ReplaceAll(url.PathEscape(ref), "%2F", "/")
}

// InspectImage implements Engine.
func (c *Client) InspectImage(ctx context.Context, ref string) (*Image, error) {
	var ii imageInspect
	if err := c.json(ctx, http.MethodGet, imagePath(ref)+"/json", nil, nil, &ii); err != nil {
		return nil, err
	}
	return &Image{ID: ii.ID, OS: ii.Os, Architecture: ii.Architecture, Author: ii.Author, Config: ii.Config, Layers: len(ii.RootFS.Layers)}, nil
}

// PullImage implements Engine.
func (c *Client) PullImage(ctx context.Context, ref string, progress io.Writer) error {
	resp, err := c.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readMessages(resp.Body, progress, nil)
}

// readMessages reads a JSON message stream; an error message is an error.
func readMessages(r io.Reader, progress io.Writer, onStream func(string)) error {
	dec := json.NewDecoder(io.LimitReader(r, 64<<20))
	for {
		var m struct {
			api.JSONMessage
			Stream string `json:"stream,omitempty"`
		}
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.Stream != "" && onStream != nil {
			onStream(m.Stream)
		}
		if progress != nil && m.Status != "" {
			if m.ID != "" {
				fmt.Fprintf(progress, "%s: %s\n", m.ID, m.Status)
			} else {
				fmt.Fprintln(progress, m.Status)
			}
		}
	}
}

// LoadImage implements Engine (POST /images/load).
func (c *Client) LoadImage(ctx context.Context, layout io.Reader) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, "/images/load", url.Values{"quiet": {"1"}}, layout, "application/x-tar")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var id string
	err = readMessages(resp.Body, nil, func(s string) {
		s = strings.TrimSpace(s)
		for _, p := range []string{"Loaded image ID: ", "Loaded image: "} {
			if v, ok := strings.CutPrefix(s, p); ok {
				id = v
			}
		}
	})
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("engine: image load named no image")
	}
	return id, nil
}

// TagImage implements Engine.
func (c *Client) TagImage(ctx context.Context, image, repo, tag string) error {
	q := url.Values{"repo": {repo}}
	if tag != "" {
		q.Set("tag", tag)
	}
	return c.json(ctx, http.MethodPost, imagePath(image)+"/tag", q, nil, nil)
}

// CreateContainer implements Engine.
func (c *Client) CreateContainer(ctx context.Context, spec RunSpec) (string, error) {
	req := api.ContainerCreateRequest{}
	req.Image = spec.Image
	req.Entrypoint = spec.Entrypoint
	req.Cmd = nil
	req.Env = spec.Env
	req.WorkingDir = spec.WorkingDir
	req.HostConfig.NetworkMode = spec.Network
	var out api.ContainerCreateResponse
	if err := c.json(ctx, http.MethodPost, "/containers/create", nil, req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// PutArchive implements Engine.
func (c *Client) PutArchive(ctx context.Context, id, dir string, tar io.Reader) error {
	q := url.Values{"path": {dir}, "noOverwriteDirNonDir": {"1"}}
	resp, err := c.do(ctx, http.MethodPut, "/containers/"+url.PathEscape(id)+"/archive", q, tar, "application/x-tar")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// StatPath implements Engine.
func (c *Client) StatPath(ctx context.Context, id, p string) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {p}}, nil, "")
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return true, nil
}

// GetArchive implements Engine.
func (c *Client) GetArchive(ctx context.Context, id, p string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {p}}, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Run implements Engine: register the wait, start, and once the step has
// exited copy its logs (the whole output, in order: an attach registered
// after the start could miss the first lines of a fast step).
func (c *Client) Run(ctx context.Context, id string, stdout, stderr io.Writer) (int, error) {
	cpath := "/containers/" + url.PathEscape(id)
	wait, err := c.do(ctx, http.MethodPost, cpath+"/wait", url.Values{"condition": {"next-exit"}}, nil, "")
	if err != nil {
		return 0, err
	}
	defer wait.Body.Close()
	startErr := c.json(ctx, http.MethodPost, cpath+"/start", nil, nil, nil)
	var res api.ContainerWaitResponse
	if err := json.NewDecoder(io.LimitReader(wait.Body, 1<<20)).Decode(&res); err != nil {
		if startErr != nil {
			return 0, startErr
		}
		return 0, fmt.Errorf("waiting for the step: %w", err)
	}
	if logs, err := c.do(ctx, http.MethodGet, cpath+"/logs", url.Values{"stdout": {"1"}, "stderr": {"1"}}, nil, ""); err == nil {
		_ = api.Demux(stdout, stderr, logs.Body)
		logs.Body.Close()
	}
	code := int(res.StatusCode)
	if startErr != nil || (res.Error != nil && res.Error.Message != "") {
		msg := ""
		if res.Error != nil {
			msg = res.Error.Message
		}
		if msg == "" && startErr != nil {
			msg = startErr.Error()
		}
		if code == 0 {
			code = ExitMissing
		}
		return code, &ExitError{Code: code, Message: msg}
	}
	return code, nil
}

// Commit implements Engine (POST /commit).
func (c *Client) Commit(ctx context.Context, spec CommitSpec) (string, error) {
	q := url.Values{"container": {spec.Container}}
	if spec.Repo != "" {
		q.Set("repo", spec.Repo)
		if spec.Tag != "" {
			q.Set("tag", spec.Tag)
		}
	}
	if spec.Comment != "" {
		q.Set("comment", spec.Comment)
	}
	if spec.Author != "" {
		q.Set("author", spec.Author)
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.json(ctx, http.MethodPost, "/commit", q, spec.Config, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("engine: commit returned no image ID")
	}
	return out.ID, nil
}

// RemoveContainer implements Engine.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	return c.json(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}}, nil, nil)
}
