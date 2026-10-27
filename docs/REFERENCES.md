# References

Every format this module reads or writes is implemented from its
specification or reference implementation. This lists what was opened, and
what each one decides here.

## Containerfile / Dockerfile syntax

- **Dockerfile reference**, `frontend/dockerfile/docs/reference.md` in
  [moby/buildkit](https://github.com/moby/buildkit) (the source of
  docs.docker.com/reference/dockerfile/). Decides: comments and leading
  whitespace ("Format"); parser directives `syntax`, `escape`, `check`, only
  before any comment, blank line or instruction, each at most once, keys
  case-insensitive, no continuation ("Parser directives"); the escape token
  `\` or `` ` ``, also the line continuation; environment replacement and
  its modifiers (`${v:-w}`, `${v-w}`, `${v:+w}`, `${v+w}`, `${v#p}`,
  `${v##p}`, `${v%p}`, `${v%%p}`, `${v/p/r}`, `${v//p/r}`), which
  instructions expand variables (ADD COPY ENV EXPOSE FROM LABEL STOPSIGNAL
  USER VOLUME WORKDIR, ONBUILD with those), one value per variable for the
  whole instruction; shell and exec forms (JSON arrays, double quotes only);
  FROM `[--platform] image [AS name]`, ARG before FROM and its re-declaration
  inside a stage; ARG scope (from its line on, per stage, ENV wins over ARG,
  predefined proxy ARGs); COPY/ADD sources relative to the context with `..`
  and a leading `/` removed, `filepath.Match` wildcards, directory contents
  copied not the directory, trailing `/` on the destination significant,
  relative destinations against WORKDIR, `--from`, `--chown`, `--chmod`;
  CMD/ENTRYPOINT interaction (a new ENTRYPOINT resets an inherited CMD);
  WORKDIR relative to the previous one and created if missing; ONBUILD
  triggers (no `ONBUILD ONBUILD`, no FROM/MAINTAINER triggers, run after the
  downstream FROM, not inherited by grandchildren); STOPSIGNAL; HEALTHCHECK;
  SHELL; VOLUME; USER; EXPOSE; here-documents (refused for now).
- **Containerfile(5)** and **containerignore(5)**, `docs/Containerfile.5.md`
  and `docs/containerignore.5.md` in
  [containers/common](https://github.com/containers/common). Decides: the
  default file is `Containerfile`, then `Dockerfile`, same syntax; the ignore
  file is `.containerignore`, then `.dockerignore`; its examples
  (`*/temp*`, `*/*/temp*`, `temp?`, `!` exceptions where the last matching
  line wins, `*` then `!` to list what to include) are this module's tests.
- **BuildKit's parser**, `frontend/dockerfile/parser` (`parser.go`,
  `directives.go`, `split_command.go`, `line_parsers.go`) at v0.34.1: the
  grammar this parser follows — line continuation only when the escape token
  is not itself escaped, comment lines skipped inside a continuation, empty
  continuation lines skipped with a warning, the leading `--flag` words,
  the per-instruction argument forms (JSON-or-string for RUN CMD
  ENTRYPOINT SHELL; JSON-or-words for ADD COPY VOLUME; words for FROM
  EXPOSE; key=value or legacy `key value` for ENV LABEL; name[=value] words
  for ARG; type plus command for HEALTHCHECK; a sub-instruction for ONBUILD).
  Its golden corpus (`testfiles`, `testfiles-negative`, 32 + 4 files,
  Apache-2.0, copied under `parser/testdata/buildkit` with its licence)
  passes unchanged: `Result.Dump` prints the same S-expressions.
  **Not imported**: measured with go1.27.3, a program that only calls
  `parser.Parse` links 34 non-standard packages (buildkit, protobuf,
  typeurl, vtprotobuf, pkg/errors …) and resolves 224 modules; its js/wasm
  binary is 8,659,143 bytes against 2,530,298 for the same program without
  it (+6.1 MB). The builder must also run in the browser playground, so the
  grammar is reimplemented (`parser`, standard library only). Deliberate
  differences: unknown instructions are an error at parse time (BuildKit
  defers it), flag words keep non-ASCII bytes intact, and here-documents are
  refused with a precise error.
- **BuildKit's shell lexer**, `frontend/dockerfile/shell/lex.go` at
  v0.34.1: the expansion grammar `expand` follows (quotes, escapes, `$name`,
  `${name}`, positional and special parameters expanding to nothing, the
  modifiers and their error messages, pattern operands where `*` `?` `\`
  are special and nothing else, shortest match by reversing for `%`). Its
  `envVarTest` (every Unix case) and `wordsTest` tables (Apache-2.0, copied under
  `expand/testdata/buildkit`) pass unchanged. Deliberate difference:
  `${v//p/r}` inserts `r` literally (BuildKit's `ReplaceAllString` would
  read `$1` in an expanded value as a regexp group).
- **moby/patternmatcher** v0.6.1 (`patternmatcher.go`,
  `ignorefile/ignorefile.go`): how `.dockerignore` patterns are read
  (BOM stripped, `#` comments in column 1, trimmed, `path.Clean`, leading
  `/` dropped, `!` kept) and matched (`*` and `?` never cross `/`, `**`
  crosses any number of directories, a path is excluded when it or a parent
  matches, the last matching pattern wins, exceptions require walking into
  excluded directories). Its `TestMatches` table is ported to
  `ignore/ignore_test.go`. Deliberate difference: an escaped character
  (`\x`) is always literal (patternmatcher pastes it into the regexp, so
  `\d` would match a digit).

## Images

- **OCI image-spec**, `config.md`
  ([opencontainers/image-spec](https://github.com/opencontainers/image-spec)):
  `rootfs.diff_ids` (one per layer, the digest of the uncompressed tar),
  `history` (`created`, `created_by`, `comment`, `empty_layer` for a step
  that changes no file — ENV, CMD, …; its example writes metadata steps as
  `/bin/sh -c #(nop) …`). The builder writes metadata steps that way and
  computes COPY layers whose digests are taken over the uncompressed tar.
- **OCI image-spec**, `image-layout.md`: the `oci-layout` file with
  `imageLayoutVersion`, `index.json` as the entry point, blobs under
  `blobs/<alg>/<encoded>`, the `org.opencontainers.image.ref.name`
  annotation as a tag (valid only on `index.json` descriptors). The empty
  image `FROM scratch` starts from is such a layout, sent to `/images/load`.

## Engine API

- **Docker Engine API**, `api/swagger.yaml` in
  [moby/moby](https://github.com/moby/moby): the shapes of the endpoints
  the builder asks of the engine (docs/ENGINE.md) — `POST /commit` (query
  `container`, `repo`, `tag`, `comment`, `author`; body a
  `ContainerConfig`; `201` with an ID), `POST /images/{name}/tag` (`repo`,
  `tag`; `201`), `POST /images/load` (a tar body, `quiet`),
  `PUT /containers/{id}/archive` (`path`, `noOverwriteDirNonDir`: "an error
  if unpacking … would cause an existing directory to be replaced with a
  non-directory and vice versa"), `HEAD` and `GET` on the same path (stat,
  tar of a resource). The engine serves a subset of this API; these are
  the shapes it is asked to add.
