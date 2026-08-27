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

echo "== e2e: $pass passed, $fail failed"
exit $((fail > 0))
