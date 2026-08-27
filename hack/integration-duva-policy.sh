#!/usr/bin/env bash
# Integration tests for duva's policy: which updates it applies unattended and
# which it queues for a human.
#
# Real registry protocol, real container, real HTTP -- all locally, against a
# registry containing exactly the tags each scenario declares. Nothing upstream
# is contacted, so these are deterministic and work offline.
#
# The unit tests fake the registry, so they cannot show that a real tag list is
# sorted and classified correctly, that labels survive a round trip through
# compose parsing, that the queue persists across restarts, or that the page
# renders over HTTP. Each of those has broken in a way no fake would notice.
set -euo pipefail

cd "$(dirname "$0")/.."
. hack/lib/context.sh
. hack/lib/registry.sh

# Everything created here is a context resource, torn down when this exits
# however it exits; see hack/lib/context.sh.
ctx_init integration-duva-policy
registry_setup

IMAGE=duva:integration-policy
NAME=integration-duva-policy
PORT=8098

echo "== building $IMAGE"
# Built from the host's Go rather than through cmd/duva/Dockerfile: that
# pulls golang: and distroless: at build time, so every run would depend on
# Docker Hub being reachable and not rate-limiting. See ctx_build_duva_image.
COVER_MOUNT=""
if [ -n "${GOCOVERDIR:-}" ]; then
  mkdir -p "$GOCOVERDIR"
  chmod 777 "$GOCOVERDIR"
  COVER_MOUNT="-v $GOCOVERDIR:/covdata -e GOCOVERDIR=/covdata"
fi
ctx_build_duva_image "$IMAGE"

scenario "policy decides what is queued"
registry_start

ROOT="$(ctx_dir project)"
mkdir -p "$ROOT/compose" "$ROOT/data"
chmod 777 "$ROOT/data"

pass=0; fail=0
check() { # check <description> <0|1>
  if [ "$2" = 1 ]; then echo "   ok   $1"; pass=$((pass + 1))
  else echo "   FAIL $1"; fail=$((fail + 1)); fi
}

# The shape this suite needs: one repo whose 1.x line has both a newer patch
# and a newer minor, so the same image gives a different verdict per service
# depending only on its include regex and threshold.
echo "== publishing test images"
push_versions app 1.26.0 1.26.3 1.31.4
STALE=$(digest_of app 1.26.0)

cat > "$ROOT/compose/docker-compose.yml" <<EOF
services:
  # patch available, duva.auto: patch -> applied unattended, never queued
  autopatch:
    image: $(internal_ref app 1.26.0)@$STALE
    labels:
      duva.include: '^1\.26\.\d+\$'
      duva.auto: patch

  # minor available, duva.auto: patch -> exceeds the threshold, queued
  exceeds:
    image: $(internal_ref app 1.26.0)@$STALE
    labels:
      duva.include: '^1\.\d+\.\d+\$'
      duva.auto: patch

  # patch available, no duva.auto -> defaults to none, queued
  defaultnone:
    image: $(internal_ref app 1.26.0)@$STALE
    labels:
      duva.include: '^1\.26\.\d+\$'

  # unpinned: not watched at all
  loose:
    image: $(internal_ref app 1.26.0)
EOF

# duva reaches the registry by name on the shared network and trusts the
# suite's certificate; see registry_container_args.
# shellcheck disable=SC2046
run() {
  docker run --rm $(registry_container_args) $COVER_MOUNT \
    -v "$ROOT/compose:/compose:ro" -v "$ROOT/data:/data" "$IMAGE" run 2>&1
}

echo "== first check"
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
# statequery reads the state file, so the suite needs no JSON parser on the
# host; see hack/lib/statequery.
queued() { "$(ctx_statequery)" "$ROOT/data/duva.json" pending; }
pending_field() { "$(ctx_statequery)" "$ROOT/data/duva.json" pending "$1" "$2"; }

got=$(queued); echo "   queue: [$got]"
c=0; [ "$got" = "defaultnone exceeds" ] && c=1
check "queue holds exactly the two needing approval" "$c"

c=0; [ -n "$(pending_field exceeds why)" ] && c=1
check "queued rows carry an explanation" "$c"

c=0; [ "$(pending_field defaultnone bump)" = "patch" ] && c=1
check "1.26.0 -> 1.26.3 classifies as patch" "$c"

c=0; [ "$(pending_field exceeds bump)" = "minor" ] && c=1
check "1.26.0 -> 1.31.4 classifies as minor" "$c"

# --- state survives a restart ------------------------------------------
before=$(queued)
run >/dev/null
c=0; [ "$(queued)" = "$before" ] && c=1
check "a second run leaves the queue unchanged" "$c"

# --- the UI serves what the state holds --------------------------------
echo "== serving the queue"
# shellcheck disable=SC2086,SC2046
ctx_run "$NAME" $(registry_container_args) $COVER_MOUNT -p "$PORT:8080" \
  -e DUVA_SCHEDULE="0 3 * * *" -e DUVA_UI_ADDR=":8080" -e DUVA_HOSTNAME=integration \
  -v "$ROOT/compose:/compose:ro" -v "$ROOT/data:/data" "$IMAGE" serve
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

echo "== integration: $pass passed, $fail failed"
exit $((fail > 0))
