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
Tag listing and selection for `upgrade`, duva and the v4 detector.
Version comparison lives beside it in `oci/version`: `Classify` and
`CompareVersions` never touched a registry, and filing them under one left
the v4 decider importing a registry client to answer a question about text. Talks to
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

Until 2026-08 this package also resolved a digest back to a version tag, so a
pin could be labelled with the version it matched. That is gone: see "The tag
is the tag to follow".

### `internal/pin`
The shared write engine. `Compute` works out what an image line should become
and `Apply` writes it; `SelectCandidate` picks the upgrade target for a
constrained service, subject to the `duva.delay` soak, and reports what it held
back as too fresh. Both `docker pin` and duva go through it, so they cannot
disagree about what a pinned line looks like.

### `internal/watch`
duva's detection and policy. `Check` turns a compose file into findings;
`Decide` says whether `duva.auto` allows duva to apply one itself; `State`
records what is queued for approval, what `duva.delay` is holding back, and
what has already been announced, so nothing is reported twice.

### `internal/ui`
duva's approval queue: one page listing what needs a human and what is soaking,
with a button that applies through the same path an unattended run uses.

It depends on three interfaces — `Source` (the rows), `Applier` (start/poll an
update), `Refresher` (start/poll a check) — which is what lets the same page
serve a local duva or a hub over remote agents without knowing which.

### `internal/agent`
The wire between a hub and an agent: payload types, the JSON `Server` that
exposes one host's duva, and the `Client` a hub collects with. The server is
the same three `internal/ui` interfaces with an HTTP encoding instead of an
HTML one, so an agent decides nothing a single-process duva would not.

`/healthz` is open; everything else is behind `DUVA_AGENT_TOKEN` when set.
`/apply` is the authority to pull an image and recreate a container as root on
that host, so an agent without a token is only safe where every host on the
network is trusted to root every other.

### `internal/hub`
Fans out over configured agents and implements the same three interfaces, so
the page cannot tell it is not talking to a local duva. Rows are labelled with
the **configured** host (not the name the agent reports) because that is what
applies route on. Reads serve a short-lived cache and re-collect when it is
older than `MaxAge`; an agent that cannot be reached is reported through
`OnUnreachable` on every collection, never silently dropped — a queue missing
a host looks exactly like that host having nothing to do.

Agents are configured, not discovered: at this scale a registration protocol
would only add an inbound path by which something could claim to be an agent.

## Modules

The repo is five modules. Not for its own sake: Go's `internal/` is invisible
across a module boundary, so anything two of them share cannot be internal to
either -- and `docker pin` and duva v4 genuinely do share file-format code and
nothing else.

| module | what | who uses it |
|---|---|---|
| `.` | the plugins, duva, and their support | — |
| `compose/` | reading and rewriting compose files | everything |
| `dockerapi/` | the docker daemon client | duva, the v4 actor |
| `oci/` | registry client + version comparison | `docker pin`, duva, the detector |
| `duva-v4/` | detector, decider, actor, ui | — |

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
20, against 7.4MB for the whole current detector. Neither is lighter where it
matters, and neither exposes Docker Hub's `tag_last_pushed`, which is a Hub
extension rather than an OCI concept.

### duva v4 (`duva-v4/`)

Three single-purpose processes plus a page, replacing what duva does in one.
Built and working end to end, not deployed. duva itself is untouched and
still ships.
See `docs/duva-v4-diun-decider-actor.md`.

- **`detector/`** — what exists, and when. Lists tags, keeps the ones
  published since the last check, publishes them to a webhook. Knows nothing
  about versions: not semver, not calver, not whether 4.39.25 beats 4.39.20.
  Its only state is one timestamp, so losing it costs a noisy run rather than
  a rebuild. Reads `diun.include_tags` / `diun.exclude_tags`, which ~45
  services already carry.
- **`decider/`** — the gate. Given a notice and what the compose file
  declares: is this an update, how big, and may it be applied unattended.
  Holds the queue, keyed by service so a newer candidate supersedes a pending
  one. Holds no docker socket and talks to no registry.
- **`actor/`** — the reference implementation of the contract: pull, write the
  pin, recreate, commit, push. The only one of the three that depends on
  `docker pin` -- applying an update means rewriting a pin, which is its
  engine.
- **`internal/actor`** — the *contract*, and a client. What the decider may
  know is an endpoint, a payload, and that the answer carries a stream URL; it
  may not know that applying involves a registry, a container or git, because
  an actor that opens a pull request touches none of them. Completed means the
  actor did its job, not that the host runs a new image.
- **`internal/detectevent`** — translates the detector's webhook, so nothing
  else knows which detector is in use. One shape at a time: a gate sniffing
  between payload formats would carry translators for detectors nobody runs.
- **`ui/`** — the page, over one or more deciders. Holds no compose file, no
  docker socket and no registry credentials: it asks deciders what is queued
  and asks them to apply what was clicked. Lifted from duva's `internal/ui`,
  minus the soak list (a soak is an entry with a release time, not a second
  list) and the refresh button (the detector owns when a check happens).
  Deciders are configured, not discovered: `DUVA_UI_DECIDERS=host=url,...`,
  with tokens in `DUVA_UI_TOKEN` or per-host `DUVA_UI_TOKEN_<HOST>` — never
  in the list, so the list itself is not a secret. Refuses to start with no
  deciders, since "nothing waiting for approval" is the most misleading
  sentence it can print and is also what it says when all is well. An
  unreachable decider renders as unreachable and suppresses that
  reassurance.

Each binary has its own Dockerfile, because each needs a different thing: the
detector and decider hold no socket and shell out to nothing; the actor holds
the socket and carries git. The build context is the repository root, since
the module replaces its siblings by relative path.

### duva (`cmd/duva`)
A container, not a CLI plugin. It watches the compose project mounted at
`/compose` and records state in `/data/duva.json` — both fixed, because duva
runs in a container where they are the contract.

`DUVA_SCHEDULE` is a 5-field cron expression, parsed by `internal/croncal`,
which rejects restricting both day-of-month and day-of-week: cron ORs those,
systemd ANDs them, and a schedule that means two different things depending on
who reads it is not a schedule.

`DUVA_MODE` selects which half this process is:

- `local` (default, and what duva has always been) — watches this host and
  serves its own page. An install that was never configured keeps working.
- `agent` — watches this host, serves JSON for a hub, no page. Holds exactly
  what a local duva holds: the docker socket, the repository, the compose
  files. The split is not a privilege reduction on this side.
- `hub` — serves the page over `DUVA_AGENTS` (`host=url,...`). Watches
  nothing, and needs neither the docker socket nor the compose files; that
  the network-facing half needs none of the privilege the work needs is the
  point of the split. Refuses to start with no agents, since an empty queue
  would read as "nothing to do".

Each agent keeps its own state and its own schedule, so one is useful on its
own and a hub is purely additive. Two agents committing to one repository is
what `PullRebase` before push already exists for.

**It must be a service in the stack it watches.** duva reads its own
container's compose labels to learn which project it is in; outside one there
are no labels and it refuses to act rather than guess. With a root compose file
that `include:`s the rest, one duva covers everything.

Applying is a transaction in `apply.go`, with `transaction.go` deciding what a
failure means: pull → write the pin → recreate the container → commit → push.
There is no rollback except one case — if the container refuses the new image,
the file is put back, because leaving a claim there would be a lie. Everything
after the container is record-keeping and cannot make the update untrue.

Recreating goes through the docker API (`recreate.go`), not `docker compose
up`. duva sees the compose file at its own mount point while the daemon sees
the stack at the host's path, and one `--project-directory` cannot satisfy both
kinds of relative path: a bind must resolve to a host path, an `include:` to a
path inside duva. A running container's configuration is already resolved, so
swapping its image needs no path interpreted at all. Anonymous volumes are
carried across explicitly — they exist only in the container's `Mounts`, and a
replacement built from `Config` alone would come up healthy and empty.

Per-service policy lives as labels: `duva.include_tags` / `duva.exclude_tags` constrain
which tags qualify, `duva.delay` soaks a release before adopting it, and
`duva.auto` (none/patch/minor/major, default none) says what duva may apply
unattended. An unknown label is an error, not something ignored.


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
  same base the integration suites build. Both are held by tests in
  `cmd/duva/dockerfile_test.go`: the released image was distroless-static long
  after duva needed git at runtime, so every test passed against an image
  nobody shipped.
