#!/usr/bin/env bash
# Integration tests for duva's detection and notification behaviour.
#
# Real registry protocol, real container, real HTTP notifications -- all
# locally, against a registry containing exactly the tags each scenario
# declares. Nothing upstream is contacted, so these are deterministic and work
# offline.
#
# The unit tests fake the registry, so what they cannot show is what duva
# actually announced: whether a notification fired at all, fired once rather
# than every run, and named the right thing. That needs something on the other
# end of the HTTP request.
set -euo pipefail

cd "$(dirname "$0")/.."
. hack/lib/context.sh
. hack/lib/registry.sh
. hack/lib/ntfy.sh

ctx_init integration-duva
registry_setup
ntfy_setup

IMAGE=duva:integration

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

# duva_run <compose-dir> <data-dir> -- one check, as the container runs it.
# It reaches the registry by name over TLS and posts notifications to the fake
# receiver, both on the shared network.
# shellcheck disable=SC2046,SC2086
duva_run() {
  docker run --rm --user "$(id -u):$(id -g)" \
    $(registry_container_args) $(ntfy_env) $COVER_MOUNT \
    -e DUVA_HOSTNAME=integration \
    -v "$1:/compose:ro" -v "$2:/data" \
    "$IMAGE" run 2>&1
}

# state_value <file> <service> -- what duva remembers about a service. A
# constrained service records its last-notified TAG; a moving tag records its
# digest baseline.
state_value() {
  "$(ctx_statequery)" "$1" remembers "$2"
}

# --- a newer tag is announced once ---------------------------------------
scenario "a newer constrained tag is announced exactly once"
registry_start
ntfy_start
push_versions app 1.0.0-alpine 1.1.0-alpine

WORK="$(ctx_dir work)"; mkdir -p "$WORK/compose" "$WORK/data"
chmod 777 "$WORK/data"
cat > "$WORK/compose/docker-compose.yml" <<EOF
services:
  app:
    image: $(internal_ref app 1.0.0-alpine)@$(digest_of app 1.0.0-alpine)
    labels:
      duva.include: '^\d+\.\d+\.\d+-alpine\$'
EOF
before_hash=$(shasum -a 256 "$WORK/compose/docker-compose.yml" | awk '{print $1}')

out=$(duva_run "$WORK/compose" "$WORK/data")
echo "$out" | sed 's/^/   | /'

c=0; echo "$out" | grep -q "app: 1.1.0-alpine available" && c=1
check "the newer tag is detected" "$c"

c=0; [ "$(shasum -a 256 "$WORK/compose/docker-compose.yml" | awk '{print $1}')" = "$before_hash" ] && c=1
check "the compose file is untouched -- duva only reports" "$c"

c=0; [ "$(ntfy_count)" = 1 ] && c=1
check "exactly one notification is sent (got $(ntfy_count))" "$c"

c=0; ntfy_mentions "1.1.0-alpine" && c=1
check "the notification names the successor tag" "$c"

c=0; [ "$(state_value "$WORK/data/duva.json" app)" = "1.1.0-alpine" ] && c=1
check "the successor is recorded as notified" "$c"

# The same candidate on a later run must not be announced again: a tool that
# repeats itself hourly gets muted, and then real news is missed too.
duva_run "$WORK/compose" "$WORK/data" >/dev/null
c=0; [ "$(ntfy_count)" = 1 ] && c=1
check "a second run does not repeat the notification (got $(ntfy_count))" "$c"

# --- a moving tag records a baseline silently ----------------------------
scenario "a moving tag records its baseline without announcing anything"
registry_start
ntfy_start
push_moving app latest first

WORK="$(ctx_dir work)"; mkdir -p "$WORK/compose" "$WORK/data"
chmod 777 "$WORK/data"
cat > "$WORK/compose/docker-compose.yml" <<EOF
services:
  app:
    image: $(internal_ref app latest)@$(digest_of app latest)
EOF

duva_run "$WORK/compose" "$WORK/data" | sed 's/^/   | /'

c=0; [ "$(ntfy_count)" = 0 ] && c=1
check "nothing is announced on the first sight of a moving tag" "$c"

baseline=$(state_value "$WORK/data/duva.json" app)
c=0; [[ "$baseline" == sha256:* ]] && c=1
check "a digest baseline is recorded (got '${baseline:0:20}')" "$c"

# Unchanged: still silent, and the baseline does not drift.
duva_run "$WORK/compose" "$WORK/data" >/dev/null
c=0; [ "$(ntfy_count)" = 0 ] && c=1
check "an unmoved tag stays silent" "$c"

c=0; [ "$(state_value "$WORK/data/duva.json" app)" = "$baseline" ] && c=1
check "the baseline is unchanged while the tag has not moved" "$c"

# --- a moved digest is announced -----------------------------------------
scenario "a moved digest is announced"
registry_start
ntfy_start
push_moving app latest first

WORK="$(ctx_dir work)"; mkdir -p "$WORK/compose" "$WORK/data"
chmod 777 "$WORK/data"
cat > "$WORK/compose/docker-compose.yml" <<EOF
services:
  app:
    image: $(internal_ref app latest)@$(digest_of app latest)
EOF

duva_run "$WORK/compose" "$WORK/data" >/dev/null   # records the baseline
ntfy_reset

push_moving app latest second                       # the tag moves
out=$(duva_run "$WORK/compose" "$WORK/data")
echo "$out" | sed 's/^/   | /'

c=0; echo "$out" | grep -q "app: sha256:.* available" && c=1
check "the digest move is detected" "$c"

c=0; [ "$(ntfy_count)" = 1 ] && c=1
check "the move is announced once (got $(ntfy_count))" "$c"

# --- pin status is the opt-in --------------------------------------------
scenario "an unpinned service is not watched at all"
registry_start
ntfy_start
push_versions app 1.0.0-alpine 1.1.0-alpine

WORK="$(ctx_dir work)"; mkdir -p "$WORK/compose" "$WORK/data"
chmod 777 "$WORK/data"
cat > "$WORK/compose/docker-compose.yml" <<EOF
services:
  app:
    image: $(internal_ref app 1.0.0-alpine)
    labels:
      duva.include: '^\d+\.\d+\.\d+-alpine\$'
EOF

out=$(duva_run "$WORK/compose" "$WORK/data")
echo "$out" | sed 's/^/   | /'

c=0; echo "$out" | grep -q "app: not pinned, skipping" && c=1
check "the service is reported as skipped" "$c"

c=0; [ "$(ntfy_count)" = 0 ] && c=1
check "nothing is announced for an unwatched service" "$c"

c=0; [ -z "$(state_value "$WORK/data/duva.json" app)" ] && c=1
check "nothing is recorded for an unwatched service" "$c"

echo "== integration: $pass passed, $fail failed"
exit $((fail > 0))
