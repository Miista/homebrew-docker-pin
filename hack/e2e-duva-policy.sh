#!/usr/bin/env bash
# e2e for duva's detection and policy, against real registries and a real
# container.
#
# The unit tests fake the registry, so they cannot catch what this exists for:
# real tags being listed, sorted and classified; labels surviving a round trip
# through compose parsing; the state file surviving a restart; and the queue
# rendering over HTTP. Each of those has broken in a way no fake would notice.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD/.e2e-duva-policy"
IMAGE=duva:e2e-policy
NAME=duva-e2e-policy
PORT=8098

echo "== building $IMAGE"
# With GOCOVERDIR set, build and run an instrumented binary so this suite's
# coverage merges with the unit tests' (go.dev/blog/integration-test-coverage).
COVER_ARGS=""
COVER_MOUNT=""
if [ -n "${GOCOVERDIR:-}" ]; then
  mkdir -p "$GOCOVERDIR"
  chmod 777 "$GOCOVERDIR"
  COVER_ARGS="--build-arg COVER=1"
  # Run as the invoking user: the image is distroless-nonroot, which cannot
  # write to a host-mounted directory. (On macOS the directory must also be
  # under a path Docker Desktop shares -- /tmp is not one.)
  COVER_MOUNT="-v $GOCOVERDIR:/covdata -e GOCOVERDIR=/covdata --user $(id -u):$(id -g)"
fi
docker build -q $COVER_ARGS -f cmd/duva/Dockerfile -t "$IMAGE" . >/dev/null

rm -rf "$ROOT" && mkdir -p "$ROOT/compose" "$ROOT/data"
chmod 777 "$ROOT/data"
# stop before rm: SIGKILL would lose the instrumented binary's coverage,
# which is only flushed on a clean exit.
trap 'docker stop -t 5 "$NAME" >/dev/null 2>&1 || true; docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$ROOT"' EXIT

pass=0; fail=0
check() { # check <description> <0|1>
  if [ "$2" = 1 ]; then echo "   ok   $1"; pass=$((pass + 1))
  else echo "   FAIL $1"; fail=$((fail + 1)); fi
}

# Real nginx tags. 1.26.0 is stale within its own line (1.26.3 exists) and
# far behind the newest 1.x, so the same image gives both a patch and a minor
# depending only on the include regex.
STALE=sha256:41b194461e4bae16f9b25d68b0976ed4735b89ca625c89aad88e1c1c3b7e8860

cat > "$ROOT/compose/docker-compose.yml" <<EOF
services:
  # patch available, duva.auto: patch -> applied unattended, never queued
  autopatch:
    image: nginx:1.26.0@$STALE
    labels:
      duva.include: '^1\.26\.\d+\$'
      duva.auto: patch

  # minor available, duva.auto: patch -> exceeds the threshold, queued
  exceeds:
    image: nginx:1.26.0@$STALE
    labels:
      duva.include: '^1\.\d+\.\d+\$'
      duva.auto: patch

  # patch available, no duva.auto -> defaults to none, queued
  defaultnone:
    image: nginx:1.26.0@$STALE
    labels:
      duva.include: '^1\.26\.\d+\$'

  # unpinned: not watched at all
  loose:
    image: nginx:1.26.0
EOF

run() { docker run --rm $COVER_MOUNT -v "$ROOT/compose:/compose:ro" -v "$ROOT/data:/data" "$IMAGE" run 2>&1; }

echo "== first check against real registry data"
out=$(run)
echo "$out" | sed 's/^/   | /'

# --- classification of real tags ---------------------------------------
c=0; echo "$out" | grep -q "autopatch: 1.26.3 available" && c=1
check "1.26.0 -> 1.26.3 found within the 1.26 line" "$c"

c=0; echo "$out" | grep -qE "exceeds: 1\.[0-9]+\.[0-9]+ available" && c=1
check "a wider regex finds a newer minor of the same image" "$c"

c=0; echo "$out" | grep -q "autopatch:.*would apply" && c=1
check "patch within duva.auto: patch would apply" "$c"

c=0; echo "$out" | grep -q "exceeds:.*needs approval" && c=1
check "minor beyond duva.auto: patch needs approval" "$c"

c=0; echo "$out" | grep -q "defaultnone:.*needs approval" && c=1
check "absent duva.auto defaults to none" "$c"

c=0; echo "$out" | grep -q "loose: not pinned, skipping" && c=1
check "unpinned service is skipped" "$c"

# --- the queue holds only what needs a human ---------------------------
queued() { python3 -c "
import json,sys
d=json.load(open('$ROOT/data/duva.json'))
print(' '.join(sorted(d['pending'])))
"; }

got=$(queued); echo "   queue: [$got]"
c=0; [ "$got" = "defaultnone exceeds" ] && c=1
check "queue holds exactly the two needing approval" "$c"

c=0; python3 -c "
import json,sys
d=json.load(open('$ROOT/data/duva.json'))
sys.exit(0 if d['pending']['exceeds'].get('why') else 1)
" && c=1
check "queued rows carry an explanation" "$c"

c=0; python3 -c "
import json,sys
d=json.load(open('$ROOT/data/duva.json'))
sys.exit(0 if d['pending']['defaultnone'].get('bump') == 'patch' else 1)
" && c=1
check "a real 1.26.0 -> 1.26.3 classifies as patch" "$c"

c=0; python3 -c "
import json,sys
d=json.load(open('$ROOT/data/duva.json'))
sys.exit(0 if d['pending']['exceeds'].get('bump') == 'minor' else 1)
" && c=1
check "a real 1.26.0 -> 1.3x classifies as minor" "$c"

# --- state survives a restart ------------------------------------------
before=$(queued)
run >/dev/null
c=0; [ "$(queued)" = "$before" ] && c=1
check "a second run leaves the queue unchanged" "$c"

# --- the UI serves what the state holds --------------------------------
echo "== serving the queue"
docker run -d --name "$NAME" $COVER_MOUNT -p "$PORT:8080" \
  -e DUVA_SCHEDULE="0 3 * * *" -e DUVA_UI_ADDR=":8080" -e DUVA_HOSTNAME=e2e \
  -v "$ROOT/compose:/compose:ro" -v "$ROOT/data:/data" "$IMAGE" serve >/dev/null
for _ in $(seq 20); do curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.5; done

page=$(curl -s "http://localhost:$PORT/")
c=0; echo "$page" | grep -q "2 waiting" && c=1
check "UI reports two waiting" "$c"

c=0; echo "$page" | grep -q "kind-minor" && c=1
check "UI shows the minor classification" "$c"

c=0; ! echo "$page" | grep -q ">autopatch<" && c=1
check "auto-applied service never appears in the queue" "$c"

c=0; ! echo "$page" | grep -q ">loose<" && c=1
check "unwatched service never appears in the queue" "$c"

echo "== e2e: $pass passed, $fail failed"
exit $((fail > 0))
