# One repo, one product: this repository becomes duva

Status: **agreed, not started.** Decided 2026-10-21. To be carried out with
duva v5's first release, so the first tag of the renamed repository is v5's.

## Why

The repository was docker-pin with duva beside it. It is now duva with
docker-pin beside it, and duva is what is run every day. docker-pin is still
used, but at a terminal now and then, and it is installed from Cloudsmith,
not the Homebrew tap. The tap is the only reason the repository is called
`homebrew-docker-pin`.

docker-pin stays. It becomes duva's command-line tool, released with it.

## Decisions

- **The repository is renamed `duva`**, and the Go module path goes from
  `github.com/Miista/homebrew-docker-pin` to `github.com/Miista/duva`.
- **Nothing moves out.** docker-pin's code (`cmd/docker-pin`,
  `cmd/docker-unpin`, `internal/pin`, `internal/docker`, `internal/help`)
  stays where it is. So do the shared modules (`compose`, `oci`,
  `dockerapi`), and they keep replacing each other by relative path. No
  second repository, and no cross-repo versions to tag.
- **One version, one tag: `vX.Y`.** It replaces the CLI's `vX.Y.Z` and
  `duva-v5/vX.Y`. duva v4 keeps its `duva-v4/vX.Y` tags until it is retired,
  as v3's were.
- **One release pipeline.** A `v*` tag publishes:
  - **the duva image** to `ghcr.io/miista/duva`, for `linux/amd64` and
    `linux/arm64`, as `.goreleaser.duva-v5.yaml` does today;
  - **the `docker-pin` and `docker-unpin` binaries** for linux and darwin,
    amd64 and arm64, with their man pages;
  - **the .deb packages to Cloudsmith** (`guldmund/stable`, distribution
    `debian/any-version`), as `release.yml`'s upload step does today.
- **No Homebrew tap.** `Formula/`, GoReleaser's `brews:` block, and the
  `cliPluginsExtraDirs` caveat go. Cloudsmith is the one way to install the
  binaries. (Today it carries only .debs. Darwin binaries stay on the GitHub
  release unless Cloudsmith gains a raw or Homebrew-style package for them.
  See open questions.)
- **docker-pin stays a Docker CLI plugin**, `docker pin ...`. Folding it into
  the duva binary as `duva pin` was considered and not wanted: the plugin
  works, and it is how it is reached at a terminal.

## What exists today

| | |
|---|---|
| `release.yml` | on `v*`: genman, GoReleaser (`.goreleaser.yaml`: builds, archives, nfpm .debs, `brews:` into `Formula/`), then a curl loop uploading each .deb to Cloudsmith |
| `duva-v5-release.yml` | on `duva-v5/v*`: GoReleaser with `.goreleaser.duva-v5.yaml`: the duva binary, and the image to ghcr.io for both arches |
| `duva-v4-release.yml` | on `duva-v4/v*`: v4's four images |
| `ci.yml` | tests |
| `Formula/` | `docker-pin.rb`, `docker-plugins.rb`, written by GoReleaser |
| module path | `github.com/Miista/homebrew-docker-pin` in 112 Go files and every `go.mod` |

## Steps

1. **Ship v5 first.** Settle the page, pin the image on both hosts, commit
   on both, and release it once under the old layout if a release is
   needed before the rename.
2. **Merge the release pipelines.**
   - One `.goreleaser.yaml` with both builds (`docker-pin`/`docker-unpin`
     and `duva`), the archives and nfpm for the plugins, and `dockers_v2`
     for the duva image. Drop `brews:`.
   - One `release.yml` on `v*`: genman, GoReleaser, the Cloudsmith upload,
     with ghcr login as today.
   - Remove `duva-v5-release.yml` and `.goreleaser.duva-v5.yaml`. Keep
     `duva-v4-release.yml` until v4 is retired.
3. **Rename the repository on GitHub** to `duva`. GitHub redirects the old
   URL for clones and fetches.
4. **Rewrite the module path** in one mechanical commit: every `go.mod`,
   `replace` and import from `github.com/Miista/homebrew-docker-pin` to
   `github.com/Miista/duva`. Then `go build ./...`, the unit tests and the
   integration suites.
5. **Remove the tap:** `Formula/`, the README's install-via-brew section,
   and CLAUDE.md's `cliPluginsExtraDirs` and "registered in the formula
   `install` line" notes.
6. **Docs.**
   - README: what duva is; docker-pin as its command-line tool; installing
     the binaries from Cloudsmith and running the image from ghcr.
   - CLAUDE.md: the opening paragraph, module path, release tags, and the
     "new top-level plugin" convention (no formula line any more).
7. **Hosts.**
   - Image references move to `ghcr.io/miista/duva:vX.Y@sha256:...`, from
     `registry.guldmund.dk/duva:latest` (optiplex) and
     `localhost:5000/duva:latest` (pi).
   - Anyone installing docker-pin through the tap switches to Cloudsmith.
8. **Tag `v5.0`** (or whatever v5's first number is), and check that the
   release produced the image on ghcr, the binaries on the GitHub release,
   and the .debs on Cloudsmith.

## What does not change

- The module split (`compose`, `oci`, `dockerapi`, duva-v5) and why it
  exists: coverage kept apart, `internal/` boundaries. Only the path prefix
  changes.
- docker-pin's behaviour and its CLAUDE.md rules: "the tag is the tag to
  follow", `pin` taking its digest from the running container first, and
  so on.
- The integration harness: it already covers both, in one place.

## Open questions

- **Darwin binaries on Cloudsmith.** Cloudsmith takes .debs from this
  pipeline today. For the Mac at a terminal, is the GitHub release download
  enough, or should Cloudsmith carry a raw or Homebrew-style package too?
- **Version number.** Does the first release of the merged repository
  continue duva's numbering (v5.x), or docker-pin's? duva's is the one
  running; docker-pin's next tag would follow it.
- **Retiring v4.** After pi and optiplex have both run v5 for a while:
  delete `duva-v4/`, its workflow, its images and its CLAUDE.md section,
  as v3's were.
