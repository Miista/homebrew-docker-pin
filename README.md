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

```bash
curl -fsSL https://apt.guldmund.dk/setup.sh | sudo sh
sudo apt install docker-pin
```

Or, if you'd rather not pipe scripts into a root shell, do the repo setup
explicitly (one-time):

```bash
sudo install -d /etc/apt/keyrings
curl -fsSL https://apt.guldmund.dk/guldmund-archive-keyring.asc \
  | sudo tee /etc/apt/keyrings/guldmund-archive-keyring.asc > /dev/null
echo "deb [signed-by=/etc/apt/keyrings/guldmund-archive-keyring.asc] https://apt.guldmund.dk stable main" \
  | sudo tee /etc/apt/sources.list.d/guldmund.list
sudo apt update && sudo apt install docker-pin
```

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
```

`apply` translates the cron expression to a systemd `OnCalendar` and writes a
`docker-pin-<dir>-<hash>.service`/`.timer` pair into `/etc/systemd/system`.
Each run upgrades the configured services like `docker pin upgrade`; when the
compose file changed it runs `docker compose up -d` and then the `on_change`
command. A service with a `tags` regex only ever moves to the newest registry
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
`pin.yaml`. Runs never leave the system in a bad state: if `docker compose up
-d` fails, the compose file is rolled back to its pre-run pins and `up -d`
re-run to re-assert the last working state (the upgrade retries next run), and
a failed `on_change` (e.g. a rejected `git push`) is non-fatal — the local
commit rides along with the next push. Restricting both day-of-month and
day-of-week in the cron
expression is rejected (cron ORs them, systemd ANDs them). Requires Linux with
systemd.

Because the schedule lives in `pin.yaml`, restoring a host from backup is
just: clone the repo, `sudo docker pin schedule apply`.

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

## Release & distribution

This repo is the Homebrew tap. Pushing a `vX.Y.Z` tag triggers a GoReleaser workflow that:

1. Builds `docker-pin` and `docker-unpin` for `linux/darwin` × `amd64/arm64`
2. Creates a GitHub release with archives, `.deb` packages, and a checksum file
3. Commits an updated `Formula/docker-pin.rb` back to this repo
4. Rebuilds the shared apt repository (docker-pin + splitdns, latest release
   of each), signs it with the GPG key in the `APT_GPG_PRIVATE_KEY` repo
   secret, and deploys it to Cloudflare Pages at https://apt.guldmund.dk
   (only latest versions served via apt; older `.deb`s stay on the releases)

## License

MIT
