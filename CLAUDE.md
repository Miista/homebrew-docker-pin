# CLAUDE.md

Two Docker CLI plugins, `docker pin` and `docker unpin`, and **duva**, which
watches pinned services and applies the updates each one's policy allows. The
repo is also the Homebrew tap it ships through, so the module path is
`github.com/Miista/homebrew-docker-pin`.

Commands are in the `Makefile`. Integration tests (`-tags integration`) run
against a local registry and daemon and contact nothing upstream, so they work
offline; `go run ./test/sandbox <suite>/<scenario>` stands one scenario up by
hand.

## Modules

Six modules, because Go's `internal/` is invisible across a module boundary
and `docker pin` and duva share file-format code and nothing else. duva v4 and
v5 are a module each, side by side until v5 replaces v4 on both hosts, as v4
did v3. They
replace each other by relative path, not version: a version would mean tagging
a release to change one line.

`oci` is separate so its coverage is not mixed with the rest: its correctness
is bounded by somebody else's server behaving as documented. Replacing it with
a library was measured and is not worth it: go-containerregistry is 6.9MB and
48 modules, regclient 7.4MB and 20, against 7.4MB for the whole watcher, and
neither exposes Docker Hub's `tag_last_pushed`.

`oci/version` is split from `oci/registry` because `Classify` and
`CompareVersions` answer questions about text, and filing them under the
registry client made the v4 queue import one.

## Registry client

Every call site shares **one** `http.Client`. A client per call means no
connection reuse and one DNS lookup per registry operation; a check across ~45
services then looks like a flood to a resolver. diun does exactly this and
exhausts Pi-hole's 1000-queries-a-minute default in 40 seconds, failing ~140
jobs with "server misbehaving" (Go's rendering of a REFUSED reply, which reads
like the registry's fault and is not). Held by
`TestConnectionsAreReusedAcrossCalls`.

`Classify` returns unknown when two tags cannot be compared, and duva treats
that as major: a change it cannot measure is not one to make unattended.

## duva v4

Released under its own `duva-v4/vX.Y` tags, separate from the CLI's `vX.Y.Z`.
See `docs/duva-v4-watch-queue-update.md`.

v4 reads two `duva.*` labels: `duva.auto` (how big a change applies
unattended) and `duva.soak` (how old a release must be first; per service, no
host default). A soaking release is an ordinary queue entry with
`ReleaseAt` set, released by the queue's reconcile tick, not a separate list.
The soak counts from the watcher's publish time, so the build cache's
`Created` is what the integration suite's soak test controls for.
`pin.SelectCandidate` is not on this path.

- **watcher** knows nothing about versions: not semver, not calver. Its only
  state is one timestamp, so losing it costs a noisy run, not a rebuild.
- **queue** holds no docker socket and talks to no registry.
- **updater** is the only one that depends on `docker pin`.
  - **No rollback.** Everything answerable is answered in a precheck before the
    pull. A failed recreate is left alone: recreating stops and removes the old
    container before creating the replacement, so one error covers the old
    container running, gone, or a new one that will not start, and restoring
    the pin is wrong in some of those. `IsClean` then blocks the next apply.
  - **Never recreates its own container.** An update naming the updater is
    handed to a one-shot `update self-update` helper copied from its own
    container; recreating in place stops the process mid-apply and leaves the
    host with no updater (both hosts, 2026-10-14). Held by
    `TestV4TheUpdaterUpdatesItself`.
  - **Must run as the user that owns the repository, in the `docker` group**
    (GID is host-specific). Root leaves root-owned files in the bind mount,
    makes git report dubious ownership, and loses the commit identity in
    `.git/config`. See `duva-v4/README.md`.
- **`internal/updater`** is the contract (`docs/update-contract.md`). The queue
  may not know that applying involves a registry, a container or git, because
  an updater that opens a pull request touches none of them. `completed` means
  the updater did its job, not that the host runs a new image.
- **`internal/detectevent`** supports one webhook shape at a time: a gate that
  sniffs between formats carries translators for watchers nobody runs.
- **ui** holds no compose file, socket or registry credentials. Queues are
  configured, not discovered, with tokens in per-host `DUVA_QUEUE_TOKEN_<HOST>`
  and never in the list, so the list is not a secret. It refuses to start with
  no queues, because "nothing waiting for approval" is the most misleading
  sentence it can print. An unreachable queue suppresses that reassurance.
  Its Update button is the authority to replace a container, so the whole
  handler sits behind the `authy` sign-in gate (`DUVA_UI_OIDC_*`, or a local
  account; `DUVA_UI_AUTH_DISABLED` turns it off); only `/healthz` is public.
  `ui/gate_test.go` pins that nothing is approved without a session. The gate
  needs a writable `/app/data` and is why `duva-v4/go.mod` is on Go 1.26.

Each binary has its own Dockerfile because each needs different things; the
build context is the repo root since modules replace siblings by relative path.

## duva v5

v4's components in one process: `duva serve`. Released under `duva-v5/vX.Y`
tags as one image, `ghcr.io/miista/duva`. See `docs/duva-v5-one-binary.md`
and `duva-v5/README.md`.

- **The same responsibilities, enforced by packages instead of processes.**
  `watcher`, `gate` (the queue), `updater` and `webui` are the v4 binaries as
  packages; `cmd/duva` connects them. The watcher hands findings to
  `Gate.Handle`, the queue applies through `updater.Server.Local()`, and the
  page reads its own host's queue through `ui.LocalQueue`. The rules above
  about what watch, queue and updater may know still hold.
- **Nothing inside a host talks over a network.** No `/v1/notify`, no updater
  HTTP API, no internal URLs or tokens. Hosts are peers: the API on 8080
  exists only for the page on another host; the page is on 8081, apart, because the page is
  published and the API is not. The self-update helper still serves its
  progress over HTTP, read with `update.WatchURL`.
- **Policy is read from the running container's labels** (`dockerapi.Labels`,
  by `container_name`), not the compose file: a label edited but not
  deployed has not taken effect. The pin still comes from the file. This is
  only safe because recreate is the API clone, which takes nothing but the
  image from the file; `compose up -d <service>` would deploy undeployed
  edits along with it. Held by `TestV5PolicyComesFromTheRunningContainer`.
- **Auth is OIDC, always.** The page refuses to start without all four
  `ui.oidc.*` settings, and v4's `DUVA_UI_AUTH_DISABLED`,
  `DUVA_UI_LOCAL_ENABLED` and `DUVA_UI_AUTH_PUBLIC_PATHS` are refused by name
  (`settings.Gone`). The API refuses to start without `api_token`. The page runs in the process
  holding the docker socket; that risk was accepted (see the design note).
- **Each component recovers from its own panic**; three in ten minutes takes
  the process down. Container events poke the queue's reconcile; the tick is
  the fallback.
- **Self-update needs nothing new:** the replacement's first reconcile drops
  the entry the helper's commit already satisfied.
- **State is one SQLite database**, `/data/duva.db` (`internal/store`,
  `modernc.org/sqlite`, numbered embedded migrations; never edit a released
  one). The queue is a row per release with a status and a history
  (`queue_events`): a delayed release (`duva.delay`; "soak" in the code) keeps
  its own clock, a release for a
  person supersedes older ones for a person, and applying supersedes
  everything older. The watcher's cutoffs and held findings are its own
  tables, which nothing references. `services` mirrors the host's
  containers, disabled never deleted, gone only when missing twice running
  (a recreate is not a removal).
- **The blocklist is by index digest, per image, never by tag**, and is
  checked twice: at enqueue where the digest is known, and by the updater
  after the pull, which is the only point a version change's digest is
  certain. The contract's `blocked` status is that refusal. Held by
  `TestV5ABlockedReleaseIsNeverApplied`.
- **Every setting goes through configy** (`internal/settings`), from the
  environment only: `DUVA_SETTING_<KEY>`, else the default; an invalid value
  is an error. No exceptions, the repo, OIDC and tokens included. There is no
  stored fallback (0004 dropped the table): nothing edits settings from the
  page, so a store would be a second, invisible source, and would put
  secrets in the database. Peers are `peers` (`host=url,...`) with
  `peer.<host>.token` each. An old name refuses to start
  (`settings.CheckRenamed`). configy needs Go 1.27.
- An image that declares `VOLUME` broke two things in `dockerapi`, both fixed:
  `containerID` must match `/containers/<id>/`, not any 64-hex string, and
  `createSpec` must not bind what compose already put in `HostConfig.Mounts`.

## `docker pin` semantics

- **`pin` takes its digest from the running container first**, then the local
  image, then a pull. Pinning happens after a stack has been up a while, and in
  that window something else can re-pull the moving tag, so the local image
  could record a digest that never ran here. A `runningDigest` error is fatal:
  never silently fall through to a different digest. "No container" and "no
  repo digest" (locally built, pruned) fall through.
- **`upgrade` always pulls**, then pins under the same tag it pulled. It never
  relabels the tag from the digest.
- **`--all` plus a write**: pulls run concurrently, compose-file writes run
  sequentially so nothing races.

## The tag is the tag to follow

In `image: <name>:<tag>@sha256:<digest>` the **tag is an instruction** (which
stream this service tracks) and the **digest is the record** of what runs. One
field cannot be both, so the tag is never rewritten to describe what was
pinned. Only `docker pin upgrade <service> <version>` changes it, because that
is what was asked for.

Until 2026-08 pin and upgrade resolved a moving tag to a concrete version tag
with the same digest. That converted "track latest" into "pinned to 1.26
forever" and the service silently stopped updating. Guarded by
`TestPinInFile_KeepsMovingTag` and `hack/e2e-pin.sh`, which uses a real
registry; the unit tests fake digest lookups and cannot catch a tag rewrite.

## Index vs manifest digests (multi-arch)

`docker image inspect` returns the **index** (manifest-list) digest. Pinning it
is correct: it stays portable across architectures, where a per-arch digest
would lock to one. Consequences that are expected, not bugs:

- Two tags with byte-identical amd64 images can have different index digests
  (`latest` carries Windows images, `alpine` does not), so "already up to date"
  will not treat them as the same.
- Docker Hub's "Digest" column shows the per-arch manifest digest, which differs
  from the index digest recorded here.

## Conventions

- **Parse flags, then reject leftovers that are flag-shaped.** Never sniff
  `args[0] == "--all"` before checking argument counts: that is what let
  `pin --all --dry-riun` drop the mistyped flag and rewrite every compose file
  for real.
- New top-level plugin: new `cmd/docker-<name>`, registered in `Makefile`
  `BINARIES`, `.goreleaser.yaml` `builds`/`archives`, and the formula `install`
  line.
- Only the standard library plus `gopkg.in/yaml.v3`. The plugins need the
  `docker` CLI at runtime; duva does not, so its image carries only git.
- Base images are pinned by digest, and the released image must be built on the
  same base the integration suites build. The released v3 image stayed
  distroless-static long after duva needed git, so every test passed against an
  image nobody shipped.
- The formula installs into `#{HOMEBREW_PREFIX}/lib/docker/cli-plugins`, which
  is not a default Docker plugin dir, hence the `cliPluginsExtraDirs` caveat.
