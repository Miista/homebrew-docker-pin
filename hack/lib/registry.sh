# Shared helpers for the integration suites: a real OCI registry, running
# locally, containing exactly the tags a test asks for.
#
# These are integration tests, not end-to-end: they exercise the real registry
# protocol, the real docker daemon and the real compose CLI, but nothing
# upstream. Depending on Docker Hub meant asserting that nginx:1.26.3 is still
# the newest of its line -- an assumption someone else can invalidate at any
# time, and one that fails with no network.
#
# Images are FROM scratch: duva and docker-pin only ever read manifests and
# digests, so there is no reason for them to contain anything. Each version
# gets distinct content, because tagging one image as several versions gives
# them all one digest -- and duva compares digests, so it would correctly
# report "up to date" and the test would be meaningless.

REGISTRY_PORT="${REGISTRY_PORT:-5555}"
REGISTRY_HOST="localhost:${REGISTRY_PORT}"
REGISTRY_NAME="${REGISTRY_NAME:-integration-registry}"

# registry_start brings up a registry and a build context for scratch images.
# Sets REGISTRY_CTX to the context directory. Requires ctx_init to have run:
# the registry and its build context are context resources, torn down with
# everything else.
registry_start() {
  REGISTRY_NAME="${CTX_SUITE}-registry"

  # The port is the resource, not the name. Anything holding it -- a registry
  # from a suite that was killed, or one started by hand -- is wiped, because
  # its contents are unknown and a test that finds unexpected tags is worse
  # than one that has to wait a second for a fresh registry.
  local holding
  holding=$(docker ps -aq --filter "publish=${REGISTRY_PORT}" 2>/dev/null || true)
  if [ -n "$holding" ]; then
    # shellcheck disable=SC2086
    docker rm -f $holding >/dev/null 2>&1 || true
  fi

  ctx_run "$REGISTRY_NAME" -p "${REGISTRY_PORT}:5000" registry:3

  REGISTRY_CTX="$(ctx_dir registry)"
  printf 'FROM scratch\nCOPY marker /marker\n' > "$REGISTRY_CTX/Dockerfile"

  # The registry needs a moment before it accepts pushes.
  for _ in $(seq 40); do
    curl -sf "http://${REGISTRY_HOST}/v2/" >/dev/null 2>&1 && return 0
    sleep 0.25
  done
  echo "registry did not come up on ${REGISTRY_HOST}" >&2
  return 1
}

# push_versions <repo> <tag>... -- publishes each tag as a distinct image.
#
#   push_versions app 1.0.0 1.0.1 1.1.0
#
# A test declares the shape it needs and gets exactly that: no more tags, no
# fewer, and none that some upstream might add tomorrow.
push_versions() {
  local repo="$1"; shift
  local tag
  for tag in "$@"; do
    printf '%s\n' "$repo:$tag" > "$REGISTRY_CTX/marker"
    docker build -q -t "${REGISTRY_HOST}/${repo}:${tag}" "$REGISTRY_CTX" >/dev/null
    docker push -q "${REGISTRY_HOST}/${repo}:${tag}" >/dev/null
    ctx_image "${REGISTRY_HOST}/${repo}:${tag}"
  done
}

# push_moving <repo> <tag> <generation> -- (re)publishes a moving tag with new
# content, so its digest changes between generations. Use a different
# generation string to simulate the tag being moved upstream.
#
#   push_moving web latest first
#   ...
#   push_moving web latest second   # same tag, new digest
push_moving() {
  local repo="$1" tag="$2" generation="$3"
  printf '%s\n' "$repo:$tag:$generation" > "$REGISTRY_CTX/marker"
  docker build -q -t "${REGISTRY_HOST}/${repo}:${tag}" "$REGISTRY_CTX" >/dev/null
  docker push -q "${REGISTRY_HOST}/${repo}:${tag}" >/dev/null
  ctx_image "${REGISTRY_HOST}/${repo}:${tag}"
}

# push_runnable <repo> <tag> <generation> -- publishes an image that can
# actually run, for scenarios needing a live container.
#
# FROM scratch with a static Go binary that prints every few seconds. A shell
# script would need a shell, and every base image carrying one is upstream --
# so the suite would depend on Docker Hub for this one scenario. A static
# binary needs nothing at all.
#
# generation varies the output, so two generations of the same tag are
# genuinely different images with different digests.
push_runnable() {
  local repo="$1" tag="$2" generation="$3"
  local dir="$REGISTRY_CTX/runnable"
  mkdir -p "$dir"

  cat > "$dir/main.go" <<GOEOF
package main

import (
	"fmt"
	"time"
)

func main() {
	for {
		fmt.Println("alive: ${generation}")
		time.Sleep(5 * time.Second)
	}
}
GOEOF
  (cd "$dir" && CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o sleeper main.go)
  printf 'FROM scratch\nCOPY sleeper /sleeper\nCMD ["/sleeper"]\n' > "$dir/Dockerfile"

  docker build -q -t "${REGISTRY_HOST}/${repo}:${tag}" "$dir" >/dev/null
  docker push -q "${REGISTRY_HOST}/${repo}:${tag}" >/dev/null
  ctx_image "${REGISTRY_HOST}/${repo}:${tag}"
}

# push_multiarch <repo> <tag> -- publishes a real manifest index covering two
# platforms, so the index-vs-manifest digest handling is exercised for real
# (buildx also adds attestation manifests, which the descent must skip).
push_multiarch() {
  local repo="$1" tag="$2"
  printf '%s\n' "$repo:$tag:multiarch" > "$REGISTRY_CTX/marker"
  docker buildx build --platform linux/amd64,linux/arm64 \
    -t "${REGISTRY_HOST}/${repo}:${tag}" --push "$REGISTRY_CTX" >/dev/null 2>&1
}

# digest_of <repo> <tag> -- the digest THIS registry serves for a tag.
#
# An image can carry several repo digests (a busybox-derived image keeps
# busybox@... alongside the local registry's), so the one for the repo being
# asked about has to be selected rather than taking the first.
digest_of() {
  local repo="$1" tag="$2"
  docker image inspect "${REGISTRY_HOST}/${repo}:${tag}" \
    --format '{{range .RepoDigests}}{{println .}}{{end}}' 2>/dev/null |
    grep "^${REGISTRY_HOST}/${repo}@" | head -1 | sed 's/.*@//'
}

# image_ref <repo> <tag> -- the full image reference for a compose file.
image_ref() {
  printf '%s/%s:%s' "$REGISTRY_HOST" "$1" "$2"
}
