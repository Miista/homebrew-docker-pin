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
2. Creates a GitHub release with archives and a checksum file
3. Commits an updated `Formula/docker-pin.rb` back to this repo

## License

MIT
