#!/usr/bin/env bash
# Integration tests for docker pin's tag and digest handling.
#
# Real registry protocol, real docker daemon, real compose files -- but a
# registry running locally, containing exactly the tags each scenario declares.
# Nothing upstream is touched, so these are deterministic and work offline.
#
# The unit tests fake the digest lookups, so they structurally cannot catch the
# class of bug this exists for: pin rewriting the tag it was given. The tag is
# the tag to FOLLOW -- the digest records what is running -- so pinning must
# write the tag back verbatim. Resolving `latest` to whatever version tag
# carried the same digest froze services on that version line: the concrete tag
# never moves, so they silently stopped receiving updates (fixed 2026-08).
set -euo pipefail

cd "$(dirname "$0")/.."
. hack/lib/context.sh
. hack/lib/registry.sh

# Everything created from here on is a context resource: containers, compose
# projects, images and directories are torn down when this exits, however it
# exits. No test has to remember to clean up, and a container left by a run
# that was killed outright is swept before this one starts.
ctx_init integration-pin
registry_setup

BIN="$(ctx_dir bin)/docker-pin"

# With GOCOVERDIR set, build an instrumented binary so this suite's coverage
# can be merged with the unit tests' (go.dev/blog/integration-test-coverage).
# Unset, this is an ordinary build and nothing changes.
echo "== building docker-pin"
if [ -n "${GOCOVERDIR:-}" ]; then
  mkdir -p "$GOCOVERDIR"
  go build -cover -coverpkg=./... -o "$BIN" ./cmd/docker-pin
else
  go build -o "$BIN" ./cmd/docker-pin
fi

echo "== starting a local registry"
registry_start

ROOT="$(ctx_dir project)"

pass=0; fail=0
check() { # check <description> <0|1>
  if [ "$2" = 1 ]; then echo "   ok   $1"; pass=$((pass + 1))
  else echo "   FAIL $1"; fail=$((fail + 1)); fi
}

image_line() { grep -E '^\s+image:' "$1" | head -1 | sed 's/^[[:space:]]*image:[[:space:]]*//'; }

# --- moving tag stays moving -------------------------------------------
scenario "pinning a service on a moving tag"
registry_start
ROOT="$(ctx_dir project)"
push_moving app latest first

cat > "$ROOT/docker-compose.yml" <<EOF
services:
  web:
    image: $(image_ref app latest)
EOF

(cd "$ROOT" && "$BIN" pin web) | sed 's/^/   | /'
line=$(image_line "$ROOT/docker-compose.yml")
echo "   -> $line"

kept=0; [[ "$line" == *":latest@sha256:"* ]] && kept=1
check "the followed tag is kept" "$kept"

# The bug being guarded against: any concrete version tag in place of latest.
not_resolved=0; [[ "$line" != *":1.0."* ]] && not_resolved=1
check "latest is not resolved to a concrete version tag" "$not_resolved"

# --- concrete tag is preserved too --------------------------------------
scenario "pinning a service on a concrete tag"
registry_start
ROOT="$(ctx_dir project)"
push_versions app 1.0.0

cat > "$ROOT/docker-compose.yml" <<EOF
services:
  web:
    image: $(image_ref app 1.0.0)
EOF

(cd "$ROOT" && "$BIN" pin web) | sed 's/^/   | /'
line=$(image_line "$ROOT/docker-compose.yml")
echo "   -> $line"

concrete=0; [[ "$line" == *":1.0.0@sha256:"* ]] && concrete=1
check "a concrete tag is preserved" "$concrete"

# --- upgrade re-pins under the same tag ---------------------------------
scenario "upgrading a service on a moving tag"
registry_start
ROOT="$(ctx_dir project)"
push_versions app 1.0.1
push_moving app latest first

# Start from the moving tag pinned at its first generation, then move it.
STALE=$(digest_of app latest)
cat > "$ROOT/docker-compose.yml" <<EOF
services:
  web:
    image: $(image_ref app latest)@$STALE
EOF
push_moving app latest second   # the tag now points somewhere else

(cd "$ROOT" && "$BIN" upgrade web) | sed 's/^/   | /'
line=$(image_line "$ROOT/docker-compose.yml")
echo "   -> $line"

up_kept=0; [[ "$line" == *":latest@sha256:"* ]] && up_kept=1
check "upgrade keeps the followed tag" "$up_kept"

moved=0; [[ "$line" != *"@$STALE" ]] && moved=1
check "upgrade moved the digest" "$moved"

# --- explicit version IS a tag change -----------------------------------
scenario "upgrading with an explicit version"
registry_start
ROOT="$(ctx_dir project)"
push_versions app 1.0.1
push_moving app latest first

cat > "$ROOT/docker-compose.yml" <<EOF
services:
  web:
    image: $(image_ref app latest)@$(digest_of app latest)
EOF

(cd "$ROOT" && "$BIN" upgrade web 1.0.1) | sed 's/^/   | /'
line=$(image_line "$ROOT/docker-compose.yml")
echo "   -> $line"

explicit=0; [[ "$line" == *":1.0.1@sha256:"* ]] && explicit=1
check "an explicit version becomes the new followed tag" "$explicit"

# --- running container wins over a moved local tag ----------------------
# The real scenario: bring a stack up, live with it, then pin. Meanwhile the
# moving tag gets re-pulled by something else, so the local image for the tag
# is newer than what the container is running. Pin must record what runs.
#
# NOT under $ROOT: compose.FindFile walks up, and $ROOT has its own compose
# file from the tests above that it would find instead.
scenario "pinning a running service whose tag has since moved"
registry_start
PROJECT="$(ctx_dir running)"

# This scenario needs a container that actually runs, so the image carries a
# static binary rather than being FROM scratch alone (see push_runnable).
#
# It is then pulled BY DIGEST, because that is what gives the image a durable
# name@digest identity. When a tag is repointed, docker strips that repo digest
# from the old image; an image pulled by digest keeps one -- which is exactly
# the state a host is in when a tag has moved upstream but the running
# container has not been recreated.
push_runnable runnable v1 first
RUNNING_DIGEST=$(digest_of runnable v1)

docker rmi -f "${REGISTRY_HOST}/runnable:v1" >/dev/null 2>&1 || true
docker pull -q "${REGISTRY_HOST}/runnable@${RUNNING_DIGEST}" >/dev/null
docker tag "${REGISTRY_HOST}/runnable@${RUNNING_DIGEST}" "${REGISTRY_HOST}/runnable:v1"
ctx_image "${REGISTRY_HOST}/runnable@${RUNNING_DIGEST}"

cat > "$PROJECT/docker-compose.yml" <<EOF
services:
  web:
    image: ${REGISTRY_HOST}/runnable:v1
EOF
ctx_compose_up "$PROJECT"

# Now repoint the tag at different content, without touching the container.
push_runnable runnable v1 second
NEW_DIGEST=$(digest_of runnable v1)

(cd "$PROJECT" && "$BIN" pin web) | sed 's/^/   | /'
line=$(image_line "$PROJECT/docker-compose.yml")
echo "   -> $line"

ran=0; [[ "$line" == *"@$RUNNING_DIGEST" ]] && ran=1
check "pins the digest the container is running" "$ran"

not_newer=0; [[ "$line" != *"@$NEW_DIGEST" ]] && not_newer=1
check "does not pin the newer digest sitting on the tag" "$not_newer"

# --- --all is exactly "pin every service", same rules ------------------
# Same scenario: it reuses the running container from above deliberately.
echo "== pin --all mixes running and not-running services"
push_versions app 1.0.0
cat > "$PROJECT/docker-compose.yml" <<EOF
services:
  web:
    image: ${REGISTRY_HOST}/runnable:v1
  idle:
    image: $(image_ref app 1.0.0)
EOF

(cd "$PROJECT" && "$BIN" pin --all) | sed 's/^/   | /'
web_line=$(grep -A2 '^  web:' "$PROJECT/docker-compose.yml" | grep image | sed 's/.*image:[[:space:]]*//')
idle_line=$(grep -A2 '^  idle:' "$PROJECT/docker-compose.yml" | grep image | sed 's/.*image:[[:space:]]*//')
echo "   -> web:  $web_line"
echo "   -> idle: $idle_line"

all_running=0; [[ "$web_line" == *"@$RUNNING_DIGEST" ]] && all_running=1
check "--all pins the running service from its container" "$all_running"

all_local=0; [[ "$idle_line" == *":1.0.0@sha256:"* ]] && all_local=1
check "--all pins the stopped service from the local image" "$all_local"

# --- unknown flags must puke, not run ----------------------------------
scenario "unknown flags are rejected"
registry_start
PROJECT="$(ctx_dir flags)"
push_versions app 1.0.0
# The bug this guards: `pin --all --dry-riun` matched --all before checking
# the argument count, dropped the mistyped flag, and rewrote every compose
# file for real. A typo in a safety flag must never become a live run.
cat > "$PROJECT/docker-compose.yml" <<EOF
services:
  web:
    image: $(image_ref app 1.0.0)
EOF
before=$(cat "$PROJECT/docker-compose.yml")

for bad in "pin --all --dry-riun" "pin web --dry-riun" "pin --typo" "upgrade --all --dry-riun" "upgrade web --dry-riun"; do
  # shellcheck disable=SC2086
  if (cd "$PROJECT" && "$BIN" $bad) >/dev/null 2>&1; then
    check "\`$bad\` exits non-zero" 0
  else
    check "\`$bad\` exits non-zero" 1
  fi
done

unchanged=0; [ "$before" = "$(cat "$PROJECT/docker-compose.yml")" ] && unchanged=1
check "no compose file was touched by any of them" "$unchanged"

# --- locally built services are skipped --------------------------------
# A built image's repo digest is local to this daemon; pinning it produces a
# reference no other host can pull.
echo "== locally built service is not pinned"
mkdir -p "$PROJECT/app"
printf 'FROM scratch\n' > "$PROJECT/app/Dockerfile"
cat > "$PROJECT/docker-compose.yml" <<'EOF'
services:
  app:
    build: ./app
    image: integration-built:local
EOF

(cd "$PROJECT" && "$BIN" pin app) | sed 's/^/   | /'
line=$(image_line "$PROJECT/docker-compose.yml")
echo "   -> $line"

skipped=0; [[ "$line" == "integration-built:local" ]] && skipped=1
check "a built service is left unpinned" "$skipped"

echo "== integration: $pass passed, $fail failed"
exit $((fail > 0))
