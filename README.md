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

This repo also ships **duva** (Swedish for dove — a carrier pigeon: flies to
the registry and comes back) as a container image: `ghcr.io/miista/duva`. It
watches **pinned** services in a compose project (`image:` has `@sha256:...`),
and for each one either applies the update or queues it for a human, according
to that service's `duva.auto` policy. Applying means: pull the image, rewrite
the pin, recreate the container, commit the change. What it cannot apply it
reports once — over ntfy and in a small web queue — and remembers, so it never
repeats itself.

Being pinned **is** the opt-in: `docker pin <service>` starts duva watching
it, `docker unpin <service>` stops it. A service without a digest is logged
as skipped and otherwise ignored — no notification, no state tracking. This
is what duva is actually for: telling you when a deliberate version pin has
gone stale, not generic "is anything newer" drift-watching.

Per-service rules live as labels on the service, right next to the pin they
govern:

```yaml
services:
  radarr:
    image: ghcr.io/linuxserver/radarr:latest@sha256:...
    labels:
      duva.include_tags: '^\d+\.\d+\.\d+$' # only consider tags matching this regex
      duva.exclude_tags: '(alpha|beta|rc)' # drop matching candidates
      duva.delay: 7d                  # let a release age this long first
      duva.auto: patch                # apply patches unattended; queue the rest
```

`duva.auto` decides what may be applied without asking: `none` (the default),
`patch`, `minor` or `major`. A service with no `duva.auto` is only ever
reported. An unknown `duva.*` label is an error rather than something ignored:
a misspelled `duva.includ` would otherwise silently mean "follow the moving
tag", which looks like duva working rather than duva misconfigured.

`duva.delay` soaks a release before adopting it — useful for images whose
publisher occasionally ships a bad build and fixes it within a day. A soaking
candidate is not hidden: the queue lists it separately, with what happens when
the wait ends, and an Update button that takes it early. The wait is a default,
not a lock.

Everything else is env vars — `DUVA_SCHEDULE` (cron expression),
`DUVA_HOSTNAME` (optional, defaults to the OS hostname),
`DUVA_NTFY_URL`/`DUVA_NTFY_TOPIC`/`DUVA_NTFY_TOKEN` for notifications, and
`DUVA_GIT_PUSH` to publish the commit. The container contract is two fixed
mount paths plus env — no config file:

```yaml
services:
  duva:
    image: ghcr.io/miista/duva:latest
    environment:
      DUVA_SCHEDULE: "0 6 * * *"
      DUVA_NTFY_URL: https://ntfy.example.net
      DUVA_NTFY_TOPIC: docker-pin
    env_file: ./duva-secrets.env   # DUVA_NTFY_TOKEN=... ; gitignored
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - .:/compose
      - duva-state:/data
    ports:
      - "8080:8080"   # the approval queue; omit to keep it unreachable
volumes:
  duva-state:
```

**duva must be a service in the stack it watches.** It is not a host-wide
daemon: it reads its own container's compose labels to learn which project to
recreate containers in, and where that project lives on the host. Outside a
compose project those labels do not exist and duva refuses to act rather than
guess — recreating in the wrong project does not replace a container, it
creates a second one alongside. With a root compose file that `include:`s the
rest, one duva covers everything, since an included service belongs to the
root project.

The three mounts are what that work needs:

- **`/var/run/docker.sock`** — duva pulls images and recreates containers by
  driving the host's daemon. This is real access to the host: duva can start
  and stop anything. Give it to a tool you are willing to trust that far.
- **`.:/compose`** — the stack itself. duva parses it to find pinned services
  and their labels, and rewrites the `image:` line when it applies an update.
  Read-write, unlike earlier versions: a read-only mount silently blocks every
  apply. It is also the git repository duva commits into, so it must be a
  checkout, not a deployed copy.
- **`duva-state:/data`** — the state file. It holds the approval queue, the
  digest baselines for moving tags, and what has already been announced, so an
  update is reported once rather than every run and the queue survives a
  restart. A named volume is the right default; it persists across upgrades
  and needs no ownership fiddling.

Both paths are fixed. duva runs in a container, where `/compose` and
`/data/duva.json` are the contract rather than a default, so there is nothing
to configure and nothing to get wrong.

Because duva commits, it will not act on a dirty repository — committing on
top of someone's half-finished edit is never wanted. It is a precondition, not
a setting.

**`/compose` MUST be the compose project _directory_, never the compose file
alone.** Two things break with a single-file mount: `include:`'d nested
compose files resolve relative to the directory and wouldn't exist inside
the container, and a single-file bind mount silently pins the old inode when
the host file is replaced by rename — which is exactly how editors and
`docker pin` itself rewrite it, so duva would keep reading a stale file
forever without any error.

`duva serve` (the image's default command) runs the check on
`DUVA_SCHEDULE`'s cron expression; `duva run` does a single check and
exits.

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

## License

MIT
