# docker-pin

Docker CLI plugins to pin container images in a Compose file to an exact tag and SHA digest — and keep them there.

## The problem

`image: postgres:16` is a moving target. Every `docker compose pull` can silently swap the image under you. Pinning to a digest (`image: postgres:16.3@sha256:...`) makes deployments reproducible, but doing it by hand — looking up digests, rewriting lines, updating after upgrades — is friction nobody wants.

## What it does

Two plugins:

- **`docker pin`** — pins a service to the digest it is *actually running* (falling back to the local image, then a pull). The tag is written back verbatim: it is the tag to *follow*, and the digest is the record of what runs, so `latest@sha256:...` stays on `latest`.
- **`docker pin upgrade`** — pulls fresh, then re-pins to the new digest. Same version-tag resolution.
- **`docker unpin`** — strips the digest, leaving just `image: postgres:16`.

All three rewrite the `image:` line in place — formatting, comments, and surrounding YAML are preserved.

## Installation

### Homebrew

```bash
brew tap Miista/homebrew-docker-pin
brew install docker-pin
```

Then add the Homebrew lib path to Docker's plugin search in `~/.docker/config.json`:

```json
{
  "cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]
}
```

Replace `/opt/homebrew` with your `HOMEBREW_PREFIX` (`brew --prefix`). On Intel Macs it's `/usr/local`.

### Debian / Ubuntu (apt)

The tools are published to a signed [Cloudsmith](https://cloudsmith.io) apt
repository (`guldmund/stable`). One-time setup:

```bash
sudo install -d /usr/share/keyrings
curl -1sLf https://dl.cloudsmith.io/public/guldmund/stable/gpg.key \
  | sudo gpg --dearmor -o /usr/share/keyrings/guldmund-stable-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/guldmund-stable-archive-keyring.gpg] https://dl.cloudsmith.io/public/guldmund/stable/deb/debian any-version main" \
  | sudo tee /etc/apt/sources.list.d/guldmund-stable.list
sudo apt update && sudo apt install docker-pin
```

The repo is distro-agnostic (`debian any-version`), so the same line works on
any Debian/Raspberry Pi OS/Ubuntu release.

The plugins are installed into `/usr/libexec/docker/cli-plugins`, which Docker
scans by default — no extra configuration needed.

### Manual

Download the binaries for your platform from the [releases page](https://github.com/Miista/homebrew-docker-pin/releases), then install them:

```bash
mkdir -p ~/.docker/cli-plugins
install -m 755 docker-pin docker-unpin ~/.docker/cli-plugins/
```

## Usage

### Pin a service

```bash
docker pin <service>
docker pin --all
```

No-op if the service is already pinned.

Before:
```yaml
services:
  db:
    image: postgres:16
```

After:
```yaml
services:
  db:
    image: postgres:16@sha256:a3dc6b...
```

#### Which digest gets pinned

The intended workflow is: bring the stack up, live with it, decide this is what
you want, *then* pin. So `pin` records **what is actually running**, in this
order:

1. **The running container's image** — if a container for the service is up.
2. **The local image** for the tag — if the service isn't running.
3. **A pull** — only if the image isn't present locally at all (first pin on a
   fresh host, after a prune, or a service that runs on another box).

The order matters because of the gap between "up" and "pinned". In that window
something else may re-pull the moving tag — a sibling service on the same base
image, a build, a manual `docker pull` — so the local image for `nginx:latest`
can be *newer* than the container is running. Pinning that would record a
digest that has never run on this host. When the two disagree, `pin` says so
and pins the running one:

```
Using digest from running container: sha256:41b1944...
Note: nginx:latest now resolves to sha256:b34848e... locally — pinning what is running, not that.
```

To move to what the tag points at now, that is what `upgrade` is for.

The tag is kept exactly as written — it is the tag to **follow**, while the
digest records what is actually running. A service on `latest` stays on
`latest` and keeps tracking it:

```yaml
  web:
    image: nginx:latest@sha256:...
```

Only `docker pin upgrade <service> <version>` changes the tag, because that is
what it was asked to do.

### Upgrade a service

```bash
docker pin upgrade <service>
docker pin upgrade <service> <version>
docker pin upgrade --all
```

Always pulls, then re-pins to the freshly pulled digest. With no version, the plugin derives the moving tag from the current pin (e.g. if you're on `16.3`, it pulls `16` — the line/variant you're on — not blindly `latest`).

```bash
# Pull latest on the current line and re-pin
docker pin upgrade db

# Jump to a specific version
docker pin upgrade db 17
```

`--all` cannot be combined with an explicit version.

This is also the command to run when a tag moved upstream and you want to follow it — `upgrade` re-pulls the tag and re-pins to the new digest. There's no need to `docker pull` by hand first (upgrade pulls for you), and a pinned service never drifts to a moved tag on its own: `docker compose up` fetches strictly by the pinned digest.

### List services and their pin status

```bash
docker pin list             # table: service, image, tag, digest, pinned
docker pin list --missing   # only unpinned services; exits non-zero if any
docker pin list -q          # names only (for scripting)
```

```
SERVICE  IMAGE               TAG     DIGEST        PINNED
db       postgres            16.3    a3dc6bd4a4a5  ✓
plex     plexinc/pms-docker  latest  -             ✗
```

`--missing` is CI-friendly: it prints only unpinned services and exits `1`
when any exist, so a single step enforces "everything in this repo is pinned":

```yaml
- name: All images must be pinned
  run: docker pin list --missing
```

Read-only — parses the compose file, never touches Docker or the network.

### Shell completion

Both plugins implement the completion protocol the Docker CLI (v25+) uses to
delegate `docker pin <TAB>` to plugins: subcommands, flags, and the service
names from the nearest compose file all complete. It requires docker's own
completion v2 to be installed for your shell, e.g. for zsh:

```sh
mkdir -p ~/.docker/completions
docker completion zsh > ~/.docker/completions/_docker
# ensure fpath includes ~/.docker/completions before compinit in ~/.zshrc
```

### Unpin a service

```bash
docker unpin <service>
docker unpin --all
```

Strips the digest, keeping the tag:

```yaml
# before
image: postgres:16.3@sha256:a3dc6b...

# after
image: postgres:16.3
```

No-op if the service isn't pinned.

## duva — the update companion

**duva** (Swedish for dove — a carrier pigeon: flies to the registry and comes
back) watches **pinned** services in a compose project (`image:` has
`@sha256:...`) and either applies an update or queues it for a person,
according to that service's `duva.auto` policy. Applying means: pull the
image, rewrite the pin, recreate the container, commit the change.

Being pinned **is** the opt-in: `docker pin <service>` starts duva watching
it, `docker unpin <service>` stops it. A service without a digest is ignored.
This is what duva is for — telling you when a deliberate version pin has gone
stale, not generic "is anything newer" drift-watching.

duva ships as **four images**, one per stage:

    watch  ->  queue  ->  update
                 ^
                 |
                 ui

| stage | what it does | docker socket | /compose | git |
|---|---|---|---|---|
| **watch** (`ghcr.io/miista/duva-watch`) | what tags exist, and when | — | read | — |
| **queue** (`ghcr.io/miista/duva-queue`) | is this an update, how big, may it be applied | — | read | — |
| **update** (`ghcr.io/miista/duva-update`) | pull, write the pin, recreate, commit | read-write | **read-write** | yes |
| **ui** (`ghcr.io/miista/duva-ui`) | the approval queue, over one or more hosts | — | — | — |

Only update holds any privilege, and that is the point of the split: the
network-facing half needs none of what the work needs. Watch, queue and update
each run on the host they touch, since each reads that host's compose project
and — for update — its docker socket and its repository. Only the UI is
host-agnostic, serving as many hosts' queues as it is given.

These version independently of the CLI, under their own `duva-v4/vX.Y` tags.
See `duva-v4/README.md` for the full deployment contract, including why the
updater refuses to run as root.

### Per-service labels

Rules live on the service, next to the pin they govern:

```yaml
services:
  radarr:
    image: ghcr.io/linuxserver/radarr:latest@sha256:...
    labels:
      diun.include_tags: '^\d+\.\d+\.\d+$'  # only consider tags matching this
      diun.exclude_tags: '(alpha|beta|rc)'  # drop matching candidates
      duva.auto: digest                     # what may be applied unattended
```

**Tag filtering reads diun's labels, not duva's.** These are detection rules
and the watcher is what reads them; around forty-five services already carry
them because diun was the watcher here for a long time, and a second namespace
saying the same thing would mean a migration that changes nothing. Policy —
what may be applied unattended — is a different question and lives on
`duva.auto`.

`duva.auto` decides what may be applied without asking: `none` (the default),
`digest`, `patch`, `minor` or `major`. A service with no `duva.auto` is only
ever queued for a person.

`digest` is not a rung on that ladder but a different mode. `patch`, `minor`
and `major` size a *version step* — the tag itself changing — and each also
implies taking a digest move on the tag already followed. `digest` takes only
the latter: what `latest` now points at, or an upstream republishing `1.4.2`
with a patched base layer, without also consenting to be walked up to `1.4.3`.
A digest move has no magnitude to threshold on, which is why it is a mode
rather than the smallest step.

An unrecognised value is an error rather than a fallback: `duva.auto: pathc`
silently meaning "none" is the kind of thing discovered months later, when
something has not been happening and nobody knows why — and the opposite
mistake, a typo that quietly widened what may be applied unattended, is worse
still.

### Configuration

Each stage is configured by env vars; there is no config file.

**watch** — `DUVA_SCHEDULE` (cron expression), `DUVA_WEBHOOK_URL` (where to
publish notices), `DUVA_HOST`, `DUVA_SINCE`, `DUVA_LOG_LEVEL`. Mounts the
project read-only at `/compose` and keeps one timestamp in `/data`. Its only
state is that timestamp, so losing it costs a noisy run rather than a rebuild.
`serve` checks on the schedule and stays up between checks — restarting it is
how you ask for a check now.

**queue** — `DUVA_UPDATE_URL`/`DUVA_UPDATE_TOKEN` (the updater it hands work
to), `DUVA_QUEUE_TOKEN` (what a UI must present), `DUVA_NTFY_ENDPOINT`/
`DUVA_NTFY_TOPIC`/`DUVA_NTFY_CLICK` for notifications, `DUVA_UPDATE_TIMEOUT`,
and `DUVA_RECONCILE_INTERVAL` (how often a queued entry is re-checked against
the compose file, to catch one satisfied by something other than its own
apply; default 10m). Mounts the project read-only at `/compose` and keeps the
queue in `/data` — without that volume a deploy drops every decision waiting
on a person, and announces each one again when the watcher re-sends it.

**update** — `DUVA_REPO` (the repository, at the path it has on the host),
`DUVA_COMMIT_TEMPLATE`, `DUVA_GIT_PUSH` (off by default: commits stay local),
`DUVA_UPDATE_TOKEN`. Mounts the repository read-write at its own host path,
plus the docker socket.

**ui** — `DUVA_QUEUES` (`host=url,host=url`), `DUVA_QUEUE_TOKEN_<HOST>` per
host, and `DUVA_READ_ONLY` to serve the page without the Update button. Tokens
are never in the queue list, so the list itself is not a secret. It refuses to
start with no queues: "nothing waiting for approval" is the most misleading
sentence it can print, and is also what it says when all is well.

### What the mounts are for

- **`/var/run/docker.sock`** (update only) — pulling images and recreating
  containers means driving the host's daemon. This is real access to the host.
  `:ro` on a socket applies to the file, not the API served over it: a
  container holding it can still create and delete containers either way.
- **`/compose`** — the compose project *directory*, never the file alone. Two
  things break with a single-file mount: `include:`'d nested compose files
  resolve relative to the directory and would not exist inside the container,
  and a single-file bind silently pins the old inode when the host file is
  replaced by rename — which is exactly how editors and `docker pin` itself
  rewrite it, so the stage would read a stale file forever without any error.
  Read-only for watch and queue; read-write for update, because rewriting the
  pin is the whole point.
- **`/data`** (watch, queue) — a named volume, not a bind. Docker chowns a
  fresh named volume to match what the image carries; a bind mount would be
  root-owned and the stage would fail on its final write, having done all the
  work.

Because update commits, it will not act on a dirty repository — committing on
top of someone's half-finished edit is never wanted. It is a precondition, not
a setting.

**No rollback.** Everything answerable is answered in a precheck before the
pull. A failed recreate is left alone: recreating stops and removes the old
container before creating the replacement, so one error covers the old
container running, gone, or a new one that will not start — and restoring the
pin is wrong in some of those. The file keeps what was decided, the repository
is dirty, and the next apply is blocked with the reason.

## How digests work (multi-arch)

`docker image inspect` returns the **index digest** — the hash of the multi-arch manifest list, not a per-platform image digest. This is intentional:

- Pinning the index digest keeps the pin portable across architectures. A per-arch digest would lock you to one platform.
- Two tags can point to byte-identical `linux/amd64` images but have different index digests if their manifest lists carry different platform sets (e.g. `latest` includes Windows images, `alpine` does not). The "already up to date" check will therefore treat them as different — this is correct, not a bug.
- The digest shown in the Docker Hub "Digest" column is the per-arch manifest digest and may differ from the index digest recorded by this tool. This is expected.

## The tag is the tag to follow

In `image: <name>:<tag>@sha256:<digest>`, the **tag is an instruction** — which
stream of releases this service tracks — and the **digest is the record** of
what is actually running. One field cannot be both, so the tag is never
rewritten to describe what got pinned:

| Command | Tag |
|---|---|
| `docker pin <service>` | written back verbatim |
| `docker pin upgrade <service>` | unchanged; only the digest moves |
| `docker pin upgrade <service> <version>` | becomes `<version>` — the one operation that changes it, because that is what was asked |

Earlier versions resolved a moving tag to whatever concrete version tag carried
the same digest, so that the file read `radarr:5.28.1@sha256:…` instead of
`radarr:latest@sha256:…`. That was actively harmful: a concrete tag never
moves, so the service silently stopped receiving updates the moment it was
pinned. If you have services pinned by an older version, check for ones whose
tag you did not choose yourself.

The registry tag-listing code is still used to find upgrade candidates
(`docker pin upgrade` and duva); it just no longer decides what
tag a pin is written under. Supported registries: **Docker Hub**, **GHCR**, and
any **OCI-compliant registry** (bearer auth discovered via the
`WWW-Authenticate` challenge).

## Unknown flags are rejected

Every command parses the flags it knows and treats anything else that looks
like a flag as an error, rather than silently passing it through as a
positional argument:

```
$ docker pin --all --dry-riun
Error: unknown flag "--dry-riun"
Usage: docker pin <service>
       docker pin --all [--dry-run]
```

This matters because `--all` combined with a mistyped `--dry-run` would
otherwise run for real against every service. A typo in a safety flag must
never become a live run.

## Dry-run summaries

`--dry-run` on any `--all` command prints a summary table instead of changing
anything. Rows are sorted alphabetically by service, so the output is stable
between runs and easy to diff:

```
$ docker pin --all --dry-run
Summary:
SERVICE  ACTION  SHA
alpha    pin     sha256:...
mango    none    sha256:...
web      pin     sha256:...
```

`ACTION` is `pin`/`upgrade`/`unpin` for services that would change, `none` for
those already in the desired state, and `FAILED` for ones that errored.

### GHCR/OCI tag listing cost

Docker Hub's tag API sorts by push time and lets this tool ask for that order
explicitly, so a "is there anything newer" check can stop as soon as it finds
a qualifying tag — most checks need only the first page.

GHCR and generic OCI registries have no such option. The OCI Distribution
Spec's `tags/list` endpoint (`GET /v2/<name>/tags/list`) mandates **lexical
(ASCIIbetical) ordering with no sort parameter**, confirmed directly against
`ghcr.io`: no `order`/`sort`/`orderby` query parameter changes the response.
Lexical order is not numeric order — `"2.10.0"` sorts *before* `"2.9.0"`,
since `'1' < '9'` at the first differing character — so a tag doesn't have to
be at the end of the list just because it's the newest release. That also
rules out seeding the endpoint's `last=` pagination cursor at the currently
pinned tag as a shortcut: any newer release whose version number grew a digit
(`9` → `10`) would already sort *before* that cursor and be skipped.

The endpoint paginates 100 tags per response via a `Link: rel="next"` header,
and there is no way to know a page is the last one except by requesting it
and finding no `Link` header at all — no total count, no `rel="last"`. This
tool therefore walks every page to get a complete, correct list, with no
early exit. For a tag-heavy image this is real cost: `ghcr.io/home-assistant/home-assistant`
currently has ~4,400 tags, meaning ~45 requests per check for that one image.
There is currently no cap or warning on this walk.

## Release & distribution

This repo is the Homebrew tap. Pushing a `vX.Y.Z` tag triggers a GoReleaser workflow that:

1. Builds `docker-pin` and `docker-unpin` for `linux/darwin` × `amd64/arm64`
2. Creates a GitHub release with archives, `.deb` packages, and a checksum file
3. Commits an updated `Formula/docker-pin.rb` back to this repo
4. Publishes the `.deb`s to the shared [Cloudsmith](https://cloudsmith.io) apt
   repository (`guldmund/stable`), which indexes and signs them server-side.
   Each tool pushes only its own artifacts — there is no shared build step.

duva's four images version independently, under `duva-v4/vX.Y` tags handled by
`.github/workflows/duva-v4-release.yml` and its own `.goreleaser.duva-v4.yaml`.
A queue fix should not wait on a CLI release, and a CLI release should not
force a rebuild of four images that did not change — so the tag namespaces are
deliberately separate, and the `duva-v4/` prefix is what keeps these tags out
of the CLI workflow's `v*` trigger.

## License

MIT
