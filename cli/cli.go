// Package cli is the `build` command: `wctr build`, and the standalone
// `wctr-build` until the engine's wctr wires it in.
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/build/buildcontext"
	"github.com/go-wasmcontainers/build/builder"
	"github.com/go-wasmcontainers/build/driver"
)

// DefaultHost is the engine's socket when neither -H nor WCTR_HOST says.
const DefaultHost = "unix:///var/run/wctr.sock"

// Options wire the command into a host program.
type Options struct {
	// Engine returns the engine to build with; cmd is the build command
	// (its flags, and its parents', are parsed). Nil: dial the -H/--host
	// flag of the command or a parent, else $WCTR_HOST, else DefaultHost.
	Engine func(cmd *cobra.Command) (driver.Engine, error)
	// Cache is the step cache. Nil: a file under the user cache directory,
	// or a cache for this build only where there is none (a browser).
	Cache builder.Cache
	// Stdin is read for "-" (default os.Stdin).
	Stdin io.Reader
	// Limits bound the context (default buildcontext.DefaultLimits).
	Limits buildcontext.Limits
}

// Command is `build` with the default wiring.
func Command() *cobra.Command { return NewCommand(Options{}) }

// NewCommand is `build` wired by opt.
func NewCommand(opt Options) *cobra.Command {
	var (
		tags      []string
		file      string
		buildArgs []string
		target    string
		noCache   bool
		progress  string
		quiet     bool
	)
	cmd := &cobra.Command{
		Use:   "build [OPTIONS] PATH | -",
		Short: "Build an image from a Containerfile (or Dockerfile)",
		Long: `Build an image from a Containerfile or a Dockerfile (same syntax) and the
files of PATH, the build context. The file is PATH/Containerfile, else
PATH/Dockerfile, unless -f names one. The context is sent without what its
.containerignore (else .dockerignore) excludes. With "-", the context is a
tar read from standard input, holding the Containerfile.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if progress != "plain" && progress != "auto" {
				return fmt.Errorf("--progress=%s: only plain (or auto) is supported", progress)
			}
			bargs := map[string]string{}
			for _, a := range buildArgs {
				k, v, ok := strings.Cut(a, "=")
				if k == "" {
					return fmt.Errorf("--build-arg %q: want KEY=VALUE", a)
				}
				if !ok {
					// KEY alone takes the value from the environment, if set.
					ev, set := os.LookupEnv(k)
					if !set {
						continue
					}
					v = ev
				}
				bargs[k] = v
			}
			lim := opt.Limits
			if lim.MaxFiles <= 0 {
				lim = buildcontext.DefaultLimits
			}
			stdin := opt.Stdin
			if stdin == nil {
				stdin = cmd.InOrStdin()
			}
			bc, df, name, ignored, err := loadContext(args[0], file, stdin, lim)
			if err != nil {
				return err
			}
			eng, err := engineFor(cmd, opt)
			if err != nil {
				return err
			}
			cache := opt.Cache
			var fc *builder.FileCache
			if cache == nil {
				if p, err := builder.DefaultCachePath(); err == nil {
					fc = builder.OpenFileCache(p, 0)
					cache = fc
				}
			}
			out := cmd.OutOrStdout()
			if quiet {
				out = io.Discard
			}
			res, err := builder.Build(cmd.Context(), builder.Options{
				Containerfile: df, Name: name, Context: bc, Ignored: ignored,
				BuildArgs: bargs, Target: target, Tags: tags, NoCache: noCache,
				Engine: eng, Cache: cache, Out: out, Limits: lim,
			})
			if fc != nil {
				if err := fc.Save(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: the build cache was not saved: %v\n", err)
				}
			}
			if err != nil {
				return err
			}
			if quiet {
				fmt.Fprintln(cmd.OutOrStdout(), res.ImageID)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&tags, "tag", "t", nil, `name and optionally a tag ("name:tag")`)
	f.StringVarP(&file, "file", "f", "", `the Containerfile ("-": standard input; default PATH/Containerfile, else PATH/Dockerfile)`)
	f.StringArrayVar(&buildArgs, "build-arg", nil, "set a build-time variable (KEY=VALUE, or KEY to take it from the environment)")
	f.StringVar(&target, "target", "", "build this stage")
	f.BoolVar(&noCache, "no-cache", false, "do not use the cache")
	f.StringVar(&progress, "progress", "plain", "progress output: plain")
	f.BoolVarP(&quiet, "quiet", "q", false, "print only the image ID")
	return cmd
}

func engineFor(cmd *cobra.Command, opt Options) (driver.Engine, error) {
	if opt.Engine != nil {
		return opt.Engine(cmd)
	}
	host := ""
	if fl := cmd.Flags().Lookup("host"); fl != nil {
		host = fl.Value.String()
	}
	if host == "" {
		host = os.Getenv("WCTR_HOST")
	}
	if host == "" {
		host = DefaultHost
	}
	return driver.Dial(host)
}

// maxContainerfile bounds a Containerfile read from a file or stdin.
const maxContainerfile = 1 << 20

func readLimited(r io.Reader, what string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxContainerfile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxContainerfile {
		return nil, fmt.Errorf("%s is larger than %d bytes", what, maxContainerfile)
	}
	return b, nil
}

// loadContext reads the context and the Containerfile.
func loadContext(arg, file string, stdin io.Reader, lim buildcontext.Limits) (*buildcontext.Context, []byte, string, bool, error) {
	if arg == "-" {
		if file == "-" {
			return nil, nil, "", false, errors.New("the context and the Containerfile cannot both come from standard input")
		}
		bc, err := buildcontext.FromTar(stdin, lim, "")
		if err != nil {
			return nil, nil, "", false, err
		}
		m, err := bc.IgnoreMatcher()
		if err != nil {
			return nil, nil, "", false, err
		}
		name, df := "", []byte(nil)
		cands := []string{"Containerfile", "Dockerfile"}
		if file != "" {
			cands = []string{file}
		}
		for _, n := range cands {
			if e, ok := bc.Get(strings.TrimPrefix(filepathToSlash(n), "/")); ok && e.Mode.IsRegular() {
				name, df = n, e.Data
				break
			}
		}
		if name == "" {
			return nil, nil, "", false, fmt.Errorf("no %s in the context", strings.Join(cands, " or "))
		}
		bc.Exclude(m)
		return bc, df, name, m != nil, nil
	}
	st, err := os.Stat(arg)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("unable to prepare context: %w", err)
	}
	if !st.IsDir() {
		return nil, nil, "", false, fmt.Errorf("unable to prepare context: %s is not a directory", arg)
	}
	var df []byte
	name := file
	switch {
	case file == "-":
		if df, err = readLimited(stdin, "the Containerfile"); err != nil {
			return nil, nil, "", false, err
		}
	case file != "":
		fh, err := os.Open(file)
		if err != nil {
			return nil, nil, "", false, err
		}
		df, err = readLimited(fh, file)
		fh.Close()
		if err != nil {
			return nil, nil, "", false, err
		}
	default:
		for _, n := range []string{"Containerfile", "Dockerfile"} {
			p := filepath.Join(arg, n)
			fh, err := os.Open(p)
			if err != nil {
				continue
			}
			df, err = readLimited(fh, p)
			fh.Close()
			if err != nil {
				return nil, nil, "", false, err
			}
			name = n
			break
		}
		if name == "" {
			return nil, nil, "", false, fmt.Errorf("no Containerfile or Dockerfile in %s (use -f)", arg)
		}
	}
	var buf bytes.Buffer
	if _, err := buildcontext.Pack(arg, &buf, lim); err != nil {
		return nil, nil, "", false, fmt.Errorf("sending the build context: %w", err)
	}
	root, err := os.OpenRoot(arg)
	if err != nil {
		return nil, nil, "", false, err
	}
	m, _, _ := buildcontext.ReadIgnore(root.FS())
	root.Close()
	bc, err := buildcontext.FromTar(&buf, lim, "")
	if err != nil {
		return nil, nil, "", false, err
	}
	return bc, df, name, m != nil, nil
}

func filepathToSlash(p string) string { return filepath.ToSlash(p) }
