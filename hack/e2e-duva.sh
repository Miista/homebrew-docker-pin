#!/usr/bin/env bash
# End-to-end test for duva against the real registry: builds the
# container image, points it at a compose file pinned to an old tag, and
# verifies it (1) detects the newer tag and notifies, (2) does not notify
# again for the same tag on a second run, and (3) never touches the compose
# file it was pointed at (duva is read-only by design).
#
# The tag pair is NOT hardcoded -- a hardcoded "old" tag drifts stale over
# time (eventually every patch on the branch is "old"), and a hardcoded pair
# risks the older tag eventually being pruned from the registry's listing.
# Instead this discovers, live, the two newest x.y.z-alpine tags currently
# published for redis (same regex duva itself is configured with below),
# pins the compose file to the OLDER of that pair, and expects duva to
# report exactly the NEWER one -- the real predecessor/successor relationship
# on whatever redis has actually published by the time this runs.
#
# Notifications are captured by a throwaway local HTTP server standing in for
# ntfy, so this needs no real ntfy account/token and no network egress beyond
# the registry lookup.
#
# Usage: hack/e2e-duva.sh   (run from the repo root; needs docker)
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD/.e2e-duva"
IMAGE=duva:e2e

echo "== building $IMAGE"
docker build -q -f cmd/duva/Dockerfile -t "$IMAGE" . >/dev/null

echo "== discovering the two newest redis x.y.z-alpine tags"
read -r OLD_TAG NEW_TAG < <(
  curl -s "https://hub.docker.com/v2/repositories/library/redis/tags?page_size=100" |
    python3 -c '
import json, re, sys
tags = json.load(sys.stdin)["results"]
pat = re.compile(r"^\d+\.\d+\.\d+-alpine$")
versioned = sorted(
    (t["name"] for t in tags if pat.match(t["name"])),
    key=lambda s: tuple(map(int, s.split("-")[0].split("."))),
)
if len(versioned) < 2:
    sys.exit("not enough versioned alpine tags found")
print(versioned[-2], versioned[-1])
'
)
echo "   predecessor: $OLD_TAG   successor: $NEW_TAG"

NTFY_PORT=8933

rm -rf "$ROOT" && mkdir -p "$ROOT/work" "$ROOT/data"
cat > "$ROOT/work/docker-compose.yml" <<EOF
services:
  redis:
    image: redis:$OLD_TAG
EOF
cat > "$ROOT/work/pin.yaml" <<EOF
schedule: "0 0 * * *"
services:
  - name: redis
    tags: '^\d+\.\d+\.\d+-alpine\$'
notify:
  ntfy:
    url: http://host.docker.internal:$NTFY_PORT
    topic: e2e
EOF

# Minimal fake ntfy: a netcat-less HTTP responder using python3 (present on
# GitHub Actions ubuntu-latest and any dev machine with Docker), logging each
# POST body to a file so the test can assert exactly one notification fired.
NTFY_LOG="$ROOT/ntfy-requests.log"
: > "$NTFY_LOG"
python3 - "$NTFY_PORT" "$NTFY_LOG" >"$ROOT/ntfy-server.log" 2>&1 <<'PYEOF' &
import http.server, sys, threading

port, log_path = int(sys.argv[1]), sys.argv[2]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode(errors="replace")
        with open(log_path, "a") as f:
            f.write(self.headers.get("Title", "") + "|" + body + "\n")
        self.send_response(200)
        self.end_headers()
    def log_message(self, *a):
        pass

httpd = http.server.HTTPServer(("0.0.0.0", port), Handler)
httpd.serve_forever()
PYEOF
NTFY_PID=$!
trap 'kill $NTFY_PID 2>/dev/null || true; wait $NTFY_PID 2>/dev/null || true; rm -rf "$ROOT"' EXIT

sleep 0.5 # let the fake ntfy bind before duva can reach it

pass=0 fail=0
check() { # $1 = description, $2 = condition (already evaluated as 0/1)
  if [ "$2" = 1 ]; then echo "   PASS: $1"; pass=$((pass+1))
  else echo "   FAIL: $1"; fail=$((fail+1)); fi
}

# state_value <file> <key>: prints state.json's value for key, or nothing if
# the file/key is missing -- used to assert the exact stored value, not just
# that some matching substring appears somewhere in the file.
state_value() {
  python3 -c '
import json, sys
try:
    with open(sys.argv[1]) as f:
        data = json.load(f)
except (FileNotFoundError, json.JSONDecodeError):
    sys.exit(0)
print(data.get(sys.argv[2], ""), end="")
' "$1" "$2"
}

before_compose_hash=$(sha256sum "$ROOT/work/docker-compose.yml" | awk '{print $1}')

echo "== first run: expect a newer tag detected and one notification sent"
docker run --rm --add-host=host.docker.internal:host-gateway \
  -v "$ROOT/work:/work" -w /work -v "$ROOT/data:/data" \
  "$IMAGE" run 2>&1 | sed 's/^/   | /'

after_compose_hash=$(sha256sum "$ROOT/work/docker-compose.yml" | awk '{print $1}')
same_hash=0; [ "$before_compose_hash" = "$after_compose_hash" ] && same_hash=1
check "compose file untouched" "$same_hash"

notif_count=$(wc -l < "$NTFY_LOG" | tr -d ' ')
one_notif=0; [ "$notif_count" = 1 ] && one_notif=1
check "exactly one notification sent" "$one_notif"

mentions_new_tag=0; grep -q "$NEW_TAG" "$NTFY_LOG" && mentions_new_tag=1
check "notification names the discovered successor tag ($NEW_TAG)" "$mentions_new_tag"

state_written=0; [ -f "$ROOT/data/duva.json" ] && state_written=1
check "state file written" "$state_written"

redis_state=$(state_value "$ROOT/data/duva.json" redis)
state_has_new_tag=0; [ "$redis_state" = "$NEW_TAG" ] && state_has_new_tag=1
check "state[redis] is exactly the discovered successor tag ($NEW_TAG, got '$redis_state')" "$state_has_new_tag"

echo "== second run: expect no repeat notification for the same tag"
docker run --rm --add-host=host.docker.internal:host-gateway \
  -v "$ROOT/work:/work" -w /work -v "$ROOT/data:/data" \
  "$IMAGE" run 2>&1 | sed 's/^/   | /'

notif_count2=$(wc -l < "$NTFY_LOG" | tr -d ' ')
still_one=0; [ "$notif_count2" = 1 ] && still_one=1
check "no repeat notification (still exactly one)" "$still_one"

# --- moving-tag scenario: unconstrained service (no tags: regex) ---
# duva has no local baseline for a moving tag (nothing is pulled), so the
# first check must record the remote digest silently, then only notify once
# a later check sees a different digest.
echo "== moving-tag scenario: unconstrained service, no baseline yet"
MROOT="$ROOT-moving"
rm -rf "$MROOT" && mkdir -p "$MROOT/work" "$MROOT/data"
cat > "$MROOT/work/docker-compose.yml" <<EOF
services:
  redis:
    image: redis:alpine
EOF
MNTFY_PORT=8934
cat > "$MROOT/work/pin.yaml" <<EOF
schedule: "0 0 * * *"
services:
  - redis
notify:
  ntfy:
    url: http://host.docker.internal:$MNTFY_PORT
    topic: e2e
EOF

MNTFY_LOG="$MROOT/ntfy-requests.log"
: > "$MNTFY_LOG"
python3 - "$MNTFY_PORT" "$MNTFY_LOG" >"$MROOT/ntfy-server.log" 2>&1 <<'PYEOF' &
import http.server, sys

port, log_path = int(sys.argv[1]), sys.argv[2]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode(errors="replace")
        with open(log_path, "a") as f:
            f.write(self.headers.get("Title", "") + "|" + body + "\n")
        self.send_response(200)
        self.end_headers()
    def log_message(self, *a):
        pass

http.server.HTTPServer(("0.0.0.0", port), Handler).serve_forever()
PYEOF
MNTFY_PID=$!
trap 'kill $NTFY_PID $MNTFY_PID 2>/dev/null || true; wait $NTFY_PID $MNTFY_PID 2>/dev/null || true; rm -rf "$ROOT" "$MROOT"' EXIT
sleep 0.5

docker run --rm --add-host=host.docker.internal:host-gateway \
  -v "$MROOT/work:/work" -w /work -v "$MROOT/data:/data" \
  "$IMAGE" run 2>&1 | sed 's/^/   | /'

no_notif_yet=0; [ "$(wc -l < "$MNTFY_LOG" | tr -d ' ')" = 0 ] && no_notif_yet=1
check "moving tag: no notification on first check (no baseline yet)" "$no_notif_yet"

moving_state=$(state_value "$MROOT/data/duva.json" redis)
baseline_written=0
case "$moving_state" in sha256:*) baseline_written=1 ;; esac
check "moving tag: state[redis] holds a sha256 digest baseline (got '$moving_state')" "$baseline_written"

echo "== moving-tag scenario: second run with an unchanged digest must not notify"
docker run --rm --add-host=host.docker.internal:host-gateway \
  -v "$MROOT/work:/work" -w /work -v "$MROOT/data:/data" \
  "$IMAGE" run 2>&1 | sed 's/^/   | /'

still_no_notif=0; [ "$(wc -l < "$MNTFY_LOG" | tr -d ' ')" = 0 ] && still_no_notif=1
check "moving tag: no notification while the digest is unchanged" "$still_no_notif"

moving_state2=$(state_value "$MROOT/data/duva.json" redis)
baseline_unchanged=0; [ "$moving_state2" = "$moving_state" ] && baseline_unchanged=1
check "moving tag: recorded baseline unchanged across the unchanged-digest run" "$baseline_unchanged"

echo "== e2e: $pass passed, $fail failed"
exit $((fail > 0))
