# CLAUDE.md

Three Docker CLI plugins — `docker pin`, `docker upgrade`, `docker unpin` — that
pin/upgrade/unpin a Docker Compose service's image to a specific tag **and** SHA
digest (`image:tag@sha256:...`), rewriting the `image:` line in place. The repo
is also the Homebrew tap it's distributed through
(`github.com/Miista/homebrew-docker-pin`), so the module path is
`github.com/Miista/homebrew-docker-pin`.

## Build / test / install

```sh
make build      # builds docker-pin, docker-upgrade, docker-unpin in repo root
make install    # build + install -m 755 into ~/.docker/cli-plugins/
make clean      # remove built binaries
go test ./...   # unit tests (internal/compose, internal/registry)
```

Each binary is built from its own `cmd/<name>` package. `version` is injected via
`-ldflags "-X main.version=..."` (defaults to `dev`).

## Architecture

### Plugins (`cmd/docker-{pin,upgrade,unpin}/main.go`)
Each `main` is a Docker CLI plugin: invoked as `docker-<name> docker-cli-plugin-metadata`
it prints the metadata JSON Docker expects; otherwise it strips a leading
`<name>` arg (Docker passes it) and runs. All three accept `<service>` or `--all`
(iterate every service from the compose file, collecting failures). `upgrade` also
takes an optional positional `[version]`.

### `internal/compose`
- `FindFile(dir)` — walks up from `dir` looking for `docker-compose.yml|yaml` /
  `compose.yml|yaml`.
- YAML is parsed only to read service names / image strings. `ListServices`,
  `RawImage` (image exactly as written), `ParseImage` (base + tag, stripping any
  existing `@sha256:` digest; defaults tag to `latest`).
- `PinImage` rewrites the `image:` line via line-based regex (NOT YAML
  re-serialization) so all surrounding formatting and comments are preserved.

### `internal/docker`
Shells out to the `docker` CLI. `Pull` streams `docker pull`; `GetDigest` runs
`docker image inspect --format '{{index .RepoDigests 0}}'` and extracts the
`sha256:...` repo digest of a locally-present image.

### `internal/registry`
Resolves which *version* tag (e.g. `1.2.3`) corresponds to a digest, so a
`latest` pin can be recorded under a meaningful tag.
- `ResolveVersionTag(baseImage, digest)` dispatches by registry: `ghcr.io/` →
  GHCR; no dot in first path segment or `docker.io/` prefix → Docker Hub;
  anything else → generic OCI Distribution (`oci.go`), discovering bearer auth
  from the `WWW-Authenticate` challenge and a `/token` realm request.
- Each resolver lists tags, keeps only version-like tags (`versionRe`:
  digits-and-dots with optional suffixes), sorts by `tagSpecificity` (more dots,
  then longer — so the most specific immutable tag such as a `-g<sha>` build tag
  wins over a bare `1.2`), and matches each tag's manifest digest against the
  local digest (OCI/GHCR cap manifest checks at 20).
- `Result` carries the matched `Tag` and `VersionTagsSeen`.
- `ResolveOrWarn(baseImage, pullTag, digest, service)` is the entry point the
  plugins call. It prints progress and, on failure, distinguishes three cases:
  resolution error, registry publishes no version tags, or version tags exist but
  **none match the local digest** (orphaned/stale image — warns that a newer build
  replaced the tag and suggests `docker upgrade <service>`). In every failure case
  it falls back to pinning with `pullTag` unchanged.

## Command semantics

- **pin** `<service>`: no-op if the image is already digest-pinned. Uses the
  *local* digest, pulling only if the image isn't present locally. If the tag is
  `latest`, resolves it to a version tag via `ResolveOrWarn`. Writes
  `base:tag@sha256:...`.
- **upgrade** `<service> [version]`: *always* pulls (`version` or `latest`), then
  pins to the freshly pulled digest; resolves the version tag when pulling
  `latest`. `--all` cannot be combined with a version.
- **unpin** `<service>`: strips the `@sha256:...` digest, keeping `base:tag`;
  no-op if not pinned.
- `--all` is supported by all three.

## Release & distribution

Pushing a `vX.Y.Z` tag triggers `.github/workflows/release.yml` →
`goreleaser release --clean` (`.goreleaser.yaml`): builds linux/darwin ×
amd64/arm64 archives, creates the GitHub release, and commits an updated
`Formula/docker-pin.rb` back to this repo (the `brews` block).

This repo **is** the Homebrew tap. The formula installs the binaries into
`#{HOMEBREW_PREFIX}/lib/docker/cli-plugins`; because that isn't a default Docker
plugin dir, the formula caveat tells users to add it to `cliPluginsExtraDirs` in
`~/.docker/config.json`.

## Conventions

- New subcommand = new `cmd/docker-<name>` `main` that responds to
  `docker-cli-plugin-metadata` and strips its own leading arg; register it in the
  `Makefile` `BINARIES`, `.goreleaser.yaml` `builds`/`archives`, and the formula
  `install` line.
- Only the standard library plus `gopkg.in/yaml.v3`; the `docker` CLI must be on
  PATH at runtime.
