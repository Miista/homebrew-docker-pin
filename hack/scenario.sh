#!/usr/bin/env bash
# Set up one scenario: lay its fixture into the testbed, publish the images it
# declares, pin the watched service and bring it up.
#
#   hack/scenario.sh beyond-policy     # stand it up and leave it running
#   hack/scenario.sh down              # remove whatever is up
#
# The integration suite sources this and calls scenario_up for each of its
# scenarios, so what a person stands up by hand is the same thing the suite
# tests -- there is no second copy of the setup to drift.
#
# Run directly it also starts duva's queue on :8099, since the reason to stand
# a scenario up by hand is usually to look at what duva made of it.

# scenario_up <name> -- everything a scenario needs, short of running duva.
#
# Sets WORK (the project) and DATA (duva's state) for the caller.
scenario_up() {
  local name="$1"
  local src="${CTX_ROOT}/hack/fixtures/${CTX_SUITE#integration-}/${name}"
  [ -d "$src" ] || { echo "no such scenario: $name" >&2; return 1; }

  WORK="$(ctx_dir work)"
  ctx_fixture "$name" "$WORK"

  # Inside the project, so the fixture can name them relatively rather than
  # through a variable only the suite could expand.
  DATA="$WORK/data"; mkdir -p "$DATA"; chmod 777 "$DATA"
  mkdir -p "$WORK/certs"; cp "$REGISTRY_CERT_DIR"/*.pem "$WORK/certs/"

  # The registry first, alone. It is a service in the fixture like any other,
  # through the shared include -- but nothing else can start until it holds
  # the image the fixture names, since a service whose image does not exist
  # cannot be created. --wait means it is accepting pushes on return.
  ctx_compose_up "$WORK" testregistry

  # The tags the fixture's own compose file refers to, so the stack can start.
  # Only those: what is AVAILABLE to update to is the variable each test is
  # about, so a test pushes that itself.
  local tag
  while read -r tag; do
    [ -n "$tag" ] || continue
    push_runnable app "$tag"
  done < "$src/images"

  ctx_compose_up "$WORK"
}

# Sourced by the suite, which drives scenarios itself.
[ "${BASH_SOURCE[0]}" = "${0}" ] || return 0

set -euo pipefail
cd "$(dirname "$0")/.."
. hack/lib/context.sh
. hack/lib/registry.sh

SCENARIO="${1:-}"
IMAGE=duva:integration-apply
PORT=8099

if [ -z "$SCENARIO" ]; then
  echo "usage: hack/scenario.sh <scenario|down>" >&2
  echo "scenarios:" >&2
  ls hack/fixtures/duva-apply | sed 's/^/  /' >&2
  exit 2
fi

ctx_init integration-duva-apply

if [ "$SCENARIO" = "down" ]; then
  ctx_sweep
  echo "removed"
  exit 0
fi

# Standing a scenario up by hand means leaving it up: the context's teardown
# trap is what a suite wants, not a person.
trap - EXIT INT TERM

registry_setup
echo "== building $IMAGE"
ctx_build_duva_image "$IMAGE"
scenario_up "$SCENARIO"

# One check, so there is something to look at.
(cd "$WORK" && docker compose up -d duva >/dev/null 2>&1)
for _ in $(seq 60); do
  case "$(cd "$WORK" && docker compose ps -a --status exited --format '{{.Service}}' 2>/dev/null)" in
    *duva*) break ;;
  esac
  sleep 0.5
done
(cd "$WORK" && docker compose logs --no-log-prefix duva 2>&1) | sed 's/^/   | /'

ctx_run duva-queue \
  --add-host=localhost:host-gateway \
  -v "$REGISTRY_CERT:/etc/ssl/certs/ca-certificates.crt:ro" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$WORK:/compose" -v "$DATA:/data" \
  -p "$PORT:8080" \
  -e DUVA_SCHEDULE="0 3 * * *" -e DUVA_HOSTNAME=sandbox \
  "$IMAGE" serve
for _ in $(seq 20); do curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.5; done

echo
echo "scenario: $SCENARIO"
echo "project:  $WORK"
echo "pinned:   $(grep -E '^\s+image:.*app' "$WORK/docker-compose.yml" | head -1 | sed 's/^ *image: *//')"
echo "queue:    http://localhost:$PORT/"
echo
echo "tear down with: hack/scenario.sh down"
