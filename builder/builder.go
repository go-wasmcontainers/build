// Package builder builds an OCI image from a Containerfile by driving the
// engine: each step runs in a container made from the previous step's
// image, which is then committed — RUN executes a WebAssembly program
// there, COPY and ADD extract a computed layer into it, and metadata
// instructions (ENV, LABEL, CMD, …) commit an empty layer with the new
// configuration, so the image's history has one entry per instruction.
//
// The builder reads only the Containerfile and the build context it is
// given (in memory); it never opens a host path and fetches nothing from
// the network itself. Steps are cached by parent image, resulting
// configuration, instruction and input digest.
package builder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-wasmcontainers/build/buildcontext"
	"github.com/go-wasmcontainers/build/driver"
	"github.com/go-wasmcontainers/build/expand"
	"github.com/go-wasmcontainers/build/parser"
)

// Platform is the only platform the engine runs.
const Platform = "wasip1/wasm"

// DefaultShell runs shell-form RUN, CMD and ENTRYPOINT unless SHELL says
// otherwise.
var DefaultShell = []string{"/bin/sh", "-c"}

// Options of one build.
type Options struct {
	// Containerfile is the file's content; Name is shown in messages.
	Containerfile []byte
	Name          string
	// Context is the build context (nil: empty).
	Context *buildcontext.Context
	// Ignored says the context was filtered by an ignore file.
	Ignored   bool
	BuildArgs map[string]string
	// Target is the stage to build (default: the last).
	Target  string
	Tags    []string
	NoCache bool
	Engine  driver.Engine
	// Cache (nil: a cache for this build only).
	Cache Cache
	// Out receives the plain progress output (nil: discarded).
	Out io.Writer
	// MaxSteps bounds the instructions executed, ONBUILD triggers
	// included (default 1000).
	MaxSteps int
	// Limits bound COPY/ADD layers and --from sources.
	Limits buildcontext.Limits
}

// Result of a build.
type Result struct {
	ImageID  string
	Steps    int
	Cached   int
	Warnings []string
}

// MaxLayers keeps a build below the engine's 128-layer bound.
const MaxLayers = 127

// MaxConfigBytes bounds an image configuration (the engine reads configs
// of at most 4 MiB) and, with MaxVarBytes, what ENV and ARG can hold.
const (
	MaxConfigBytes = 1 << 20
	MaxVarBytes    = 1 << 20
)

var proxyArgs = map[string]bool{
	"HTTP_PROXY": true, "http_proxy": true, "HTTPS_PROXY": true, "https_proxy": true,
	"FTP_PROXY": true, "ftp_proxy": true, "NO_PROXY": true, "no_proxy": true,
	"ALL_PROXY": true, "all_proxy": true,
}

type stage struct {
	index    int
	name     string
	from     *parser.Node
	nodes    []*parser.Node
	platform string
}

type state struct {
	image   string // "" until FROM scratch is materialised
	cfg     driver.ImageConfig
	author  string
	args    []kv // declared in this stage, in order
	cmdSet  bool
	layers  int
	stage   *stage
	fromRef string
}

type kv struct {
	k, v string
	set  bool
}

type builder struct {
	opt      Options
	lx       expand.Lexer
	globals  []kv
	stages   []*stage
	built    map[int]*state
	building map[int]bool
	used     map[string]bool
	scratch  string
	steps    int
	cached   int
	warnings []string
	out      io.Writer
}

// Build runs a build and returns the image it made.
func Build(ctx context.Context, opt Options) (*Result, error) {
	if opt.Engine == nil {
		return nil, errors.New("builder: no engine")
	}
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.Cache == nil {
		opt.Cache = NewMemoryCache(0)
	}
	if opt.MaxSteps <= 0 {
		opt.MaxSteps = 1000
	}
	if opt.Limits.MaxFiles <= 0 {
		opt.Limits = buildcontext.DefaultLimits
	}
	if opt.Context == nil {
		opt.Context = buildcontext.New(opt.Limits)
	}
	if opt.Name == "" {
		opt.Name = "Containerfile"
	}
	res, err := parser.Parse(bytes.NewReader(opt.Containerfile), parser.DefaultLimits)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opt.Name, err)
	}
	b := &builder{opt: opt, lx: expand.Lexer{Escape: res.Escape}, built: map[int]*state{}, building: map[int]bool{}, used: map[string]bool{}, out: opt.Out}
	for _, w := range res.Warnings {
		b.warn("%s: %s", opt.Name, w)
	}
	if s, ok := res.Directives["syntax"]; ok {
		b.warn("%s: the syntax directive (%s) is ignored: this builder has one built-in frontend", opt.Name, s)
	}
	if err := b.split(res.Nodes); err != nil {
		return nil, err
	}
	target := len(b.stages) - 1
	if opt.Target != "" {
		target = -1
		for _, s := range b.stages {
			if s.name == strings.ToLower(opt.Target) {
				target = s.index
			}
		}
		if target < 0 {
			return nil, fmt.Errorf("target stage %q could not be found", opt.Target)
		}
	}
	st, err := b.buildStage(ctx, target)
	if err != nil {
		return nil, err
	}
	if st.image == "" {
		if st.image, err = b.scratchImage(ctx); err != nil {
			return nil, err
		}
	}
	var unused []string
	for k := range opt.BuildArgs {
		if !b.used[k] && !proxyArgs[k] {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		b.warn("one or more build-args %v were not consumed", unused)
	}
	for _, t := range opt.Tags {
		repo, tag, err := splitTag(t)
		if err != nil {
			return nil, err
		}
		if err := opt.Engine.TagImage(ctx, st.image, repo, tag); err != nil {
			return nil, fmt.Errorf("tagging %s: %w", t, err)
		}
	}
	fmt.Fprintf(b.out, "Successfully built %s\n", shortID(st.image))
	for _, t := range opt.Tags {
		fmt.Fprintf(b.out, "Successfully tagged %s\n", t)
	}
	return &Result{ImageID: st.image, Steps: b.steps, Cached: b.cached, Warnings: b.warnings}, nil
}

func (b *builder) warn(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	b.warnings = append(b.warnings, msg)
	fmt.Fprintf(b.out, "WARNING: %s\n", msg)
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

var stageNameRE = regexp.MustCompile(`^[a-z][a-z0-9\-_.]*$`)

// split separates global ARGs from stages.
func (b *builder) split(nodes []*parser.Node) error {
	b.globals = []kv{
		{"TARGETPLATFORM", Platform, true}, {"TARGETOS", "wasip1", true}, {"TARGETARCH", "wasm", true}, {"TARGETVARIANT", "", true},
		{"BUILDPLATFORM", runtime.GOOS + "/" + runtime.GOARCH, true}, {"BUILDOS", runtime.GOOS, true}, {"BUILDARCH", runtime.GOARCH, true}, {"BUILDVARIANT", "", true},
	}
	var cur *stage
	for _, n := range nodes {
		switch {
		case n.Cmd == "from":
			cur = &stage{index: len(b.stages), from: n}
			if len(n.Args) == 3 && strings.EqualFold(n.Args[1], "as") {
				cur.name = strings.ToLower(n.Args[2])
				if !stageNameRE.MatchString(cur.name) {
					return fmt.Errorf("line %d: invalid name for build stage: %q, name can't start with a number or contain symbols", n.StartLine, n.Args[2])
				}
				for _, s := range b.stages {
					if s.name == cur.name {
						return fmt.Errorf("line %d: duplicate stage name %q", n.StartLine, cur.name)
					}
				}
			} else if len(n.Args) != 1 {
				return fmt.Errorf("line %d: FROM requires either one or three arguments", n.StartLine)
			}
			b.stages = append(b.stages, cur)
		case cur == nil && n.Cmd == "arg":
			env := expand.Map{}
			for _, g := range b.globals {
				if g.set {
					env[g.k] = g.v
				}
			}
			defs, err := b.argDefs(n, env)
			if err != nil {
				return err
			}
			for _, d := range defs {
				b.globals = setKV(b.globals, d)
			}
		case cur == nil:
			return fmt.Errorf("line %d: %s: no build stage in current context (a Containerfile must begin with FROM, after optional ARGs)", n.StartLine, strings.ToUpper(n.Cmd))
		default:
			cur.nodes = append(cur.nodes, n)
		}
	}
	if len(b.stages) == 0 {
		return errors.New("the Containerfile has no FROM instruction")
	}
	return nil
}

func setKV(list []kv, d kv) []kv {
	for i := range list {
		if list[i].k == d.k {
			list[i] = d
			return list
		}
	}
	return append(list, d)
}

func lookupKV(list []kv, k string) (kv, bool) {
	for _, x := range list {
		if x.k == k {
			return x, true
		}
	}
	return kv{}, false
}

// argDefs reads an ARG instruction: each name, with the --build-arg value,
// else its default (expanded), else unset.
func (b *builder) argDefs(n *parser.Node, env expand.Env) ([]kv, error) {
	if len(n.Args) == 0 {
		return nil, fmt.Errorf("line %d: ARG requires at least one argument", n.StartLine)
	}
	var out []kv
	for _, w := range n.Args {
		name, def, hasDef := strings.Cut(w, "=")
		if name == "" || strings.ContainsAny(name, " \t$\"'") {
			return nil, fmt.Errorf("line %d: invalid ARG name %q", n.StartLine, name)
		}
		d := kv{k: name}
		if v, ok := b.opt.BuildArgs[name]; ok {
			d.v, d.set = v, true
			b.used[name] = true
		} else if hasDef {
			v, err := b.lx.Word(def, env)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n.StartLine, err)
			}
			d.v, d.set = v, true
		}
		out = append(out, d)
	}
	return out, nil
}

func (b *builder) globalEnv() expand.Map {
	m := expand.Map{}
	for _, g := range b.globals {
		if g.set {
			m[g.k] = g.v
		}
	}
	return m
}

// env is what a stage's words expand with: its ARGs, then its ENV (which
// wins).
func (st *state) env() expand.Map {
	m := expand.Map{}
	for _, a := range st.args {
		if a.set {
			m[a.k] = a.v
		}
	}
	for _, e := range st.cfg.Env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

func (st *state) workdir() string {
	if st.cfg.WorkingDir == "" {
		return "/"
	}
	return st.cfg.WorkingDir
}

func (b *builder) stageByName(name string) *stage {
	name = strings.ToLower(name)
	for _, s := range b.stages {
		if s.name == name {
			return s
		}
	}
	return nil
}

func (b *builder) buildStage(ctx context.Context, i int) (*state, error) {
	if st, ok := b.built[i]; ok {
		return st, nil
	}
	if b.building[i] {
		return nil, fmt.Errorf("stage %d refers to itself", i)
	}
	b.building[i] = true
	s := b.stages[i]
	label := strconv.Itoa(i)
	if s.name != "" {
		label = s.name
	}
	st, err := b.from(ctx, s, label)
	if err != nil {
		return nil, err
	}
	for k, n := range s.nodes {
		fmt.Fprintf(b.out, "[%s] STEP %d/%d: %s\n", label, k+2, len(s.nodes)+1, n.Original)
		if err := b.dispatch(ctx, st, n, false); err != nil {
			return nil, fmt.Errorf("%s line %d: %s: %w", b.opt.Name, n.StartLine, strings.ToUpper(n.Cmd), err)
		}
	}
	b.built[i] = st
	return st, nil
}

func (b *builder) count() error {
	b.steps++
	if b.steps > b.opt.MaxSteps {
		return fmt.Errorf("more than %d steps", b.opt.MaxSteps)
	}
	return nil
}

// flags parses --name=value words against the allowed names.
func flags(n *parser.Node, allowed ...string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range n.Flags {
		name, val, hasVal := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		ok := false
		for _, a := range allowed {
			if a == name {
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("unknown flag: --%s", name)
		}
		if !hasVal {
			val = "true"
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--%s given twice", name)
		}
		out[name] = val
	}
	return out, nil
}

func (b *builder) from(ctx context.Context, s *stage, label string) (*state, error) {
	n := s.from
	fmt.Fprintf(b.out, "[%s] STEP 1/%d: %s\n", label, len(s.nodes)+1, n.Original)
	if err := b.count(); err != nil {
		return nil, err
	}
	fl, err := flags(n, "platform")
	if err != nil {
		return nil, fmt.Errorf("line %d: FROM: %w", n.StartLine, err)
	}
	genv := b.globalEnv()
	if p, ok := fl["platform"]; ok {
		pv, err := b.lx.Word(p, genv)
		if err != nil {
			return nil, err
		}
		if pv != Platform {
			return nil, fmt.Errorf("line %d: FROM --platform=%s: this engine runs only %s images", n.StartLine, pv, Platform)
		}
	}
	ref, err := b.lx.Word(n.Args[0], genv)
	if err != nil {
		return nil, fmt.Errorf("line %d: FROM: %w", n.StartLine, err)
	}
	if ref == "" {
		return nil, fmt.Errorf("line %d: FROM: base name (%s) should not be blank", n.StartLine, n.Args[0])
	}
	st := &state{stage: s, fromRef: ref}
	if base := b.stageByName(ref); base != nil && base.index < s.index {
		parent, err := b.buildStage(ctx, base.index)
		if err != nil {
			return nil, err
		}
		st.image, st.cfg, st.author, st.layers = parent.image, cloneConfig(parent.cfg), parent.author, parent.layers
		st.args = append([]kv(nil), parent.args...)
		return st, nil
	}
	if strings.EqualFold(ref, "scratch") {
		return st, nil
	}
	img, err := b.resolveImage(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("line %d: FROM %s: %w", n.StartLine, ref, err)
	}
	st.image, st.cfg, st.author, st.layers = img.ID, cloneConfig(img.Config), img.Author, img.Layers
	if triggers := st.cfg.OnBuild; len(triggers) > 0 {
		st.cfg.OnBuild = nil // triggers are not inherited by grandchildren
		for _, t := range triggers {
			fmt.Fprintf(b.out, "[%s] ONBUILD %s\n", label, t)
			tr, err := parser.Parse(strings.NewReader(t), parser.DefaultLimits)
			if err != nil || len(tr.Nodes) != 1 {
				return nil, fmt.Errorf("ONBUILD trigger %q of %s: %v", t, ref, err)
			}
			if c := tr.Nodes[0].Cmd; c == "onbuild" || c == "from" || c == "maintainer" {
				return nil, fmt.Errorf("ONBUILD trigger %q of %s is not allowed", t, ref)
			}
			if err := b.dispatch(ctx, st, tr.Nodes[0], true); err != nil {
				return nil, fmt.Errorf("ONBUILD trigger %q of %s: %w", t, ref, err)
			}
		}
	}
	return st, nil
}

// resolveImage finds an image in the engine, pulling it when missing, and
// checks its platform.
func (b *builder) resolveImage(ctx context.Context, ref string) (*driver.Image, error) {
	img, err := b.opt.Engine.InspectImage(ctx, ref)
	if errors.Is(err, driver.ErrNotFound) {
		if err := b.opt.Engine.PullImage(ctx, ref, b.out); err != nil {
			return nil, err
		}
		img, err = b.opt.Engine.InspectImage(ctx, ref)
	}
	if err != nil {
		return nil, err
	}
	if (img.OS != "" && img.OS != "wasip1") || (img.Architecture != "" && img.Architecture != "wasm") {
		return nil, fmt.Errorf("image is %s/%s: this engine runs only %s images", img.OS, img.Architecture, Platform)
	}
	return img, nil
}

func cloneConfig(c driver.ImageConfig) driver.ImageConfig {
	b, _ := json.Marshal(c)
	var out driver.ImageConfig
	_ = json.Unmarshal(b, &out)
	return out
}

func (b *builder) scratchImage(ctx context.Context) (string, error) {
	if b.scratch != "" {
		return b.scratch, nil
	}
	layout, cfgDigest := scratchLayout()
	if id, ok := b.opt.Cache.Get(sum("scratch", cfgDigest)); ok && !b.opt.NoCache {
		if _, err := b.opt.Engine.InspectImage(ctx, id); err == nil {
			b.scratch = id
			return id, nil
		}
	}
	id, err := b.opt.Engine.LoadImage(ctx, bytes.NewReader(layout))
	if err != nil {
		return "", fmt.Errorf("loading the empty base image: %w", err)
	}
	b.opt.Cache.Put(sum("scratch", cfgDigest), id)
	b.scratch = id
	return id, nil
}

func sum(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s\n", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// step is one committed instruction.
type step struct {
	desc    string   // for "#(nop) desc" (metadata, COPY)
	argv    []string // RUN: the command; nil for nop steps
	env     []string // RUN only
	keyEnv  []string // the part of env that is in the cache key
	network string
	layer   *layer
	key     string // extra cache material
	shell   bool   // RUN in shell form (for the missing-shell message)
}

func (b *builder) commit(ctx context.Context, st *state, s step) error {
	e := b.opt.Engine
	if st.image == "" {
		id, err := b.scratchImage(ctx)
		if err != nil {
			return err
		}
		st.image = id
	}
	cfgJSON, _ := json.Marshal(st.cfg)
	if len(cfgJSON) > MaxConfigBytes {
		return fmt.Errorf("the image configuration would exceed %d bytes", MaxConfigBytes)
	}
	argvJSON, _ := json.Marshal(s.argv)
	envJSON, _ := json.Marshal(s.keyEnv)
	ldig := ""
	if s.layer != nil {
		ldig = s.layer.digest
	}
	key := sum("v1", st.image, string(cfgJSON), st.author, s.desc, string(argvJSON), string(envJSON), s.network, ldig, s.key)
	if !b.opt.NoCache {
		if id, ok := b.opt.Cache.Get(key); ok {
			if img, err := e.InspectImage(ctx, id); err == nil {
				fmt.Fprintf(b.out, "--> Using cache %s\n", shortID(id))
				st.image = id
				st.layers = max(st.layers, img.Layers)
				b.cached++
				return nil
			}
		}
	}
	if s.layer != nil || s.argv != nil {
		if st.layers+1 > MaxLayers {
			return fmt.Errorf("the image would have more than %d layers", MaxLayers)
		}
	}
	entry := s.argv
	if entry == nil {
		entry = []string{"/bin/sh", "-c", "#(nop) " + s.desc}
	}
	cid, err := e.CreateContainer(ctx, driver.RunSpec{Image: st.image, Entrypoint: entry, Env: s.env, WorkingDir: st.cfg.WorkingDir, User: st.cfg.User, Network: s.network})
	if err != nil {
		return err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = e.RemoveContainer(rctx, cid)
	}()
	if s.layer != nil {
		if err := e.PutArchive(ctx, cid, "/", bytes.NewReader(s.layer.tar)); err != nil {
			return err
		}
	}
	if s.argv != nil {
		code, err := e.Run(ctx, cid, b.out, b.out)
		var xe *driver.ExitError
		if errors.As(err, &xe) && (code == driver.ExitMissing || code == driver.ExitNotExecutable) {
			if s.shell {
				return fmt.Errorf("RUN in shell form needs the shell %q, which this image cannot run (%s): the image has no shell, or the engine cannot spawn processes yet. Use the exec form, RUN [\"program\", \"arg\"], or a base image that provides %s", s.argv[0], xe.Message, s.argv[0])
			}
			if code == driver.ExitMissing {
				return fmt.Errorf("%q was not found in the image: %s", s.argv[0], xe.Message)
			}
			return fmt.Errorf("%q is not a WebAssembly module the engine can run: %s", s.argv[0], xe.Message)
		}
		if err != nil && xe == nil {
			return err
		}
		if code != 0 {
			msg := ""
			if xe != nil && xe.Message != "" {
				msg = " (" + xe.Message + ")"
			}
			return fmt.Errorf("process %q did not complete successfully: exit code: %d%s", strings.Join(s.argv, " "), code, msg)
		}
	}
	id, err := e.Commit(ctx, driver.CommitSpec{Container: cid, Config: st.cfg, Author: st.author})
	if err != nil {
		return err
	}
	b.opt.Cache.Put(key, id)
	if s.layer != nil || s.argv != nil {
		st.layers++
	}
	st.image = id
	fmt.Fprintf(b.out, "--> %s\n", shortID(id))
	return nil
}

func (b *builder) dispatch(ctx context.Context, st *state, n *parser.Node, trigger bool) error {
	if err := b.count(); err != nil {
		return err
	}
	env := st.env()
	size := 0
	for k, v := range env {
		size += len(k) + len(v)
	}
	if size > MaxVarBytes {
		return fmt.Errorf("ARG and ENV values exceed %d bytes", MaxVarBytes)
	}
	word := func(s string) (string, error) { return b.lx.Word(s, env) }
	nop := func(desc string) error { return b.commit(ctx, st, step{desc: desc}) }
	switch n.Cmd {
	case "arg":
		parentEnv := env
		defs, err := b.argDefs(n, parentEnv)
		if err != nil {
			return err
		}
		for _, d := range defs {
			if !d.set {
				if g, ok := lookupKV(b.globals, d.k); ok && g.set {
					d.v, d.set = g.v, true
				}
			}
			st.args = setKV(st.args, d)
		}
		return nil
	case "env", "label":
		if len(n.Args) == 0 || len(n.Args)%3 != 0 {
			return fmt.Errorf("%s requires at least one argument", strings.ToUpper(n.Cmd))
		}
		var desc []string
		for i := 0; i < len(n.Args); i += 3 {
			k, err := word(n.Args[i])
			if err != nil {
				return err
			}
			v, err := word(n.Args[i+1])
			if err != nil {
				return err
			}
			if k == "" {
				return fmt.Errorf("%s names must not be empty", strings.ToUpper(n.Cmd))
			}
			if n.Cmd == "env" {
				if strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
					return fmt.Errorf("invalid ENV %q", k)
				}
				st.cfg.Env = setEnv(st.cfg.Env, k, v)
			} else {
				if st.cfg.Labels == nil {
					st.cfg.Labels = map[string]string{}
				}
				st.cfg.Labels[k] = v
			}
			desc = append(desc, k+"="+v)
		}
		return nop(strings.ToUpper(n.Cmd) + " " + strings.Join(desc, " "))
	case "maintainer":
		if len(n.Args) == 0 {
			return errors.New("MAINTAINER requires an argument")
		}
		st.author = n.Args[0]
		return nop("MAINTAINER " + st.author)
	case "expose":
		var ports []string
		for _, a := range n.Args {
			ws, err := b.lx.Words(a, env)
			if err != nil {
				return err
			}
			ports = append(ports, ws...)
		}
		if len(ports) == 0 {
			return errors.New("EXPOSE requires at least one argument")
		}
		if st.cfg.ExposedPorts == nil {
			st.cfg.ExposedPorts = map[string]struct{}{}
		}
		for _, p := range ports {
			norm, err := normalizePort(p)
			if err != nil {
				return err
			}
			st.cfg.ExposedPorts[norm] = struct{}{}
		}
		return nop("EXPOSE " + strings.Join(ports, " "))
	case "volume":
		if len(n.Args) == 0 {
			return errors.New("VOLUME requires at least one argument")
		}
		if st.cfg.Volumes == nil {
			st.cfg.Volumes = map[string]struct{}{}
		}
		var vs []string
		for _, a := range n.Args {
			v, err := word(a)
			if err != nil {
				return err
			}
			v = strings.TrimSpace(v)
			if v == "" {
				return errors.New("VOLUME specified can not be an empty string")
			}
			st.cfg.Volumes[v] = struct{}{}
			vs = append(vs, v)
		}
		return nop("VOLUME " + fmt.Sprint(vs))
	case "user":
		if len(n.Args) == 0 {
			return errors.New("USER requires an argument")
		}
		u, err := word(n.Args[0])
		if err != nil {
			return err
		}
		st.cfg.User = u
		return nop("USER " + u)
	case "stopsignal":
		if len(n.Args) == 0 {
			return errors.New("STOPSIGNAL requires an argument")
		}
		sig, err := word(n.Args[0])
		if err != nil {
			return err
		}
		if !validSignal(sig) {
			return fmt.Errorf("invalid signal: %s", sig)
		}
		st.cfg.StopSignal = sig
		b.warn("STOPSIGNAL %s is recorded in the image, but a WebAssembly guest receives no signals: stopping it kills it", sig)
		return nop("STOPSIGNAL " + sig)
	case "healthcheck":
		hc, err := healthcheck(n)
		if err != nil {
			return err
		}
		st.cfg.Healthcheck = hc
		b.warn("HEALTHCHECK is recorded in the image, but the engine does not run health checks yet")
		return nop(strings.TrimSpace(n.Original))
	case "shell":
		if !n.JSON || len(n.Args) == 0 {
			return errors.New("SHELL requires the arguments to be in JSON form, and at least one element")
		}
		st.cfg.Shell = append([]string(nil), n.Args...)
		return nop("SHELL " + jsonString(n.Args))
	case "cmd", "entrypoint":
		argv := st.command(n)
		if n.Cmd == "cmd" {
			st.cfg.Cmd = argv
			st.cmdSet = true
			return nop("CMD " + jsonString(argv))
		}
		st.cfg.Entrypoint = argv
		if !st.cmdSet {
			st.cfg.Cmd = nil // an inherited CMD does not survive a new ENTRYPOINT
		}
		return nop("ENTRYPOINT " + jsonString(argv))
	case "onbuild":
		if n.Sub == nil {
			return errors.New("ONBUILD requires at least one argument")
		}
		if trigger {
			return errors.New("ONBUILD is not allowed in a trigger")
		}
		t := strings.TrimSpace(n.Sub.Original)
		st.cfg.OnBuild = append(st.cfg.OnBuild, t)
		return nop("ONBUILD " + t)
	case "workdir":
		if len(n.Args) == 0 {
			return errors.New("WORKDIR requires exactly one argument")
		}
		w, err := word(n.Args[0])
		if err != nil {
			return err
		}
		if w == "" {
			return errors.New("WORKDIR cannot be empty")
		}
		dir := path.Clean(path.Join("/", st.workdir(), w))
		if path.IsAbs(w) {
			dir = path.Clean(w)
		}
		st.cfg.WorkingDir = dir
		s := step{desc: "WORKDIR " + dir}
		if dir != "/" && !b.exists(ctx, st, dir) {
			op := &copyOp{}
			l, err := op.writeTar(map[string]*buildcontext.Entry{strings.TrimPrefix(dir, "/"): {Mode: fs.ModeDir | 0o755, ModTime: time.Unix(0, 0)}})
			if err != nil {
				return err
			}
			s.layer = l
		}
		return b.commit(ctx, st, s)
	case "copy", "add":
		return b.copy(ctx, st, n, env)
	case "run":
		return b.run(ctx, st, n, env)
	case "from":
		return errors.New("FROM is not allowed here")
	}
	return fmt.Errorf("unknown instruction %s", strings.ToUpper(n.Cmd))
}

// exists asks whether dir is present in the current image (a HEAD on the
// container archive endpoint; nothing is transferred).
func (b *builder) exists(ctx context.Context, st *state, dir string) bool {
	if st.image == "" {
		return false
	}
	cid, err := b.opt.Engine.CreateContainer(ctx, driver.RunSpec{Image: st.image, Entrypoint: []string{"/bin/sh", "-c", "#(nop) stat " + dir}})
	if err != nil {
		return false
	}
	defer b.opt.Engine.RemoveContainer(context.WithoutCancel(ctx), cid)
	ok, err := b.opt.Engine.StatPath(ctx, cid, dir)
	return err == nil && ok
}

func setEnv(env []string, k, v string) []string {
	for i, e := range env {
		if ek, _, _ := strings.Cut(e, "="); ek == k {
			env[i] = k + "=" + v
			return env
		}
	}
	return append(env, k+"="+v)
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// command is a CMD/ENTRYPOINT/RUN argv: the exec form as written, the
// shell form through SHELL (default /bin/sh -c).
func (st *state) command(n *parser.Node) []string {
	if n.JSON {
		return append([]string{}, n.Args...)
	}
	if len(n.Args) == 0 {
		return nil
	}
	sh := st.cfg.Shell
	if len(sh) == 0 {
		sh = DefaultShell
	}
	return append(append([]string{}, sh...), n.Args[0])
}

func normalizePort(p string) (string, error) {
	port, proto, ok := strings.Cut(p, "/")
	if !ok {
		proto = "tcp"
	}
	proto = strings.ToLower(proto)
	if proto != "tcp" && proto != "udp" && proto != "sctp" {
		return "", fmt.Errorf("invalid proto: %s", proto)
	}
	lo, hi, rng := strings.Cut(port, "-")
	for _, x := range []string{lo, hi} {
		if !rng && x == hi {
			continue
		}
		n, err := strconv.Atoi(x)
		if err != nil || n < 0 || n > 65535 {
			return "", fmt.Errorf("invalid containerPort: %s", port)
		}
	}
	return port + "/" + proto, nil
}

func validSignal(s string) bool {
	if n, err := strconv.Atoi(s); err == nil {
		return n > 0 && n < 128
	}
	s = strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '+' && r != '-' {
			return false
		}
	}
	return true
}

func healthcheck(n *parser.Node) (*driver.HealthConfig, error) {
	if len(n.Args) == 0 {
		return nil, errors.New("HEALTHCHECK requires an argument")
	}
	switch strings.ToUpper(n.Args[0]) {
	case "NONE":
		if len(n.Args) != 1 || len(n.Flags) != 0 {
			return nil, errors.New("HEALTHCHECK NONE takes no arguments or options")
		}
		return &driver.HealthConfig{Test: []string{"NONE"}}, nil
	case "CMD":
	default:
		return nil, fmt.Errorf("unknown type %q in HEALTHCHECK (try CMD)", n.Args[0])
	}
	if len(n.Args) < 2 {
		return nil, errors.New("missing command after HEALTHCHECK CMD")
	}
	fl, err := flags(n, "interval", "timeout", "start-period", "start-interval", "retries")
	if err != nil {
		return nil, err
	}
	hc := &driver.HealthConfig{}
	if n.JSON {
		hc.Test = append([]string{"CMD"}, n.Args[1:]...)
	} else {
		hc.Test = []string{"CMD-SHELL", n.Args[1]}
	}
	dur := func(name string) (int64, error) {
		v, ok := fl[name]
		if !ok {
			return 0, nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || (d != 0 && d < time.Millisecond) {
			return 0, fmt.Errorf("--%s=%s: must be a duration of at least 1ms (or 0)", name, v)
		}
		return int64(d), nil
	}
	if hc.Interval, err = dur("interval"); err != nil {
		return nil, err
	}
	if hc.Timeout, err = dur("timeout"); err != nil {
		return nil, err
	}
	if hc.StartPeriod, err = dur("start-period"); err != nil {
		return nil, err
	}
	if hc.StartInterval, err = dur("start-interval"); err != nil {
		return nil, err
	}
	if v, ok := fl["retries"]; ok {
		r, err := strconv.Atoi(v)
		if err != nil || r < 0 {
			return nil, fmt.Errorf("--retries=%s: must be a non-negative integer", v)
		}
		hc.Retries = r
	}
	return hc, nil
}

func (b *builder) run(ctx context.Context, st *state, n *parser.Node, env expand.Map) error {
	fl, err := flags(n, "network", "mount", "security", "device")
	if err != nil {
		return err
	}
	for _, f := range []string{"mount", "security", "device"} {
		if _, ok := fl[f]; ok {
			return fmt.Errorf("RUN --%s is not supported yet", f)
		}
	}
	network := ""
	switch fl["network"] {
	case "", "default":
	case "none":
		network = "none"
	case "host":
		return errors.New("RUN --network=host: a WebAssembly engine has no host network")
	default:
		return fmt.Errorf("RUN --network=%s: unknown network mode (default or none)", fl["network"])
	}
	argv := st.command(n)
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("RUN requires a command")
	}
	// The step's environment: the stage's ARGs that ENV does not override,
	// plus proxy build-args (not part of the cache key).
	var argEnv, proxyEnv []string
	cfgEnv := map[string]bool{}
	for _, e := range st.cfg.Env {
		k, _, _ := strings.Cut(e, "=")
		cfgEnv[k] = true
	}
	for _, a := range st.args {
		if a.set && !cfgEnv[a.k] {
			argEnv = append(argEnv, a.k+"="+a.v)
		}
	}
	for k, v := range b.opt.BuildArgs {
		if proxyArgs[k] && !cfgEnv[k] {
			if _, declared := lookupKV(st.args, k); !declared {
				proxyEnv = append(proxyEnv, k+"="+v)
			}
		}
	}
	sort.Strings(proxyEnv)
	// Proxy values stay out of the cache key (the reference: predefined
	// ARGs are exempt from caching unless declared).
	s := step{argv: argv, env: append(append([]string{}, argEnv...), proxyEnv...), keyEnv: argEnv, network: network, shell: !n.JSON}
	return b.commit(ctx, st, s)
}

func (b *builder) copy(ctx context.Context, st *state, n *parser.Node, env expand.Map) error {
	what := strings.ToUpper(n.Cmd)
	allowed := []string{"from", "chown", "chmod", "link", "parents", "exclude"}
	if what == "ADD" {
		allowed = []string{"chown", "chmod", "link", "unpack", "checksum", "keep-git-dir", "exclude"}
	}
	fl, err := flags(n, allowed...)
	if err != nil {
		return err
	}
	for _, f := range []string{"parents", "exclude", "checksum", "keep-git-dir"} {
		if _, ok := fl[f]; ok {
			return fmt.Errorf("%s --%s is not supported yet", what, f)
		}
	}
	if _, ok := fl["link"]; ok {
		b.warn("%s --link is accepted and has no effect here: every layer is already independent of the files below it", what)
	}
	if len(n.Args) < 2 {
		return fmt.Errorf("%s requires at least two arguments, but got %d", what, len(n.Args))
	}
	var args []string
	// Both forms of COPY expand variables: they are paths, not a command.
	for _, a := range n.Args {
		v, err := b.lx.Word(a, env)
		if err != nil {
			return err
		}
		args = append(args, v)
	}
	srcs, dest := args[:len(args)-1], args[len(args)-1]
	for _, s := range srcs {
		if what == "ADD" && (strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "git@")) {
			return fmt.Errorf("ADD %s: adding from a URL or a Git repository is not supported yet (the builder fetches nothing from the network): put the file in the build context", s)
		}
	}
	chownV, err := b.lx.Word(fl["chown"], env)
	if err != nil {
		return err
	}
	chown, err := parseChown(chownV)
	if err != nil {
		return err
	}
	chmodV, err := b.lx.Word(fl["chmod"], env)
	if err != nil {
		return err
	}
	chmod, err := parseChmod(chmodV)
	if err != nil {
		return err
	}
	op := &copyOp{srcs: srcs, dest: resolveDest(dest, st.workdir()), chown: chown, chmod: chmod, what: what, src: b.opt.Context, ignored: b.opt.Ignored, fromContext: true}
	if what == "ADD" {
		op.unpack = fl["unpack"] != "false"
	}
	fromDesc := ""
	if from, ok := fl["from"]; ok {
		fv, err := b.lx.Word(from, env)
		if err != nil {
			return err
		}
		src, desc, err := b.fromSource(ctx, st, fv, srcs)
		if err != nil {
			return fmt.Errorf("COPY --from=%s: %w", fv, err)
		}
		op.src, op.fromContext, op.ignored, fromDesc = src, false, false, desc
	}
	l, err := op.build(b.opt.Limits)
	if err != nil {
		return err
	}
	desc := fmt.Sprintf("%s %sfile:%s in %s", what, fromDesc, l.digest, op.dest)
	return b.commit(ctx, st, step{desc: desc, layer: l})
}

// fromSource fetches the sources of COPY --from out of a stage or an
// image, through the engine's archive endpoint, into a context.
func (b *builder) fromSource(ctx context.Context, st *state, from string, srcs []string) (*buildcontext.Context, string, error) {
	var image string
	if s := b.stageByName(from); s != nil && s.index < st.stage.index {
		ps, err := b.buildStage(ctx, s.index)
		if err != nil {
			return nil, "", err
		}
		image = ps.image
	} else if i, err := strconv.Atoi(from); err == nil {
		if i < 0 || i >= st.stage.index {
			return nil, "", fmt.Errorf("stage %d is not a previous stage", i)
		}
		ps, err := b.buildStage(ctx, i)
		if err != nil {
			return nil, "", err
		}
		image = ps.image
	} else {
		img, err := b.resolveImage(ctx, from)
		if err != nil {
			return nil, "", err
		}
		image = img.ID
	}
	if image == "" {
		var err error
		if image, err = b.scratchImage(ctx); err != nil {
			return nil, "", err
		}
	}
	e := b.opt.Engine
	cid, err := e.CreateContainer(ctx, driver.RunSpec{Image: image, Entrypoint: []string{"/bin/sh", "-c", "#(nop) COPY --from source"}})
	if err != nil {
		return nil, "", err
	}
	defer e.RemoveContainer(context.WithoutCancel(ctx), cid)
	src := buildcontext.New(b.opt.Limits)
	seen := map[string]bool{}
	for _, s := range srcs {
		p := path.Clean("/" + s)
		if buildcontext.HasMeta(s) {
			// Fetch the deepest directory with no wildcard in it.
			parts := strings.Split(p, "/")
			k := 0
			for k < len(parts) && !buildcontext.HasMeta(parts[k]) {
				k++
			}
			p = path.Clean("/" + strings.Join(parts[:k], "/"))
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		rc, err := e.GetArchive(ctx, cid, p)
		if errors.Is(err, driver.ErrNotFound) {
			continue // reported by the copy as "file not found"
		}
		if err != nil {
			return nil, "", err
		}
		prefix := strings.TrimPrefix(path.Dir(p), "/")
		if p == "/" {
			prefix = ""
		}
		err = src.ReadTar(rc, prefix)
		rc.Close()
		if err != nil {
			return nil, "", err
		}
	}
	return src, "from:" + shortID(image) + " ", nil
}

// splitTag reads NAME[:TAG] (default tag latest).
func splitTag(t string) (string, string, error) {
	if t == "" || len(t) > 255 {
		return "", "", fmt.Errorf("invalid tag %q", t)
	}
	repo, tag := t, "latest"
	if i := strings.LastIndexByte(t, ':'); i > strings.LastIndexByte(t, '/') {
		repo, tag = t[:i], t[i+1:]
	}
	if repo == "" || tag == "" || strings.ToLower(repo) != repo || strings.Contains(t, "@") {
		return "", "", fmt.Errorf("invalid tag %q", t)
	}
	for _, r := range repo {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/:", r)) {
			return "", "", fmt.Errorf("invalid reference format: %q", t)
		}
	}
	for _, r := range tag {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return "", "", fmt.Errorf("invalid tag %q", tag)
		}
	}
	return repo, tag, nil
}
