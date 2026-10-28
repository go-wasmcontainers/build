# build

[![Docs](https://img.shields.io/badge/docs-hugo%20%2B%20hextra-0079A8)](https://go-wasmcontainers.github.io/docs/)
[![License: BSD-3-Clause](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![CI](https://github.com/go-wasmcontainers/build/actions/workflows/ci.yml/badge.svg)](https://github.com/go-wasmcontainers/build/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-wasmcontainers/build.svg)](https://pkg.go.dev/github.com/go-wasmcontainers/build)
![status](https://img.shields.io/badge/status-M1-green)

**`wctr build`: build OCI images from a Containerfile (or a Dockerfile,
same syntax) for the [go-wasmcontainers engine](https://github.com/go-wasmcontainers/engine)**,
whose containers are WASI preview 1 (`wasip1/wasm`) modules. Pure Go
(`CGO_ENABLED=0`), so it also runs in the browser
[playground](https://github.com/go-wasmcontainers/playground). The builder
is a separate module that drives the engine through its API, the way
BuildKit sits beside a container daemon: each step runs in a container
made from the previous step's image, which is then committed.

> **Status: M1.** Multi-stage builds, the build cache, `.containerignore`,
> and every instruction below are done, and tested against the real engine
> (v0.12.0, in-process, wazero), which serves every endpoint a build needs
> ([docs/ENGINE.md](docs/ENGINE.md)). `wctr-build` works with a `wctrd` of
> v0.12.0 or later; `cli.ForEngine` plugs the command into `wctr` through
> its extension hook (`wctr build` appears once wctr wires it).

## Demo

```console
$ go install github.com/go-wasmcontainers/build/cmd/wctr-build@latest
$ cat Containerfile
FROM scratch AS tools
COPY tool.wasm /bin/tool.wasm
ARG GREETING=hi
RUN ["/bin/tool.wasm", "write", "/out/greeting", "made at build time"]

FROM scratch
COPY --from=tools /bin/tool.wasm /bin/
COPY --from=tools /out/ /data/
ENTRYPOINT ["/bin/tool.wasm", "cat"]
CMD ["/data/greeting"]
$ wctr-build -H "$WCTR_HOST" -t lab/app:v1 .
[1] STEP 1/5: FROM scratch
…
Successfully built 0f32949b3a81
Successfully tagged lab/app:v1
$ wctr run --rm lab/app:v1
made at build time
```

This sequence is `TestMultiStageBuildAndRun` in `builder`, which builds
the Go test program `testdata/wasm/tool` for wasip1 and runs the result.

## Command

`wctr build [OPTIONS] PATH | -` (standalone: `wctr-build`, with
`-H/--host unix:///path`, else `$WCTR_HOST`, else `unix:///var/run/wctr.sock`).

| Flag | |
|---|---|
| `-t, --tag NAME[:TAG]` | name the image (repeatable) |
| `-f, --file FILE` | the Containerfile (`-`: standard input). Default `PATH/Containerfile`, else `PATH/Dockerfile` |
| `--build-arg K=V` | a build argument (`K` alone: from the environment) |
| `--target STAGE` | build up to this stage |
| `--no-cache` | run every step |
| `--progress plain` | plain progress (the only kind; `auto` is accepted) |
| `-q, --quiet` | print only the image ID |

`PATH` is the build context, sent without what its `.containerignore`
(else `.dockerignore`) excludes; `-` reads the context as a tar on
standard input, the Containerfile inside it.

## Instructions

| Instruction | Support |
|---|---|
| `FROM [--platform=wasip1/wasm] IMAGE [AS NAME]` | images are pulled for `wasip1/wasm` (another platform is an error); `scratch`; earlier stages; ARGs declared before the first FROM; ONBUILD triggers of the base run after it |
| `ARG`, `ENV`, `LABEL` | the reference's scoping: global ARGs, re-declaration in a stage, ENV over ARG, predefined proxy ARGs (out of the cache key) and `TARGETPLATFORM` & co. |
| `RUN ["prog", "arg"]` | runs a WebAssembly program in the step's container; `--network=none` |
| `RUN cmd` (shell form) | runs `SHELL` (default `["/bin/sh", "-c"]`) with the command. The engine does not spawn processes yet, so this needs an image whose `/bin/sh` is itself a program that runs without spawning (a builtin-only shell); otherwise the build stops with "RUN in shell form needs the shell "/bin/sh" … the image has no shell, or the engine cannot spawn processes yet". A real `/bin/sh` and process spawning are on their way (go-wasmcontainers/shell). |
| `COPY`, `ADD` | from the context or `--from=` a stage (name or index) or an image; wildcards (`filepath.Match`), directory contents, trailing `/`, relative destinations against WORKDIR, `--chown=UID[:GID]` (numeric), `--chmod=OCTAL`, `--link` (accepted, no effect); ADD unpacks local tar, tar.gz, tar.bz2 archives (`--unpack=false` to keep them) |
| `WORKDIR` | relative to the previous one; created when missing |
| `CMD`, `ENTRYPOINT`, `SHELL`, `USER`, `EXPOSE`, `VOLUME`, `LABEL`, `MAINTAINER` | recorded in the image (a new ENTRYPOINT clears an inherited CMD); USER is recorded only: WASI has no users |
| `STOPSIGNAL`, `HEALTHCHECK` | recorded, with a warning: a WebAssembly guest gets no signals, and the engine runs no health checks yet |
| `ONBUILD` | recorded; run when the image is used as a base |
| Not yet | here-documents, `RUN --mount`, `ADD` from a URL or Git, `COPY --parents/--exclude`, `--chown` by name, symbolic `--chmod`, xz/zstd archives in ADD — each is a precise error, never ignored |

Every step adds one history entry (`created_by` like
`/bin/sh -c #(nop) ENV A=1` for metadata, `COPY file:<digest> in /dst`);
metadata steps add an empty layer.

## Cache

A step is reused when the parent image, the configuration it produces, the
instruction (after expansion) and the digest of its inputs are the same:
for COPY/ADD the names, modes, owners, links and contents of what is copied
(not modification times, as the reference says); for RUN the command, its
build arguments (so a changed `--build-arg` misses from the first RUN after
the ARG, as the reference describes), and its network. A hit is used only
after the engine confirms the image still exists. The cache is kept in
`$XDG_CACHE_HOME/wasmcontainers/build-cache.json` (0600, bounded), in
memory in a browser.

## Packages

| Package | What it does |
|---|---|
| `parser` | Containerfile/Dockerfile → instructions (directives, comments, continuation, exec and shell forms, flags, ENV/LABEL/ARG/HEALTHCHECK/ONBUILD); BuildKit's golden parser corpus passes unchanged |
| `expand` | variable substitution as the reference specifies (`$v`, `${v}`, `${v:-w}` … `${v//p/r}`), word splitting; nothing is executed |
| `ignore` | `.containerignore` / `.dockerignore` matching |
| `buildcontext` | the build context in memory, from a tar; `Pack` sends a directory through an `os.Root` |
| `driver` | the engine API client the builder uses (`Engine` interface) |
| `builder` | the build: stages, instructions, layers, cache |
| `cli` | the cobra command, `Command()` for `wctr build` |
| `cmd/wctr-build` | the standalone program |

## Security

The Containerfile and the build context are untrusted input.

- **Nothing on the host but the context**: the builder reads the context
  from a tar held in memory; the client sends a directory through an
  `os.Root`, so no file outside it is read, whatever links it holds;
  symbolic links are sent as links, never followed, and owners are not
  sent (files from the context belong to root unless `--chown`).
- **Paths**: context names that are absolute, hold `..` or NUL are refused;
  COPY sources lose `..` and a leading `/` (as the reference says); a
  source that goes through a symbolic link is followed only inside the
  context — a link to an absolute path or above the root is an error;
  destinations are cleaned and absolute.
- **Bounds**: context 256 MiB / 65536 entries / 4096-byte names / depth
  128; one COPY layer the same; ADD archives stop at the same size
  whatever their compression ratio; Containerfile 1 MiB, 4096
  instructions; 1000 steps per build (ONBUILD triggers counted); 127
  layers; one expanded word 1 MiB, `${}` nesting 32; a stage's ARG and
  ENV values 1 MiB together, an image configuration 1 MiB.
- **Expansion** never re-scans a value (no injection), runs nothing, and
  `${v//p/r}` inserts `r` literally.
- **No network from the builder**: ADD from a URL is refused; a RUN step's
  network is the engine's, `--network=none` honoured, `host` refused.
- **Fuzzed** in CI: the parser, the expander, the ignore matcher, the
  context reader. `govulncheck` runs on every change.

## Performance

`go test -run '^$' -bench Build -cpu 1 ./builder` (Apple M4 Max, go1.27.3;
load average 66 when measured, so indicative only): a seven-instruction
build with one RUN takes 61.7 ms cold (`--no-cache`, module already
compiled by the engine) and 4.8 ms when every step is a cache hit (one
image inspect per step), 12.8× faster. Layers are computed in memory from
the context, bounded as above. Not yet compared with BuildKit on the same
Containerfile — an open item.

In the browser, `wctr build` costs 0.98 MB of js/wasm: a program holding
the engine's (v0.12.0) `wctr` command tree is 13,777,834 bytes, 14,757,857
with the build command added (go1.27.3, `GOOS=js`).

## Limitations

- An engine of v0.12.0 or later is required (the endpoints in
  [docs/ENGINE.md](docs/ENGINE.md)). The engine answers `HEAD` on a
  container archive by building the whole tar and discarding it, so a
  WORKDIR check on a large directory costs a walk of it.
- Shell-form RUN needs a builtin-only shell until the engine spawns
  processes; step output is printed when the step ends, not streamed.
- `RUN pkgx install …` (installing go-pkgx bottles) is a later goal; what it
  needs is in [docs/ENGINE.md](docs/ENGINE.md).

## References

Every format is implemented from its specification:
[docs/REFERENCES.md](docs/REFERENCES.md), including the measurement behind
writing the parser rather than importing BuildKit's (+6.1 MB to a js/wasm
binary). Docker is a trademark of Docker, Inc.; this project is not
affiliated with it. "Dockerfile" is used only as the name of the file
format it reads, and the engine API is described as compatible with a
subset of the Docker Engine API.

## License

BSD 3-Clause, see [LICENSE](LICENSE). The golden test files copied from
BuildKit under `*/testdata/buildkit` are Apache-2.0 (see the `NOTICE` and
`LICENSE` beside them).
