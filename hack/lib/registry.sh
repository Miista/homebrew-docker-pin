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

# The registry serves HTTPS with a per-run self-signed certificate, because
# duva speaks HTTPS to registries and that is the right default -- making it
# fall back to plaintext for a test's convenience would weaken the thing being
# tested. A container that must reach it mounts the certificate as its CA
# bundle; see registry_container_args.

REGISTRY_PORT="${REGISTRY_PORT:-5555}"
# From the host, via the published port. Docker trusts localhost registries
# without configuration, so pushes need no daemon changes.
REGISTRY_HOST="localhost:${REGISTRY_PORT}"
# From another container on REGISTRY_NETWORK. Must match the certificate's SAN.
REGISTRY_INTERNAL_HOST="testregistry:5000"
REGISTRY_NAME="testregistry"

# registry_setup prepares what every scenario's registry will share: the TLS
# certificate. Call it once, after ctx_init and before the first scenario.
#
# One certificate per suite, not per scenario: it is the same certificate
# every time, so regenerating a 2048-bit key on each scenario boundary is pure
# cost. Generated rather than committed, so there is no private key in the
# repo and no expiry to trip over in a year. It lives in a run-scoped
# directory, so scenario boundaries leave it alone and only teardown removes
# it.
registry_setup() {
  REGISTRY_CERT_DIR="$(ctx_run_dir certs)"
  # SANs cover both names the registry answers to: testregistry from another
  # container, localhost from the host.
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -keyout "$REGISTRY_CERT_DIR/key.pem" -out "$REGISTRY_CERT_DIR/cert.pem" \
    -subj "/CN=testregistry" \
    -addext "subjectAltName=DNS:testregistry,DNS:localhost,IP:127.0.0.1" 2>/dev/null
  REGISTRY_CERT="$REGISTRY_CERT_DIR/cert.pem"
}

# registry_start brings up a registry and a build context for scratch images.
# Sets REGISTRY_CTX to the context directory. Requires ctx_init to have run:
# the registry and its build context are context resources, torn down with
# everything else.
registry_start() {
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

  docker rm -f "$REGISTRY_NAME" >/dev/null 2>&1 || true

  if [ -z "${REGISTRY_CERT_DIR:-}" ]; then
    echo "registry_setup must be called once before the first scenario" >&2
    return 1
  fi
  local certs="$REGISTRY_CERT_DIR"

  REGISTRY_NETWORK="${CTX_SUITE}-net"
  ctx_network "$REGISTRY_NETWORK"
  ctx_run "$REGISTRY_NAME" \
    --network "$REGISTRY_NETWORK" \
    -p "${REGISTRY_PORT}:5000" \
    -v "$certs:/certs:ro" \
    -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/cert.pem \
    -e REGISTRY_HTTP_TLS_KEY=/certs/key.pem \
    registry:3

  REGISTRY_CTX="$(ctx_dir registry)"
  printf 'FROM scratch\nCOPY marker /marker\n' > "$REGISTRY_CTX/Dockerfile"

  # The registry needs a moment before it accepts pushes.
  for _ in $(seq 40); do
    curl -skf "https://${REGISTRY_HOST}/v2/" >/dev/null 2>&1 && return 0
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

# image_ref <repo> <tag> -- the reference for a compose file the HOST acts on
# (docker pin, or a compose project the suite brings up itself).
image_ref() {
  printf '%s/%s:%s' "$REGISTRY_HOST" "$1" "$2"
}

# internal_ref <repo> <tag> -- the reference for a compose file a CONTAINER
# reads (duva running in one), which reaches the registry by name on the
# shared network rather than through the published port.
internal_ref() {
  printf '%s/%s:%s' "$REGISTRY_INTERNAL_HOST" "$1" "$2"
}

# registry_container_args -- what a container needs to reach the registry: the
# shared network, and our certificate as its CA bundle.
registry_container_args() {
  printf -- '--network %s -v %s:/etc/ssl/certs/ca-certificates.crt:ro' \
    "$REGISTRY_NETWORK" "$REGISTRY_CERT"
}
