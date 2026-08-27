#!/usr/bin/env bash
# Integration tests for duva applying an update: pull, pin, recreate, commit.
#
# Everything real except the world it runs in. A real registry, a real docker
# daemon, a real container that is genuinely replaced, a real git repository
# -- all local, nothing upstream. The unit tests fake docker and git, so what
# they cannot show is whether the container actually ends up running the image
# duva pinned, or whether the commit really lands.
set -euo pipefail

cd "$(dirname "$0")/.."
. hack/lib/context.sh
. hack/lib/registry.sh

ctx_init integration-duva-apply
registry_setup

IMAGE=duva:integration-apply

echo "== building $IMAGE"
# With GOCOVERDIR set the binary is instrumented, so this suite's coverage
# merges with the unit tests' (go.dev/blog/integration-test-coverage).
COVER_MOUNT=""
if [ -n "${GOCOVERDIR:-}" ]; then
  mkdir -p "$GOCOVERDIR"
  chmod 777 "$GOCOVERDIR"
  COVER_MOUNT="-v $GOCOVERDIR:/covdata -e GOCOVERDIR=/covdata"
fi
ctx_build_duva_applier "$IMAGE"

pass=0; fail=0
check() { # check <description> <0|1>
  if [ "$2" = 1 ]; then echo "   ok   $1"; pass=$((pass + 1))
  else echo "   FAIL $1"; fail=$((fail + 1)); fi
}

# project <dir> <service> <image-ref> [labels...] -- a compose project in a
# git repository, since duva commits its change.
project() {
  local dir="$1" service="$2" ref="$3"; shift 3
  mkdir -p "$dir"
  {
    echo "services:"
    echo "  $service:"
    echo "    image: $ref"
    if [ "$#" -gt 0 ]; then
      echo "    labels:"
      printf '      %s\n' "$@"
    fi
  } > "$dir/docker-compose.yml"

  git -C "$dir" init -q
  git -C "$dir" add docker-compose.yml
  git -C "$dir" -c user.name=t -c user.email=t@t commit -qm "initial"
}

# duva_apply <compose-dir> <data-dir> -- one applying run.
#
# The socket is mounted because applying recreates a container, and the
# compose mount is read-write because applying rewrites the file -- the two
# ways this differs from a reporting duva.
#
# The registry is addressed as localhost:PORT rather than by its network name:
# duva drives the host's daemon through the mounted socket, so every image
# reference it writes or pulls has to be one that daemon can resolve. Reaching
# the published port from inside the container needs the host gateway.
# shellcheck disable=SC2046,SC2086
duva_apply() {
  docker run --rm \
    --add-host=localhost:host-gateway \
    -v "$REGISTRY_CERT:/etc/ssl/certs/ca-certificates.crt:ro" \
    $COVER_MOUNT \
    -e DUVA_HOSTNAME=integration \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$1:$1" -v "$2:/data" \
    -w "$1" \
    -e DUVA_COMPOSE_DIR="$1" \
    "$IMAGE" run 2>&1
}

image_line() { grep -E '^\s+image:' "$1" | head -1 | sed 's/^[[:space:]]*image:[[:space:]]*//'; }

# --- an allowed update reaches the container -----------------------------
scenario "an update within policy is applied and committed"
registry_start
push_runnable app 1.0.0 first
push_runnable app 1.0.1 second

WORK="$(ctx_dir work)"
DATA="$(ctx_dir data)"; chmod 777 "$DATA"
project "$WORK" app "$(image_ref app 1.0.0)@$(digest_of app 1.0.0)" \
  "duva.include: '^1\.0\.\d+\$'" \
  "duva.auto: patch"

# Bring the service up on the old image first: duva recreates a container, it
# does not create one from nothing.
ctx_compose_up "$WORK"
before_container=$(docker ps -q --filter "label=com.docker.compose.project.working_dir=$WORK" | head -1)

out=$(duva_apply "$WORK" "$DATA")
echo "$out" | sed 's/^/   | /'

line=$(image_line "$WORK/docker-compose.yml")
echo "   -> $line"

c=0; [[ "$line" == *":1.0.1@sha256:"* ]] && c=1
check "the compose file is pinned to the new version" "$c"

after_container=$(docker ps -q --filter "label=com.docker.compose.project.working_dir=$WORK" | head -1)
c=0; [ -n "$after_container" ] && [ "$after_container" != "$before_container" ] && c=1
check "the container was actually replaced" "$c"

running=$(docker inspect "$after_container" -f '{{.Config.Image}}' 2>/dev/null || true)
c=0; [[ "$running" == *"1.0.1"* ]] && c=1
check "the running container is on the new image (got ${running##*/})" "$c"

c=0; git -C "$WORK" log --oneline -1 | grep -q "app: 1.0.0 -> 1.0.1" && c=1
check "the change is committed with a readable subject" "$c"

c=0; [ -z "$(git -C "$WORK" status --porcelain)" ] && c=1
check "the repository is left clean" "$c"

# --- nothing is applied beyond policy ------------------------------------
scenario "an update beyond policy is left for a human"
registry_start
push_runnable app 1.0.0 first
push_runnable app 2.0.0 second

WORK="$(ctx_dir work)"
DATA="$(ctx_dir data)"; chmod 777 "$DATA"
project "$WORK" app "$(image_ref app 1.0.0)@$(digest_of app 1.0.0)" \
  "duva.include: '^\d+\.\d+\.\d+\$'" \
  "duva.auto: patch"
before=$(image_line "$WORK/docker-compose.yml")

out=$(duva_apply "$WORK" "$DATA")
echo "$out" | sed 's/^/   | /'

c=0; [ "$(image_line "$WORK/docker-compose.yml")" = "$before" ] && c=1
check "the compose file is untouched" "$c"

c=0; [ -z "$(git -C "$WORK" log --oneline -1 --grep='2.0.0')" ] && c=1
check "nothing is committed" "$c"

c=0; echo "$out" | grep -q "needs approval" && c=1
check "it is reported as needing approval" "$c"

# --- a refused image leaves no lie in the file ---------------------------
# The one case duva undoes: compose refuses the new image, so the container
# never took it, and the file must not keep claiming it did.
scenario "a compose failure puts the file back"
registry_start
push_runnable app 1.0.0 first
# 1.0.1 exists in the registry but cannot run: FROM scratch with no command,
# so compose up fails after the pin has been written.
printf '%s\n' "app:1.0.1:broken" > "$REGISTRY_CTX/marker"
docker build -q -t "${REGISTRY_HOST}/app:1.0.1" "$REGISTRY_CTX" >/dev/null
docker push -q "${REGISTRY_HOST}/app:1.0.1" >/dev/null
ctx_image "${REGISTRY_HOST}/app:1.0.1"

WORK="$(ctx_dir work)"
DATA="$(ctx_dir data)"; chmod 777 "$DATA"
project "$WORK" app "$(image_ref app 1.0.0)@$(digest_of app 1.0.0)" \
  "duva.include: '^1\.0\.\d+\$'" \
  "duva.auto: patch"
ctx_compose_up "$WORK"
before=$(image_line "$WORK/docker-compose.yml")

out=$(duva_apply "$WORK" "$DATA")
echo "$out" | sed 's/^/   | /'

c=0; [ "$(image_line "$WORK/docker-compose.yml")" = "$before" ] && c=1
check "the compose file is put back" "$c"

c=0; [ -z "$(git -C "$WORK" status --porcelain)" ] && c=1
check "no half-applied change is left behind" "$c"

c=0; [ -z "$(git -C "$WORK" log --oneline -1 --grep='1.0.1')" ] && c=1
check "a failed update is not committed" "$c"

echo "== integration: $pass passed, $fail failed"
exit $((fail > 0))
