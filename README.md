# docker-pin

Docker CLI plugins to pin container images in a Compose file to an exact tag and SHA digest — and keep them there.

## The problem

`image: postgres:16` is a moving target. Every `docker compose pull` can silently swap the image under you. Pinning to a digest (`image: postgres:16.3@sha256:...`) makes deployments reproducible, but doing it by hand — looking up digests, rewriting lines, updating after upgrades — is friction nobody wants.

## What it does

Two plugins:

- **`docker pin`** — pins a service to its current digest. Pulls the image if not local. If the tag is `latest`, resolves it to the most specific immutable version tag (e.g. `5.8.0-ga1b2c3d`) so the pin is meaningful, not just `latest@sha256:...`.
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

No-op if the service is already pinned. Pulls the image if it isn't present locally.

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
    image: postgres:16.3@sha256:a3dc6b...
```

If the tag is `latest`, the plugin resolves it to the most specific immutable version tag available in the registry:

```yaml
  gatus:
    image: ghcr.io/miista/gatus-wrapper:5.8.0-ga1b2c3d@sha256:...
```

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

### Scheduled upgrades (systemd)

Declare a schedule in a git-tracked `pin.yaml` (also accepts `pin.yml`) next
to the compose file:

```yaml
schedule: "0 6 * * 1"        # 5-field cron expression, required
services:                     # optional; omitted = every service
  - caddy
  - name: paperless-db        # constrained: upgrade only to registry tags
    tags: '^17\.\d+-alpine$'  # matching this regex — majors stay pinned
    exclude: '(alpha|beta|rc)' # optional; drop matching candidates
    delay: 7d                 # optional; only adopt tags published at least
                              # this long ago ("48h", "7d", "2w")
on_change: ./pin-upgraded.sh  # optional; run in the compose dir after
                              # upgrades changed the compose file
hostname: optiplex            # optional; box name in notification titles
                              # (defaults to the OS hostname)
notify:                       # optional; report each run via ntfy
  ntfy:
    url: https://ntfy.example.net
    topic: docker-pin
    token_env: NTFY_TOKEN     # env var holding the token (this is the default)
    token_file: /etc/ntfy.env # optional KEY=VALUE file to read it from
```

```bash
sudo docker pin schedule apply    # install/update the systemd timer (idempotent)
docker pin schedule status        # config, install/drift state, next fire time
sudo docker pin schedule remove   # disable + delete the units; pin.yaml stays
docker pin schedule run           # one scheduled run in the foreground
docker pin schedule run --dry-run # full discovery, prints what would upgrade,
                                  # changes nothing
```

`apply` translates the cron expression to a systemd `OnCalendar` and writes a
`docker-pin-<dir>-<hash>.service`/`.timer` pair into `/etc/systemd/system`.
Each run treats every configured service as its own transaction: upgrade the
pin like `docker pin upgrade`, `docker compose up -d <service>`, run
`on_change`, send one notification — independently per service. `on_change`
receives `PIN_SERVICE`, `PIN_OLD_IMAGE` and `PIN_NEW_IMAGE` in its
environment, so a hook like

```yaml
on_change: git add docker-compose.yml && git commit -m "optiplex/$PIN_SERVICE: upgrade to $PIN_NEW_IMAGE" && git push
```

produces one revertable commit per service upgrade. The full set of hook
variables: `PIN_SERVICE`, `PIN_OLD_IMAGE`/`PIN_NEW_IMAGE` (full references),
`PIN_OLD_TAG`/`PIN_NEW_TAG` and `PIN_OLD_DIGEST`/`PIN_NEW_DIGEST` — so e.g.
`git commit -m "$PIN_SERVICE: $PIN_OLD_TAG -> $PIN_NEW_TAG"` needs no string
surgery. A service with a `tags` regex only ever moves to the newest registry
tag matching that regex (numeric version order, prerelease/build suffixes rank
below the bare release) and is left untouched when nothing newer matches — it
never falls back to a moving tag, so e.g. a database can track `17.x-alpine`
patches while never jumping to 18. `exclude` drops candidates matching a
second regex. `delay` adds a soak period: the newest candidate published at
least that long ago wins, so a release gets time in the wild before you adopt
it (publish time from the Docker Hub tag API, or the image config's `created`
timestamp on GHCR and other OCI registries; at most 10 candidate dates are
checked per service per run). With `notify.ntfy` configured, every run
that upgraded or failed anything posts a summary (failures at high priority);
the token is read from the environment or a `KEY=VALUE` file, never from
`pin.yaml`. Runs never leave the system in a bad state: if a service's
`compose up` fails, only that service's pin is rolled back and re-asserted —
the other services proceed untouched and the failed upgrade retries next run —
and a failed `on_change` (e.g. a rejected `git push`) is non-fatal: the local
commit rides along with the next push. Restricting both day-of-month and
day-of-week in the cron
expression is rejected (cron ORs them, systemd ANDs them). Requires Linux with
systemd.

Because the schedule lives in `pin.yaml`, restoring a host from backup is
just: clone the repo, `sudo docker pin schedule apply`.

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

## How digests work (multi-arch)

`docker image inspect` returns the **index digest** — the hash of the multi-arch manifest list, not a per-platform image digest. This is intentional:

- Pinning the index digest keeps the pin portable across architectures. A per-arch digest would lock you to one platform.
- Two tags can point to byte-identical `linux/amd64` images but have different index digests if their manifest lists carry different platform sets (e.g. `latest` includes Windows images, `alpine` does not). The "already up to date" check will therefore treat them as different — this is correct, not a bug.
- The digest shown in the Docker Hub "Digest" column is the per-arch manifest digest and may differ from the index digest recorded by this tool. This is expected.

## Version tag resolution

When a service is on `latest` (or you upgrade to `latest`), the plugin queries the registry for all version-like tags and matches their manifest digests against the freshly pulled image. It picks the most specific matching tag — most dots, then longest — so a `-g<sha>` build tag wins over a bare `1.2`.

If no version tag matches the local digest (the image is orphaned — a newer build replaced the tag), the plugin warns and falls back to pinning with the pull tag unchanged. It also suggests `docker pin upgrade <service>` to get back onto a tagged version.

Resolution is supported for:
- **Docker Hub** — public images, no auth required
- **GHCR** — `ghcr.io/` images
- **Any OCI-compliant registry** — bearer auth discovered via `WWW-Authenticate` challenge

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
