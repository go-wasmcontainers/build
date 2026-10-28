# What the builder needs from the engine

The builder drives the engine only through its API (package `driver`). It
uses the endpoints the engine serves since v0.11.0 (`GET /images/{name}/json`,
`POST /images/create`, `POST /containers/create`, `start`, `wait`, `logs`,
`DELETE /containers/{id}`) and these, which the engine serves since
v0.12.0 (this page was their specification). Their shapes are the Docker
Engine API's (`api/swagger.yaml` in moby/moby). The tests run against the
real engine; `internal/enginetest.NewPrototype` keeps the prototype the
engine's endpoints were specified from, and `driver`'s contract test
checks that both answer alike.

Still open on the engine side: `HEAD /containers/{id}/archive` is served
by the GET handler, which builds the tar and discards it; a stat-only HEAD
would make WORKDIR checks cheap.

| Endpoint | Used for | Contract |
|---|---|---|
| `POST /commit?container=&repo=&tag=&comment=&author=` | every step | Body: the image config (`ContainerConfig` field names: `Cmd`, `Entrypoint`, `Env`, `WorkingDir`, `Labels`, `User`, `ExposedPorts`, `Volumes`, `StopSignal`, `Shell`, `OnBuild`, `Healthcheck`), which **replaces** the container's in the new image. New layer = the container's change set (`rootfs.WriteDiff`); its digest is appended to `rootfs.diff_ids`. A history entry is appended: `created_by` = the container's Entrypoint and Cmd joined by spaces, `comment` = `comment`; when the change set is empty, `empty_layer: true` and no layer. Reply `201 {"Id": "<config digest>"}`. Without `repo` the image has no name and must still resolve by ID (`sha256:…` and unique prefixes) — a dangling image, removed by `image prune`. |
| `POST /images/{name}/tag?repo=&tag=` | `-t` | `201`; `404` for an unknown image. |
| `POST /images/load?quiet=1` | `FROM scratch` | Body: a tar of an **OCI image layout** (`oci-layout`, `index.json`, `blobs/sha256/…`) holding one image. Named by the index entry's `org.opencontainers.image.ref.name` when present, else dangling. Reply: a JSON stream with `{"stream":"Loaded image: NAME\n"}` or `{"stream":"Loaded image ID: sha256:…\n"}`. It reads no host path. |
| `PUT /containers/{id}/archive?path=&noOverwriteDirNonDir=1` | COPY, ADD, WORKDIR | Extract a tar (uncompressed) below `path` in the container's root filesystem; names are relative, cleaned and kept inside; missing parent directories are created (0755); with `noOverwriteDirNonDir` replacing a directory by a non-directory or the reverse is a `400`. The container need not have run. |
| `HEAD /containers/{id}/archive?path=` | WORKDIR | `200` when `path` exists, `404` when not. |
| `GET /containers/{id}/archive?path=` | `COPY --from` | A tar of `path`: its base name is the root entry (`path=/app/bin` → `bin/…`); for `path=/`, names relative to the root. Symlinks as links, not followed. |

And two corrections of what exists:

- `GET /images/{name}/json` must return the whole `Config` (today `User`,
  `ExposedPorts`, `Volumes`, `StopSignal`, `OnBuild`, `Shell`,
  `Healthcheck` are dropped, so a build `FROM` such an image loses them),
  and `RootFS.Layers` should be the `diff_ids` (today: the manifest's
  compressed layer digests).
- The API's 1 MiB request-body bound must not apply to `PUT …/archive` and
  `POST /images/load`, which need their own bounds (the builder sends at
  most 256 MiB per layer).

Wiring `wctr build`: `cli.Command()` (or `cli.NewCommand(cli.Options{…})`)
returns the cobra command. `Options.Engine` receives the command so the
host can hand over its own client: in `wctr`, a `driver.NewClient(hc,
base)` over the same `*http.Client` (unix socket natively, `inproc` in the
browser). Without it, the command dials `-H/--host` (a persistent flag of
a parent), else `$WCTR_HOST`, else `unix:///var/run/wctr.sock`.

## Exit statuses

The builder maps the engine's run statuses: when the engine cannot start
the step's module (127 module not found, 126 not executable, reported with
an error message), a shell-form `RUN` becomes "RUN in shell form needs the
shell …", naming the missing shell, and an exec-form `RUN` "… was not
found in the image". A status the program itself returns — including the
shell's 127 for a command it cannot find — stops the build with
`process "…" did not complete successfully: exit code: N`.

## Shell-form RUN

Since engine v0.17.0 the engine's WASI host serves the WASIX process subset
(`wasix_32v1`: `proc_spawn2/3`, `proc_join`, `fd_pipe`, …; `wctrd` enables it
with its default `--wasi engine`). The shell base image's `/bin/sh` uses it to
run pipelines and programs, so `RUN echo hi > /x && cat /x | wc -c` works
`FROM shell`. An embedder building its own executor must add
`wasi.WASIX` to `wazero.Options.HostModules` (as `internal/enginetest`
does), otherwise the shell traps with "module[wasix_32v1] not instantiated".

## Later: `RUN pkgx install …`

An apt-like step that installs go-pkgx's wasip1 bottles needs, from the
engine, what any networked `RUN` needs: a network for build containers
(the default network today; `RUN --network=none` is honoured), and DNS and
HTTPS out of it (the in-engine networks have no outside access yet). It
needs a `pkgx` (or `pkgm`) wasip1 binary in a base image, able to resolve,
fetch, verify and unpack a bottle (`tar+zstd`) into the root filesystem
without spawning processes. A builder-side alternative needing no network
in the engine: `COPY --from=<bottle image>` of bottles published as OCI
images (the engine already pulls `application/vnd.pkgx.bottle.layer.v1.tar+zstd`
layers) — nothing in the builder stands in the way, but it is not tested
yet.
