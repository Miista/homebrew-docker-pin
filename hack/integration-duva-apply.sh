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
. hack/scenario.sh

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

# queued <data-dir> -- the services duva recorded as needing approval.
queued() { "$(ctx_statequery)" "$1/duva.json" pending; }

image_line() { grep -E '^\s+image:' "$1" | head -1 | sed 's/^[[:space:]]*image:[[:space:]]*//'; }

# pin_app -- write the digest with the real tool, and commit it. The fixture
# holds a plain tag, so the starting state is made the way a real stack's is,
# and duva will not act on a dirty repository.
pin_app() {
  (cd "$WORK" && "$(ctx_docker_pin)" pin app >/dev/null)
  git -C "$WORK" add -A
  git -C "$WORK" -c user.name=t -c user.email=t@t commit -qm "pin app"
}

# check_now -- make duva check, and wait for it to finish.
#
# duva runs serve, which is what ships: up permanently, checking on a
# schedule and once at startup. Restarting it is how a test asks for a check
# now rather than waiting for a cron tick. The line duva prints on going back
# to sleep is what says the check is done.
check_now() {
  # How many checks duva has announced so far. Restarting adds one, and
  # waiting for the count to rise is what distinguishes this check from the
  # one duva ran when the scenario first came up.
  local before after i
  before=$( (cd "$WORK" && docker compose logs --no-log-prefix duva 2>&1) | grep -c "next check at" || true)

  (cd "$WORK" && docker compose restart duva >/dev/null 2>&1)

  for i in $(seq 120); do
    after=$( (cd "$WORK" && docker compose logs --no-log-prefix duva 2>&1) | grep -c "next check at" || true)
    [ "$after" -gt "$before" ] && return 0
    sleep 0.25
  done
  echo "duva did not finish a check in 30s" >&2
  (cd "$WORK" && docker compose logs duva >&2)
  return 1
}

# --- an allowed update reaches the container -----------------------------
scenario "an update within policy is applied and committed"
scenario_up within-policy
push_runnable app 1.0.1
pin_app
before_container=$(victim "$WORK")

check_now

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
scenario_up beyond-policy
push_runnable app 2.0.0
pin_app
before=$(image_line "$WORK/docker-compose.yml")

check_now

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
scenario_up refused-image
pin_app

# 1.0.1 exists in the registry but cannot run, so the recreate fails after
# the pin has been written.
push_unrunnable app 1.0.1

before=$(image_line "$WORK/docker-compose.yml")

check_now

c=0; [ "$(image_line "$WORK/docker-compose.yml")" = "$before" ] && c=1
check "the compose file is put back" "$c"

c=0; [ -z "$(git -C "$WORK" status --porcelain)" ] && c=1
check "no half-applied change is left behind" "$c"

c=0; [ -z "$(git -C "$WORK" log --oneline -1 --grep='1.0.1')" ] && c=1
check "a failed update is not committed" "$c"

# --- a relative bind survives being seen at a different path -------------
scenario "a relative bind still points at the host directory after a recreate"
scenario_up relative-bind
push_runnable app 1.0.1
pin_app

# Read from the daemon's side: these images are FROM scratch with a static
# binary and have no shell to exec into.
bind_source() {
  docker inspect "$1" --format '{{range .Mounts}}{{if eq .Destination "/mnt"}}{{.Source}}{{end}}{{end}}' 2>/dev/null
}

c=0; [ "$(bind_source "$(victim "$WORK")")" = "$WORK/conf" ] && c=1
check "the bind points at the host directory to begin with" "$c"

check_now

c=0; [[ "$(image_line "$WORK/docker-compose.yml")" == *":1.0.1@sha256:"* ]] && c=1
check "the update is applied" "$c"

got=$(bind_source "$(victim "$WORK")")
c=0; [ "$got" = "$WORK/conf" ] && c=1
check "the relative bind still resolves to the host directory (got: ${got:-<empty>})" "$c"

echo "== integration: $pass passed, $fail failed"
exit $((fail > 0))
