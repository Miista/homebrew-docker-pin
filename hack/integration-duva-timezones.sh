#!/usr/bin/env bash
# Integration tests for duva rendering times in the configured timezone.
#
# Its own suite because this is about how duva reads its environment, not
# about the update transaction: nothing here pins, recreates or commits.
#
# It also cannot be a unit test. duva ships on an image with no
# /usr/share/zoneinfo, so Go cannot resolve a zone name from the filesystem
# and falls back to UTC -- silently, so TZ looks applied. A unit test would
# load the zone from the developer's own machine, which has zoneinfo, and pass
# whether or not the binary embeds the database.
set -euo pipefail

cd "$(dirname "$0")/.."
. hack/lib/context.sh
. hack/lib/registry.sh
. hack/scenario.sh

ctx_init integration-duva-timezones
registry_setup

IMAGE=duva:integration-timezones
echo "== building $IMAGE"
ctx_build_duva_image "$IMAGE"

pass=0; fail=0
check() { # check <description> <0|1>
  if [ "$2" = 1 ]; then echo "   ok   $1"; pass=$((pass + 1))
  else echo "   FAIL $1"; fail=$((fail + 1)); fi
}

# --- two zones, one instant ----------------------------------------------
scenario "the configured timezone is honoured"
scenario_up two-zones

# Two duvas, same moment, zones ten hours apart, both brought up by
# scenario_up. Each announces when it will next check, in its own local time.

next_check() { # next_check <service> -- its "next check at" line
  local i line
  for i in $(seq 40); do
    line=$( (cd "$WORK" && docker compose logs --no-log-prefix "$1" 2>&1) | grep "next check at" | head -1)
    [ -n "$line" ] && { printf '%s' "$line"; return 0; }
    sleep 0.25
  done
}

cph=$(next_check duva_copenhagen)
akl=$(next_check duva_auckland)
echo "   | $cph"
echo "   | $akl"

# The difference is the assertion. One zone alone could match by coincidence,
# and two zones agreeing would mean both fell back to UTC -- which is exactly
# the failure being guarded against.
c=0; [ -n "$cph" ] && [ -n "$akl" ] && [ "$cph" != "$akl" ] && c=1
check "two timezones produce two different times" "$c"

c=0; [[ "$cph" == *CEST* || "$cph" == *CET* ]] && c=1
check "Europe/Copenhagen is named in its output" "$c"

c=0; [[ "$akl" == *NZST* || "$akl" == *NZDT* ]] && c=1
check "Pacific/Auckland is named in its output" "$c"

echo "== integration: $pass passed, $fail failed"
exit $((fail > 0))
