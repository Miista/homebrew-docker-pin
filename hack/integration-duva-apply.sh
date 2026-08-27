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
ctx_build_duva_image "$IMAGE"

pass=0; fail=0
check() { # check <description> <0|1>
  if [ "$2" = 1 ]; then echo "   ok   $1"; pass=$((pass + 1))
  else echo "   FAIL $1"; fail=$((fail + 1)); fi
}

# duva_apply <compose-dir> -- bring duva up and wait for its run to finish.
#
# duva is started as the compose service it is declared as, not with a bare
# `docker run`: it reads its own container's project label to know which
# project to recreate containers in, and outside a project there is no label.
#
# Nothing asserts on its output. What duva did IS the state of the world -- the
# file repinned, the container replaced, the commit written, the queue
# recorded. A check on a log line would only prove duva announced something,
# which is the one thing that cannot break a stack.
duva_apply() {
  (cd "$1" && docker compose up -d duva >/dev/null 2>&1)
  # duva run is a one-shot: it checks, acts and exits.
  local i
  for i in $(seq 60); do
    case "$(cd "$1" && docker compose ps -a --status exited --format '{{.Service}}' 2>/dev/null)" in
      *duva*) return 0 ;;
    esac
    sleep 0.5
  done
  echo "duva did not finish in 30s" >&2
  (cd "$1" && docker compose logs duva >&2)
  return 1
}

# victim <dir> -- the watched container, found by project and service.
#
# Not by working_dir: that label records the directory of whoever last ran
# compose, and duva runs it from its own mount point -- so the container it
# recreates says /compose while the one the suite started says the host path.
# The project name is the same either way, which is the point of duva reading
# it from its own labels.
#
# The service is named because duva is in this project too.
victim() {
  docker ps -q --filter "label=com.docker.compose.project=$(basename "$1")" \
    --filter "label=com.docker.compose.service=app" | head -1
}

# start <scenario> -- lay out a scenario's world and bring the watched service
# up on its current image.
#
# The fixture carries a plain tag; docker pin writes the digest, so the
# starting state is produced by the real tool rather than forged here. duva
# then has something to recreate: it replaces a container, it does not create
# one from nothing.
start() {
  WORK="$(ctx_dir work)"
  DATA="$(ctx_dir data)"; chmod 777 "$DATA"
  ctx_fixture "$1" "$WORK"

  (cd "$WORK" && "$(ctx_docker_pin)" pin app >/dev/null)
  git -C "$WORK" add -A
  git -C "$WORK" -c user.name=t -c user.email=t@t commit -qm "pin app"

  ctx_compose_up "$WORK" app
}

# queued <data-dir> -- the services duva recorded as needing approval.
queued() { "$(ctx_statequery)" "$1/duva.json" pending; }

image_line() { grep -E '^\s+image:' "$1" | head -1 | sed 's/^[[:space:]]*image:[[:space:]]*//'; }

# --- an allowed update reaches the container -----------------------------
scenario "an update within policy is applied and committed"
registry_start
push_runnable app 1.0.0 first
push_runnable app 1.0.1 second

start within-policy
before_container=$(victim "$WORK")

duva_apply "$WORK"

line=$(image_line "$WORK/docker-compose.yml")
echo "   -> $line"

c=0; [[ "$line" == *":1.0.1@sha256:"* ]] && c=1
check "the compose file is pinned to the new version" "$c"

after_container=$(victim "$WORK")
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

start beyond-policy
before=$(image_line "$WORK/docker-compose.yml")

duva_apply "$WORK"

c=0; [ "$(image_line "$WORK/docker-compose.yml")" = "$before" ] && c=1
check "the compose file is untouched" "$c"

c=0; [ -z "$(git -C "$WORK" log --oneline -1 --grep='2.0.0')" ] && c=1
check "nothing is committed" "$c"

c=0; [ "$(queued "$DATA")" = "app" ] && c=1
check "it is queued for approval" "$c"

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

start refused-image
before=$(image_line "$WORK/docker-compose.yml")

duva_apply "$WORK"

c=0; [ "$(image_line "$WORK/docker-compose.yml")" = "$before" ] && c=1
check "the compose file is put back" "$c"

c=0; [ -z "$(git -C "$WORK" status --porcelain)" ] && c=1
check "no half-applied change is left behind" "$c"

c=0; [ -z "$(git -C "$WORK" log --oneline -1 --grep='1.0.1')" ] && c=1
check "a failed update is not committed" "$c"

# --- a relative bind survives being seen at a different path -------------
scenario "a relative bind still points at the host directory after a recreate"
registry_start
push_runnable app 1.0.0 first
push_runnable app 1.0.1 second

start relative-bind

# Read from the daemon's side: these images are FROM scratch with a static
# binary and have no shell to exec into.
bind_source() {
  docker inspect "$1" --format '{{range .Mounts}}{{if eq .Destination "/mnt"}}{{.Source}}{{end}}{{end}}' 2>/dev/null
}

c=0; [ "$(bind_source "$(victim "$WORK")")" = "$WORK/conf" ] && c=1
check "the bind points at the host directory to begin with" "$c"

duva_apply "$WORK"

c=0; [[ "$(image_line "$WORK/docker-compose.yml")" == *":1.0.1@sha256:"* ]] && c=1
check "the update is applied" "$c"

got=$(bind_source "$(victim "$WORK")")
c=0; [ "$got" = "$WORK/conf" ] && c=1
check "the relative bind still resolves to the host directory (got: ${got:-<empty>})" "$c"

echo "== integration: $pass passed, $fail failed"
exit $((fail > 0))
