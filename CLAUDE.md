# CLAUDE.md

Two Docker CLI plugins — `docker pin` and `docker unpin` — that
pin/upgrade/unpin a Docker Compose service's image to a specific tag **and** SHA
digest (`image:tag@sha256:...`), rewriting the `image:` line in place. The repo
is also the Homebrew tap it's distributed through
(`github.com/Miista/homebrew-docker-pin`), so the module path is
`github.com/Miista/homebrew-docker-pin`.

## Build / test / install

```sh
make build      # builds docker-pin, docker-unpin in repo root
make install    # build + install -m 755 into ~/.docker/cli-plugins/
make clean      # remove built binaries
go test ./...   # unit tests (internal/compose, internal/registry)
```

Each binary is built from its own `cmd/<name>` package. `version` is injected via
`-ldflags "-X main.version=..."` (defaults to `dev`).

## Architecture

### Plugins (`cmd/docker-{pin,unpin}/main.go`)
Each `main` is a Docker CLI plugin: invoked as `docker-<name> docker-cli-plugin-metadata`
it prints the metadata JSON Docker expects; otherwise it strips a leading
`<name>` arg (Docker passes it) and runs. Both accept `<service>` or `--all`
(iterate every service from the compose file, collecting failures).

`docker pin` also accepts an `upgrade` subcommand with an optional positional
`[version]` argument.

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
  digits-and-dots with optional suffixes), sorts newest-first by
  `CompareVersions` (numeric dotted-core comparison, not string specificity —
  a registry can have hundreds of version tags, e.g. linuxserver images, and
  only the top N are checked, so a naive specificity sort can starve out the
  real match), and matches each tag's manifest digest against the local
  digest (OCI/GHCR cap manifest checks at 20).
- `Result` carries the matched `Tag`, `VersionTagsSeen`, and `ChecksFailed`
  (manifest checks that errored rather than genuinely not matching — e.g.
  registry throttling — so a "no match" caused by failed checks is
  distinguishable from a confident one).
- `ResolveOrWarn(baseImage, pullTag, digest, service)` is the entry point
  **plain `docker pin` (not `upgrade`)** calls. It prints progress and, on
  failure, distinguishes three cases: resolution error, registry publishes no
  version tags, or version tags exist but **none match the local digest**
  (orphaned/stale image — warns that a newer build replaced the tag and
  suggests `docker pin upgrade <service>`). In every failure case it falls
  back to pinning with `pullTag` unchanged. `ResolveTag` is the same logic
  without printing, returning the warning text instead, for callers that need
  to defer/buffer it.

## Command semantics

- **`docker pin <service>`**: no-op if the image is already digest-pinned. Uses the
  *local* digest, pulling only if the image isn't present locally. If the tag is
  `latest`, resolves it to a version tag via `ResolveOrWarn` — a first-time,
  low-stakes label guess, since the digest (not the tag) is what's pinned.
  Writes `base:tag@sha256:...`.
- **`docker pin upgrade <service> [version]`**: *always* pulls — `version` if
  given, otherwise the moving tag `registry.MovingPullTag` derives from the
  service's current tag (e.g. a service pinned at `1.4.2` checks `1.4`; one
  on `latest` checks `latest`) — then pins to the freshly pulled digest
  **under that same pulled tag**. Unlike plain `pin`, upgrade never tries to
  relabel/guess a different version tag from the digest: the tag is already
  meaningful (either explicit, or whatever the service was already
  tracking), so there's nothing to resolve. `--all` cannot be combined with a
  version. `--all` pulls all services concurrently (default concurrency 4,
  `--concurrency`/`-j` to override), then applies compose file writes
  sequentially so nothing races.
- **`docker pin list [--missing] [-q]`**: read-only table of every service's
  image/tag/digest/pin-status (compose parse only — no docker/network calls).
  `--missing` shows only unpinned services and exits 1 if any exist (CI gate);
  `-q` prints bare service names for piping.
- **`docker pin schedule <apply|status|remove|run>`**: declarative scheduled
  upgrades driven by a `pin.yaml` (or `.yml`) next to the compose file
  (`schedule:` cron expr, optional `services:` list, optional `on_change:`
  hook, optional `notify.ntfy:` target). A `services:` entry is either a bare
  name or `{name, tags, exclude, delay}` (custom `Service.UnmarshalYAML`):
  `tags` is a regex constraining which registry tags qualify, `exclude` drops
  matching candidates, and `delay` ("48h"/"7d"/"2w", `schedule.ParseDelay`)
  requires a candidate to have been published at least that long ago —
  `constrainedTarget` walks the candidates newest-first (capped at
  `maxDelayChecks`=10 date lookups) and picks the newest sufficiently aged
  one. Publish time comes from `registry.TagCreated`: the Docker Hub tag API's
  `tag_last_pushed`, or manifest→config-blob `created` for GHCR/any OCI
  registry (multi-arch indexes descend into the first non-attestation
  sub-manifest). Candidate selection is `registry.ListTags` +
  `MatchingCandidates` sorted by `registry.CompareVersions` (numeric dotted
  cores; suffixed builds rank below the bare release), all in
  `internal/registry/tags.go`. Constrained services are skipped — never
  falling back to a moving tag — when nothing qualifies. `exclude`/`delay`
  require `tags`. `run` executes each service as an independent transaction
  (`upgradeServiceTxn`): pin rewrite → `docker compose up -d <service>` →
  `on_change` (env: `PIN_SERVICE`, `PIN_OLD_IMAGE`/`PIN_NEW_IMAGE`,
  `PIN_OLD_TAG`/`PIN_NEW_TAG`, `PIN_OLD_DIGEST`/`PIN_NEW_DIGEST` via
  `tagAndDigest`, so hooks can commit per service) → per-service ntfy notification
  (`notifyUpgraded`/`notifyFailed`). A failed `compose up` rolls back only
  that service's pin (restore pre-txn bytes + re-up) — other services proceed
  and the upgrade retries next run; a failed `on_change` is non-fatal (warn +
  note in the notification — a stranded commit rides with the next push).
  `run --dry-run` (`-n`) does full discovery including pulls but changes
  nothing — prints "Would upgrade ..." per service; no compose rewrite, up,
  on_change, or notifications.
  `notify.ntfy` (url + topic; token via `token_env`, default `NTFY_TOKEN`,
  optionally sourced from a `token_file` KEY=VALUE file so no secret sits in
  pin.yaml) makes `run` post a summary when anything upgraded or failed
  (failures at priority 4, via `internal/notify`); notification failures only
  warn. `apply`/`remove` install/remove a system-level systemd timer+service
  pair (`docker-pin-<slug>.*` in /etc/systemd/system; slug = compose-dir
  basename + path-hash; root + Linux required); `apply` is idempotent.
  `status` shows config, install/drift state and next fire time. `run` is what
  the service ExecStart calls: upgrades each configured service (collecting
  failures), and if the compose file changed runs `docker compose up -d` then
  `on_change` via `sh -c` in the compose dir. Cron→OnCalendar translation
  lives in `internal/croncal` (rejects restricting both day-of-month and
  day-of-week: cron ORs, systemd ANDs); config/unit generation in
  `internal/schedule` (pure, tested on any OS); systemctl/exec calls are
  seamed via `sysFuncs` in `cmd/docker-pin/schedule.go`.
- **`docker unpin <service>`**: strips the `@sha256:...` digest, keeping `base:tag`;
  no-op if not pinned.
- `--all` is supported by all commands.

## Release & distribution

Pushing a `vX.Y.Z` tag triggers `.github/workflows/release.yml` →
`goreleaser release --clean` (`.goreleaser.yaml`): builds linux/darwin ×
amd64/arm64 archives, creates the GitHub release, and commits an updated
`Formula/docker-pin.rb` back to this repo (the `brews` block).

GoReleaser also builds `.deb` packages (`nfpms` block) installing both plugins
into `/usr/libexec/docker/cli-plugins` (scanned by Docker by default on Linux).
A follow-up step in `.github/workflows/release.yml` publishes each release's
`.deb`s to a shared Cloudsmith apt repo (see that file's comments for the
current mechanism and client setup — it has drifted before, so treat it as
the source of truth over any paraphrase here).

This repo **is** the Homebrew tap. The formula installs the binaries into
`#{HOMEBREW_PREFIX}/lib/docker/cli-plugins`; because that isn't a default Docker
plugin dir, the formula caveat tells users to add it to `cliPluginsExtraDirs` in
`~/.docker/config.json`.

## Index vs manifest digests (multi-arch)

`docker image inspect` returns the **index** (manifest-list) digest — the hash of
the multi-arch wrapper that lists per-platform sub-manifests. Two tags can point to
byte-identical linux/amd64 images yet have different index digests if one index
carries extra platforms (e.g. `latest` includes Windows images, `alpine` does not).

This means:
- Pinning the index digest is correct — it keeps the pin portable across
  architectures. Pinning a per-arch manifest digest would lock to one platform.
- The "already up to date" check compares index digests, so it will not recognise
  `latest` and `alpine` as identical even when the running image bits are the same.
  This is defensible (different indexes are different artifacts).
- The Docker Hub "Digest" column shows the per-arch manifest digest, which may
  differ from the index digest our tool records. This is expected, not a bug.

## Conventions

- New subcommand on `docker pin` = new `case` in `cmd/docker-pin/main.go`'s `main()` switch.
- New top-level plugin = new `cmd/docker-<name>` package; register in `Makefile` `BINARIES`,
  `.goreleaser.yaml` `builds`/`archives`, and the formula `install` line.
- Only the standard library plus `gopkg.in/yaml.v3`; the `docker` CLI must be on
  PATH at runtime.
