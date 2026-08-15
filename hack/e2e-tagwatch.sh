#!/usr/bin/env bash
# End-to-end test for tagwatch against the real registry: builds the
# container image, points it at a compose file pinned to an old tag, and
# verifies it (1) detects the newer tag and notifies, (2) does not notify
# again for the same tag on a second run, and (3) never touches the compose
# file it was pointed at (tagwatch is read-only by design).
#
# Uses redis:7.4-alpine -> a newer 7.x-alpine tag on Docker Hub: small, public,
# no auth needed, and reliably has newer patch releases to find.
#
# Notifications are captured by a throwaway local HTTP server standing in for
# ntfy, so this needs no real ntfy account/token and no network egress beyond
# the registry lookup.
#
# Usage: hack/e2e-tagwatch.sh   (run from the repo root; needs docker)
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD/.e2e-tagwatch"
IMAGE=tagwatch:e2e
OLD_TAG=7.4-alpine

echo "== building $IMAGE"
docker build -q -f cmd/tagwatch/Dockerfile -t "$IMAGE" . >/dev/null

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
    tags: '^\d+\.\d+-alpine\$'
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

sleep 0.5 # let the fake ntfy bind before tagwatch can reach it

pass=0 fail=0
check() { # $1 = description, $2 = condition (already evaluated as 0/1)
  if [ "$2" = 1 ]; then echo "   PASS: $1"; pass=$((pass+1))
  else echo "   FAIL: $1"; fail=$((fail+1)); fi
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

mentions_service=0; grep -q "redis" "$NTFY_LOG" && mentions_service=1
check "notification mentions the service" "$mentions_service"

state_written=0; [ -f "$ROOT/data/tagwatch.json" ] && state_written=1
check "state file written" "$state_written"

state_has_redis=0; grep -q '"redis"' "$ROOT/data/tagwatch.json" 2>/dev/null && state_has_redis=1
check "state file records redis" "$state_has_redis"

echo "== second run: expect no repeat notification for the same tag"
docker run --rm --add-host=host.docker.internal:host-gateway \
  -v "$ROOT/work:/work" -w /work -v "$ROOT/data:/data" \
  "$IMAGE" run 2>&1 | sed 's/^/   | /'

notif_count2=$(wc -l < "$NTFY_LOG" | tr -d ' ')
still_one=0; [ "$notif_count2" = 1 ] && still_one=1
check "no repeat notification (still exactly one)" "$still_one"

echo "== e2e: $pass passed, $fail failed"
exit $((fail > 0))
