#!/usr/bin/env bash
# e2e for docker pin's tag handling, against a real registry.
#
# The unit tests fake getDigest/pull, so they can't catch the class of bug
# this exists for: pin rewriting the tag it was given. The tag is the tag to
# FOLLOW — the digest records what is running — so pinning must write the tag
# back verbatim. Resolving `latest` to whatever version tag carries the same
# digest froze services on that version line: the concrete tag never moves, so
# they silently stopped receiving updates (fixed 2026-08).
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD/.e2e-pin"
BIN="$PWD/docker-pin"

echo "== building docker-pin"
go build -o "$BIN" ./cmd/docker-pin

rm -rf "$ROOT" && mkdir -p "$ROOT"
trap 'rm -rf "$ROOT" "$BIN"' EXIT

pass=0; fail=0
check() { # check <description> <0|1>
  if [ "$2" = 1 ]; then echo "   ok   $1"; pass=$((pass + 1))
  else echo "   FAIL $1"; fail=$((fail + 1)); fi
}

image_line() { grep -E '^\s+image:' "$1" | head -1 | sed 's/^[[:space:]]*image:[[:space:]]*//'; }

# --- moving tag stays moving -------------------------------------------
cat > "$ROOT/docker-compose.yml" <<'EOF'
services:
  web:
    image: nginx:latest
EOF

echo "== pinning a service on :latest"
(cd "$ROOT" && "$BIN" pin web) | sed 's/^/   | /'
line=$(image_line "$ROOT/docker-compose.yml")
echo "   -> $line"

kept=0; [[ "$line" == nginx:latest@sha256:* ]] && kept=1
check "latest is kept as the followed tag" "$kept"

# The bug being guarded against: any concrete version tag in place of latest.
not_resolved=0; [[ "$line" != nginx:1.* && "$line" != nginx:v* ]] && not_resolved=1
check "latest not resolved to a concrete version tag" "$not_resolved"

# --- concrete tag is preserved too --------------------------------------
cat > "$ROOT/docker-compose.yml" <<'EOF'
services:
  web:
    image: nginx:1.26.3
EOF

echo "== pinning a service on a concrete tag"
(cd "$ROOT" && "$BIN" pin web) | sed 's/^/   | /'
line=$(image_line "$ROOT/docker-compose.yml")
echo "   -> $line"

concrete=0; [[ "$line" == nginx:1.26.3@sha256:* ]] && concrete=1
check "concrete tag is preserved" "$concrete"

# --- upgrade re-pins under the same tag ---------------------------------
# Start from latest pinned at a deliberately stale digest (an old 1.26.0
# index digest) so upgrade has something to move.
STALE=sha256:41b194461e4bae16f9b25d68b0976ed4735b89ca625c89aad88e1c1c3b7e8860
cat > "$ROOT/docker-compose.yml" <<EOF
services:
  web:
    image: nginx:latest@$STALE
EOF

echo "== upgrading a service on :latest"
(cd "$ROOT" && "$BIN" pin upgrade web) | sed 's/^/   | /'
line=$(image_line "$ROOT/docker-compose.yml")
echo "   -> $line"

up_kept=0; [[ "$line" == nginx:latest@sha256:* ]] && up_kept=1
check "upgrade keeps latest as the followed tag" "$up_kept"

moved=0; [[ "$line" != "nginx:latest@$STALE" ]] && moved=1
check "upgrade moved the digest" "$moved"

# --- explicit version IS a tag change -----------------------------------
echo "== upgrading with an explicit version"
(cd "$ROOT" && "$BIN" pin upgrade web 1.26.3) | sed 's/^/   | /'
line=$(image_line "$ROOT/docker-compose.yml")
echo "   -> $line"

explicit=0; [[ "$line" == nginx:1.26.3@sha256:* ]] && explicit=1
check "explicit version becomes the new followed tag" "$explicit"

# --- running container wins over a moved local tag ----------------------
# The real scenario: bring a stack up, live with it, then pin. Meanwhile the
# moving tag gets re-pulled by something else, so the local image for the tag
# is newer than what the container is running. Pin must record what runs.
echo "== pinning a running service whose tag has since moved"
# NOT under $ROOT: compose.FindFile walks up, and $ROOT has its own
# docker-compose.yml from the tests above that it would find instead.
PROJECT="$PWD/.e2e-pin-running"
rm -rf "$PROJECT"
mkdir -p "$PROJECT"
OLD_DIGEST=sha256:41b194461e4bae16f9b25d68b0976ed4735b89ca625c89aad88e1c1c3b7e8860  # nginx 1.26.3
cat > "$PROJECT/docker-compose.yml" <<EOF
services:
  web:
    image: nginx:pinsandbox
EOF

# Build the divergence by hand: tag an old image as :pinsandbox, run it, then
# repoint the tag at a newer image without touching the container.
docker pull -q "nginx@$OLD_DIGEST" >/dev/null
docker tag "nginx@$OLD_DIGEST" nginx:pinsandbox
(cd "$PROJECT" && docker compose up -d >/dev/null 2>&1)
docker pull -q nginx:latest >/dev/null
NEW_DIGEST=$(docker image inspect nginx:latest --format '{{index .RepoDigests 0}}' | sed 's/^.*@//')
docker tag nginx:latest nginx:pinsandbox   # tag now points somewhere newer
trap 'cd "$PROJECT" 2>/dev/null && docker compose down >/dev/null 2>&1; docker rmi nginx:pinsandbox e2e-pin-built:local >/dev/null 2>&1; rm -rf "$ROOT" "$PROJECT" "$BIN"' EXIT

(cd "$PROJECT" && "$BIN" pin web) | sed 's/^/   | /'
line=$(image_line "$PROJECT/docker-compose.yml")
echo "   -> $line"

ran=0; [[ "$line" == *"@$OLD_DIGEST" ]] && ran=1
check "pins the digest the container is running" "$ran"

not_newer=0; [[ "$line" != *"@$NEW_DIGEST" ]] && not_newer=1
check "does not pin the newer digest sitting on the tag" "$not_newer"

# --- --all is exactly "pin every service", same rules ------------------
# --all is a convenience for running pin against each service, so it must use
# the same digest source: running container first. Here `web` is up on the old
# image (tag since moved) and `idle` was never started.
echo "== pin --all mixes running and not-running services"
cat > "$PROJECT/docker-compose.yml" <<EOF
services:
  web:
    image: nginx:pinsandbox
  idle:
    image: nginx:1.26.3
EOF

(cd "$PROJECT" && "$BIN" pin --all) | sed 's/^/   | /'
web_line=$(grep -A1 '^  web:' "$PROJECT/docker-compose.yml" | grep image | sed 's/.*image:[[:space:]]*//')
idle_line=$(grep -A1 '^  idle:' "$PROJECT/docker-compose.yml" | grep image | sed 's/.*image:[[:space:]]*//')
echo "   -> web:  $web_line"
echo "   -> idle: $idle_line"

all_running=0; [[ "$web_line" == *"@$OLD_DIGEST" ]] && all_running=1
check "--all pins the running service from its container" "$all_running"

all_local=0; [[ "$idle_line" == nginx:1.26.3@sha256:* ]] && all_local=1
check "--all pins the stopped service from the local image" "$all_local"

# --- unknown flags must puke, not run ----------------------------------
# The bug this guards: `pin --all --dry-riun` matched --all before checking
# the argument count, dropped the mistyped flag, and rewrote every compose
# file for real. A typo in a safety flag must never become a live run.
echo "== unknown flags are rejected"
cat > "$PROJECT/docker-compose.yml" <<'EOF'
services:
  web:
    image: nginx:1.26.3
EOF
before=$(cat "$PROJECT/docker-compose.yml")

for bad in "pin --all --dry-riun" "pin web --dry-riun" "pin --typo" "pin upgrade --all --dry-riun" "pin upgrade web --dry-riun"; do
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
printf 'FROM nginx:1.26.3\n' > "$PROJECT/app/Dockerfile"
cat > "$PROJECT/docker-compose.yml" <<'EOF2'
services:
  app:
    build: ./app
    image: e2e-pin-built:local
EOF2
(cd "$PROJECT" && docker compose build -q >/dev/null 2>&1)

(cd "$PROJECT" && "$BIN" pin app) | sed 's/^/   | /'
line=$(image_line "$PROJECT/docker-compose.yml")
echo "   -> $line"

skipped=0; [[ "$line" == "e2e-pin-built:local" ]] && skipped=1
check "built service left unpinned" "$skipped"

echo "== e2e: $pass passed, $fail failed"
exit $((fail > 0))
