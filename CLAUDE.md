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

Five modules, because Go's `internal/` is invisible across a module boundary
and `docker pin` and duva v4 share file-format code and nothing else. They
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

v4 reads exactly one `duva.*` label, `duva.auto`. There is **no soak**: v3's
`duva.delay` has no equivalent, and `pin.SelectCandidate` is not on this path.

- **watcher** knows nothing about versions: not semver, not calver. Its only
  state is one timestamp, so losing it costs a noisy run, not a rebuild.
- **queue** holds no docker socket and talks to no registry.
- **updater** is the only one that depends on `docker pin`.
  - **No rollback.** Everything answerable is answered in a precheck before the
    pull. A failed recreate is left alone: recreating stops and removes the old
    container before creating the replacement, so one error covers the old
    container running, gone, or a new one that will not start, and restoring
    the pin is wrong in some of those. `IsClean` then blocks the next apply.
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

Each binary has its own Dockerfile because each needs different things; the
build context is the repo root since modules replace siblings by relative path.

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
