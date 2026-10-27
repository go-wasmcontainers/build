# build

[![Docs](https://img.shields.io/badge/docs-hugo%20%2B%20hextra-0079A8)](https://go-wasmcontainers.github.io/docs/)
[![License: BSD-3-Clause](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![CI](https://github.com/go-wasmcontainers/build/actions/workflows/ci.yml/badge.svg)](https://github.com/go-wasmcontainers/build/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-wasmcontainers/build.svg)](https://pkg.go.dev/github.com/go-wasmcontainers/build)
![status](https://img.shields.io/badge/status-M0%20parser-yellow)

**`wctr build`: build OCI images from a Containerfile (or a Dockerfile,
same syntax) for the [go-wasmcontainers engine](https://github.com/go-wasmcontainers/engine)**,
whose containers are WASI preview 1 (`wasip1/wasm`) modules. Pure Go
(`CGO_ENABLED=0`), standard library only for the front end, so it also runs
in the browser [playground](https://github.com/go-wasmcontainers/playground).
The builder is a separate module that drives the engine through its API —
run each step in a container from the previous image, then commit — the way
BuildKit sits beside a container daemon.

> **Status: M0, the front end.** The Containerfile parser, the variable
> expander and the `.containerignore` matcher are done and tested against
> the reference implementation's own golden files. The build itself
> (`wctr build`) is in progress.

## Packages

| Package | What it does |
|---|---|
| `parser` | Containerfile/Dockerfile → instructions: parser directives (`syntax`, `escape`, `check`), comments, line continuation, exec (JSON) and shell forms, flags, ENV/LABEL pairs, ARG, HEALTHCHECK, ONBUILD. BuildKit's parser golden corpus passes unchanged. Here-documents are refused with a precise error (not yet supported). |
| `expand` | Variable substitution as the Dockerfile reference specifies: quotes, escape token, `$v`, `${v}`, `${v:-w}` `${v-w}` `${v:+w}` `${v+w}` `${v?w}` `${v#p}` `${v##p}` `${v%p}` `${v%%p}` `${v/p/r}` `${v//p/r}`; word splitting after substitution. Nothing is executed. |
| `ignore` | `.containerignore` / `.dockerignore`: `filepath.Match` syntax, `**`, `!` exceptions (last match wins), parent-directory matches. |

## Security

The Containerfile and the build context are untrusted input.

- **Parser**: file size (1 MiB), line length (256 KiB) and instruction count
  (4096) are bounded; input must be UTF-8; unknown instructions are errors.
- **Expander**: a value is inserted as data and never re-scanned, so a
  variable cannot inject quotes, `$` or anything else; there is no command
  substitution; one word expands to at most 1 MiB and `${…}` nests at most
  32 deep; patterns compile to Go regular expressions (linear time).
- **Ignore files**: at most 1 MiB and 4096 patterns; malformed patterns are
  errors, never "match nothing".
- **Fuzzed** in CI (corpus seeded from the golden files): the parser, the
  expander, the ignore matcher. `govulncheck` runs on every change.

## References

What each specification decides here, and the measurement behind writing
the parser rather than importing BuildKit's (+6.1 MB to a js/wasm binary),
are in [docs/REFERENCES.md](docs/REFERENCES.md). Docker is a trademark of
Docker, Inc.; this project is not affiliated with it. "Dockerfile" is used
only as the name of the file format it reads.

## License

BSD 3-Clause, see [LICENSE](LICENSE). The golden test files copied from
BuildKit under `*/testdata/buildkit` are Apache-2.0 (see the `NOTICE` and
`LICENSE` beside them).
