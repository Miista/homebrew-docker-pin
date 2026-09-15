# CLAUDE.md

Two Docker CLI plugins — `docker pin` and `docker unpin` — that
pin/upgrade/unpin a Docker Compose service's image to a specific tag **and** SHA
digest (`image:tag@sha256:...`), rewriting the `image:` line in place; and
**duva**, a container that watches pinned services and applies the updates each
one's policy allows. The repo is also the Homebrew tap it's distributed through
(`github.com/Miista/homebrew-docker-pin`), so the module path is
`github.com/Miista/homebrew-docker-pin`.

## Build / test / install

```sh
make build      # builds docker-pin, docker-unpin in repo root
make install    # build + install -m 755 into ~/.docker/cli-plugins/
make clean      # remove built binaries

make test             # unit + integration
make test-unit        # fast, no docker
make test-integration # real registry, real daemon, all local
make cover            # unit / integration / merged breakdown
```

Integration tests are Go behind a build tag (`-tags integration`) in
`test/integration`, driving real containers against a local registry. Nothing
upstream is contacted, so they work offline. `go run ./test/sandbox
<suite>/<scenario>` stands one of their scenarios up by hand and leaves it
running.

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

### `compose/` (module)
- `FindFile(dir)` — walks up from `dir` looking for `docker-compose.yml|yaml` /
  `compose.yml|yaml`.
- YAML is parsed only to read service names / image strings. `ListServices`,
  `RawImage` (image exactly as written), `ParseImage` (base + tag, stripping any
  existing `@sha256:` digest; defaults tag to `latest`).
- `PinImage` rewrites the `image:` line via line-based regex (NOT YAML
  re-serialization) so all surrounding formatting and comments are preserved.

### `internal/docker`
Shells out to the `docker` CLI, for the plugins: `Pull` streams `docker pull`;
`GetDigest` extracts the `sha256:...` repo digest of a locally-present image;
`RunningDigest` finds what a service's container is actually running. duva does
not use this — it runs in a container and talks to the daemon's API over the
mounted socket, so its image needs no CLI.

### `oci/registry` (module)
Tag listing and selection for `upgrade`, duva and the v4 watcher.
Version comparison lives beside it in `oci/version`: `Classify` and
`CompareVersions` never touched a registry, and filing them under one left
the v4 queue importing a registry client to answer a question about text. Talks to
registries directly over HTTPS — GHCR, Docker Hub, or any OCI Distribution
registry — discovering bearer auth from the `WWW-Authenticate` challenge.
- `ListMatchingTags` / `MatchingCandidates` keep only version-like tags and
  sort them newest-first by `CompareVersions` (numeric dotted cores, so a
  suffixed build ranks below the bare release).
- `TagCreated` is a tag's publish time, for the `duva.delay` soak: the Docker
  Hub tag API's `tag_last_pushed`, or manifest→config-blob `created` elsewhere.
- `Classify(from, to)` sizes a change as patch/minor/major, or unknown when the
  two tags cannot be compared — which duva treats as major, since a change it
  cannot measure is not one to make unattended.

Every call site shares **one** `http.Client` (`client.go`). A Client holds a
Transport and a Transport holds the connection pool, so a client per call
means no reuse: one connection, and therefore one DNS lookup, per registry
operation. A check across ~45 services is several hundred operations against
three or four hosts, which is what makes a scheduled run look like a flood to
a resolver — diun, which does exactly this, exhausts Pi-hole's default
1000-queries-a-minute allowance in 40 seconds and fails ~140 jobs with
"server misbehaving" (Go's rendering of a REFUSED reply, which reads like the
registry's fault and is not). `MaxIdleConnsPerHost` is raised from the default
2 for the same reason. Held by `TestConnectionsAreReusedAcrossCalls`.

Until 2026-08 this package also resolved a digest back to a version tag, so a
pin could be labelled with the version it matched. That is gone: see "The tag
is the tag to follow".

### `internal/pin`
The shared write engine. `Compute` works out what an image line should become
and `Apply` writes it; `SelectCandidate` picks the upgrade target for a
constrained service, subject to the `duva.delay` soak, and reports what it held
back as too fresh. Both `docker pin` and duva go through it, so they cannot
disagree about what a pinned line looks like.

## Modules

The repo is five modules. Not for its own sake: Go's `internal/` is invisible
across a module boundary, so anything two of them share cannot be internal to
either -- and `docker pin` and duva v4 genuinely do share file-format code and
nothing else.

| module | what | who uses it |
|---|---|---|
| `.` | the plugins, duva, and their support | — |
| `compose/` | reading and rewriting compose files | everything |
| `dockerapi/` | the docker daemon client | duva, the v4 updater |
| `oci/` | registry client + version comparison | `docker pin`, duva, the watcher |
| `duva-v4/` | watcher, queue, updater, ui | — |

Each replaces the others by relative path rather than by version: they are
developed together, and a version would mean tagging a release to change one
line.

`oci` is separate mostly so its coverage is not mixed with the rest. It is the
one part of this repo whose correctness is bounded by somebody else's server
behaving as documented, so its tests exercise retries, auth discovery and
pagination against fakes rather than the logic the rest of the repo is about.
It also means swapping it for a library would be a module replacement rather
than surgery -- measured, and not worth it today: go-containerregistry is
6.9MB and 48 modules for a program that only lists tags, regclient 7.4MB and
20, against 7.4MB for the whole current watcher. Neither is lighter where it
matters, and neither exposes Docker Hub's `tag_last_pushed`, which is a Hub
extension rather than an OCI concept.

### duva v4 (`duva-v4/`)

Three single-purpose processes plus a page, replacing what duva does in one.
Built and working end to end, not deployed. duva itself is untouched and
still ships.
See `docs/duva-v4-watch-queue-update.md`.

- **`watcher/`** — what exists, and when. Lists tags, keeps the ones
  published since the last check, publishes them to a webhook. Knows nothing
  about versions: not semver, not calver, not whether 4.39.25 beats 4.39.20.
  Its only state is one timestamp, so losing it costs a noisy run rather than
  a rebuild. Reads `diun.include_tags` / `diun.exclude_tags`, which ~45
  services already carry.
- **`queue/`** — the gate. Given a notice and what the compose file
  declares: is this an update, how big, and may it be applied unattended.
  Holds the queue, keyed by service so a newer candidate supersedes a pending
  one. Holds no docker socket and talks to no registry.
- **`updater/`** — the reference implementation of the contract: pull, write the
  pin, recreate, commit, push. The only one of the three that depends on
  `docker pin` -- applying an update means rewriting a pin, which is its
  engine.

  **No rollback.** Everything answerable is
  answered in a precheck before the pull, via the same `pin.Compute` the write
  uses. A failed recreate is left alone: recreating stops and removes the old
  container before creating the replacement, so one error covers the old
  container running, gone, or a new one that will not start — and restoring
  the pin is wrong in some of those. The file keeps what was decided, the
  repository is dirty, `IsClean` blocks the next apply, and the reason says so.

  **It must run as the user that owns the repository, and that user must be
  in the `docker` group** (`user: "1000:1000"` plus `group_add`, GID from
  `stat -c '%g' /var/run/docker.sock` — host-specific). Root leaves
  root-owned files in a bind-mounted repository, makes git report dubious
  ownership, and loses the commit identity `.git/config` already carries.
  See `duva-v4/README.md`.
- **`internal/updater`** — the *contract*, and a client. Specified in
  `docs/update-contract.md`, so a different updater can be written without
  reading this one's source. What the queue may
  know is an endpoint, a payload, and that the answer carries a stream URL; it
  may not know that applying involves a registry, a container or git, because
  an updater that opens a pull request touches none of them. Completed means the
  updater did its job, not that the host runs a new image.
- **`internal/detectevent`** — translates the watcher's webhook, so nothing
  else knows which watcher is in use. One shape at a time: a gate sniffing
  between payload formats would carry translators for watchers nobody runs.
- **`ui/`** — the page, over one or more queues. Holds no compose file, no
  docker socket and no registry credentials: it asks queues what is queued
  and asks them to apply what was clicked. Lifted from duva's `internal/ui`,
  minus the soak list (a soak is an entry with a release time, not a second
  list) and the refresh button (the watcher owns when a check happens).
  Queues are configured, not discovered: `DUVA_UI_DECIDERS=host=url,...`,
  with tokens in `DUVA_UI_TOKEN` or per-host `DUVA_UI_TOKEN_<HOST>` — never
  in the list, so the list itself is not a secret. Refuses to start with no
  queues, since "nothing waiting for approval" is the most misleading
  sentence it can print and is also what it says when all is well. An
  unreachable queue renders as unreachable and suppresses that
  reassurance.

Each binary has its own Dockerfile, because each needs a different thing: the
watcher and queue hold no socket and shell out to nothing; the updater holds
the socket and carries git. The build context is the repository root, since
the module replaces its siblings by relative path.

## Command semantics

- **`docker pin <service>`**: no-op if the image is already digest-pinned. Digest
  source, in order: the **running container's** image (`docker.RunningDigest`,
  found via compose's own `com.docker.compose.service` +
  `project.working_dir` labels), else the local image, else a pull. Preferring
  the running container matters because pinning happens *after* a stack has been
  up a while, and in that window something else can re-pull the moving tag —
  pinning the local image would then record a digest that never ran here. When
  the two disagree it prints a "Note:" line. A `runningDigest` error is fatal
  (never silently fall through to a different digest); "no container" and "no
  repo digest" (locally built / pruned) both return empty and fall through.
  Writes `base:tag@sha256:...` with the tag **exactly as written in the compose
  file** — see "The tag is the tag to follow" below.
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

## The tag is the tag to follow

In `image: <name>:<tag>@sha256:<digest>` the **tag is an instruction** — which
stream of releases this service tracks — and the **digest is the record** of
what is actually running. One field cannot be both, so the tag is never
rewritten to describe what was pinned:

- `docker pin <service>` writes the tag back verbatim. A service on `latest`
  stays on `latest`.
- `docker pin upgrade <service>` re-resolves the digest for the tag already in
  the file; the tag is unchanged.
- `docker pin upgrade <service> <version>` is the one operation that *does*
  change the tag, because changing it is what was asked for.

Until 2026-08 both `pin` (via `ResolveOrWarn`) and `upgrade` resolved a moving
tag to whatever concrete version tag carried the same digest, for readability.
That was actively harmful: it converted "track latest" into "pinned to the
1.26 line forever" — a concrete tag never moves, so the service silently
stopped receiving updates the moment it was pinned. Guarded by
`TestPinInFile_KeepsMovingTag` and `hack/e2e-pin.sh` (real registry; the unit
tests fake the digest lookups and so cannot catch a tag rewrite).

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

- **Parse flags, then reject what's left over if it's flag-shaped.** Each
  command consumes its known flags into variables (`--dry-run`/`-n`,
  `--all`/`-a`, `--concurrency`/`-j`) and everything else falls through to a
  `filtered` slice of positional args; `rejectUnknownFlags` then errors on any
  of those starting with `-`. Never sniff `args[0] == "--all"` before checking
  argument counts — that is what let `pin --all --dry-riun` drop the mistyped
  flag and rewrite every compose file for real.
- **`--all` dry-run summaries are sorted by service name** (`sort.Slice` on
  results, `sort.Strings` on failures) in all three tables: `pinAll`,
  `upgradeAll`, unpin's `runAll`. `upgradeAll`'s was otherwise in
  goroutine-completion order, i.e. different every run. Covered by
  `Test*_SummaryIsSorted` in both commands' `main_test.go`, which capture
  stdout via the local `captureStdout` helper.
- New subcommand on `docker pin` = new `case` in `cmd/docker-pin/main.go`'s `main()` switch.
- New top-level plugin = new `cmd/docker-<name>` package; register in `Makefile` `BINARIES`,
  `.goreleaser.yaml` `builds`/`archives`, and the formula `install` line.
- Only the standard library plus `gopkg.in/yaml.v3`. The plugins need the
  `docker` CLI on PATH at runtime; duva does not — it talks to the daemon's API
  over the socket, so its image carries only git, for the commit.
- Base images are pinned by digest, and the released image must be built on the
  same base the integration suites build. The lesson that earned this rule: the
  released v3 image was distroless-static long after duva needed git at
  runtime, so every test passed against an image nobody shipped.
