# A disposable context for an integration suite.
#
# Everything a suite creates -- containers, compose projects, images, temp
# directories, the registry itself -- is registered here as it is created, and
# torn down when the suite exits, however it exits. Cleanup is not something a
# test has to remember, and a leftover container from a previous run cannot
# influence the next one.
#
# That is not hypothetical: a stale container survived an earlier run, compose
# reused it because its config still matched, and the suite pinned the wrong
# image for two hours before anyone noticed.
#
# Every resource is also labelled and prefixed with the suite name, so a run
# that is killed outright (SIGKILL, a closed laptop) can still be swept up by
# ctx_sweep on the next run.

CTX_LABEL="io.github.miista.docker-pin.integration"

# ctx_init <suite-name> -- start a context. Registers the exit trap.
ctx_init() {
  CTX_SUITE="$1"
  # Working directories live under the repo; see ctx_dir.
  CTX_ROOT="${CTX_ROOT:-$PWD}"
  CTX_SCENARIO=""
  CTX_RUN_DIRS=()
  CTX_RUN_IMAGES=()
  ctx_reset_registers

  # Anything left by a run that died before its trap fired.
  ctx_sweep

  trap ctx_teardown EXIT INT TERM
}

ctx_reset_registers() {
  CTX_DIRS=()
  CTX_PROJECTS=()
  CTX_CONTAINERS=()
  CTX_IMAGES=()
  CTX_NETWORKS=()
}

# scenario <description> -- begin a scenario.
#
# Every scenario starts from nothing and leaves nothing: whatever the previous
# one created -- containers, compose projects, images, networks, directories --
# is removed before this one begins. Isolation is then a property of the
# harness rather than a convention about picking distinct names, and no
# scenario can pass because of state another left behind.
#
# The cost is small: the images are FROM scratch and the registry is on
# loopback, so a full teardown and rebuild is a fraction of a second.
scenario() {
  ctx_release
  CTX_SCENARIO="$1"
  echo "== $1"
}

# ctx_sweep removes anything belonging to this suite -- containers by label,
# working directories by name prefix -- regardless of which run created them.
# A run killed outright never fires its trap, so the next one cleans up after
# it rather than accumulating.
ctx_sweep() {
  local stale
  stale=$(docker ps -aq --filter "label=${CTX_LABEL}=${CTX_SUITE}" 2>/dev/null || true)
  if [ -n "$stale" ]; then
    echo "   (sweeping ${CTX_LABEL}=${CTX_SUITE} containers from a previous run)"
    # shellcheck disable=SC2086
    docker rm -f $stale >/dev/null 2>&1 || true
  fi

  # ctx_dir names every directory ".<suite>-<suffix>.XXXXXX" under CTX_ROOT.
  local leftovers
  leftovers=$(find "${CTX_ROOT}" -maxdepth 1 -type d -name ".${CTX_SUITE}-*" 2>/dev/null || true)
  if [ -n "$leftovers" ]; then
    echo "   (sweeping ${CTX_SUITE} directories from a previous run)"
    printf '%s\n' "$leftovers" | while read -r d; do [ -n "$d" ] && rm -rf "$d"; done
  fi
}

# ctx_dir [suffix] -- a working directory that is removed on teardown.
#
# Under the repo, not $TMPDIR. On macOS $TMPDIR lives beneath /var, which is a
# symlink to /private/var, so docker records a resolved path in
# com.docker.compose.project.working_dir that never matches the unresolved one
# a script holds -- and any lookup by working_dir silently finds nothing. The
# repo path has no such indirection.
#
# Safe to nest here because the repo root has no compose file of its own, so
# compose.FindFile walking upwards cannot stray out of the project.
ctx_dir() {
  local dir
  dir="$(mktemp -d "${CTX_ROOT}/.${CTX_SUITE}${1:+-$1}.XXXXXX")"
  CTX_DIRS+=("$dir")
  printf '%s' "$dir"
}

# ctx_run_dir [suffix] -- like ctx_dir, but survives scenario boundaries and
# is removed only when the run ends. For artefacts that are the same for every
# scenario, such as the registry certificate.
# ctx_run_image <ref> -- register an image that outlives scenario boundaries
# and is removed only when the run ends. For test tooling, which is identical
# for every scenario: rebuilding a byte-identical binary per scenario is pure
# cost, and nothing a scenario asserts depends on it being fresh.
ctx_run_image() {
  CTX_RUN_IMAGES+=("$1")
}

ctx_run_dir() {
  local dir
  dir="$(mktemp -d "${CTX_ROOT}/.${CTX_SUITE}${1:+-$1}.XXXXXX")"
  CTX_RUN_DIRS+=("$dir")
  printf '%s' "$dir"
}

# ctx_container <name> -- register a container for removal. Callers should also
# pass --label "$CTX_LABEL=$CTX_SUITE" to docker run, so a killed run is still
# sweepable.
ctx_container() {
  CTX_CONTAINERS+=("$1")
}

# ctx_run <name> [docker run args...] -- docker run, labelled and registered.
ctx_run() {
  local name="$1"; shift
  ctx_container "$name"
  docker run -d --name "$name" --label "${CTX_LABEL}=${CTX_SUITE}" "$@" >/dev/null
}

# ctx_compose_up <dir> -- compose up in dir, registering the project so it is
# torn down. The project is named after the suite and the directory, so two
# suites cannot collide.
ctx_compose_up() {
  local dir="$1"
  CTX_PROJECTS+=("$dir")
  (cd "$dir" && docker compose up -d --remove-orphans >/dev/null 2>&1)
}

# ctx_image <ref> -- register a locally tagged image for removal, so a rerun
# cannot reuse a stale layer under the same tag.
ctx_image() {
  CTX_IMAGES+=("$1")
}

# ctx_network <name> -- create a network and register it for removal.
ctx_network() {
  local name="$1"
  docker network rm "$name" >/dev/null 2>&1 || true
  docker network create --label "${CTX_LABEL}=${CTX_SUITE}" "$name" >/dev/null
  CTX_NETWORKS+=("$name")
}

# ctx_release removes everything registered so far, without ending the run.
# This is what a scenario boundary uses; teardown calls it too.
ctx_release() {
  local dir name ref net

  for dir in "${CTX_PROJECTS[@]:-}"; do
    [ -n "$dir" ] && [ -d "$dir" ] &&
      (cd "$dir" && docker compose down --remove-orphans --timeout 3 >/dev/null 2>&1) || true
  done

  for name in "${CTX_CONTAINERS[@]:-}"; do
    [ -n "$name" ] && docker rm -f "$name" >/dev/null 2>&1 || true
  done

  # Local images too: the daemon's cache outlives the registry, so an image
  # left under a repo:tag a later scenario reuses would be read instead of
  # the one that scenario pushed.
  for ref in "${CTX_IMAGES[@]:-}"; do
    [ -n "$ref" ] && docker rmi -f "$ref" >/dev/null 2>&1 || true
  done

  for net in "${CTX_NETWORKS[@]:-}"; do
    [ -n "$net" ] && docker network rm "$net" >/dev/null 2>&1 || true
  done

  for dir in "${CTX_DIRS[@]:-}"; do
    [ -n "$dir" ] && rm -rf "$dir"
  done

  ctx_reset_registers
}

# ctx_teardown removes everything registered and ends the run.
ctx_teardown() {
  local status=$?
  trap - EXIT INT TERM

  ctx_release

  local ref
  for ref in "${CTX_RUN_IMAGES[@]:-}"; do
    [ -n "$ref" ] && docker rmi -f "$ref" >/dev/null 2>&1 || true
  done

  local rundir
  for rundir in "${CTX_RUN_DIRS[@]:-}"; do
    [ -n "$rundir" ] && rm -rf "$rundir"
  done

  # Belt and braces: anything labelled for this suite that was never
  # registered (a container compose created under its own name, say).
  ctx_sweep

  return $status
}

# ctx_statequery -- builds the state-file query helper once per run and prints
# its path. Used instead of a JSON parser on the host: Go is already required
# to build what is under test, python3 is not.
ctx_statequery() {
  if [ -z "${CTX_STATEQUERY:-}" ]; then
    local dir
    dir="$(ctx_run_dir statequery)"
    CTX_STATEQUERY="$dir/statequery"
    (cd "$CTX_ROOT" && go build -o "$CTX_STATEQUERY" ./hack/lib/statequery)
  fi
  printf '%s' "$CTX_STATEQUERY"
}

# ctx_build_duva_image <tag> -- builds a duva image without pulling anything.
#
# The shipped Dockerfile builds inside golang:1.22-alpine and lands on
# distroless -- correct for a release, but it means every suite run depends on
# Docker Hub being reachable and not rate-limiting. Here the binary is built on
# the host (Go is already required to build what is under test) and copied into
# a scratch image, so the suite pulls nothing.
#
# The result differs from the shipped image in one way that matters: no CA
# bundle. Suites that need one mount it, which they do anyway to trust the test
# registry's certificate.
ctx_build_duva_image() {
  local tag="$1" dir
  dir="$(ctx_run_dir duvaimg)"

  local flags=(-trimpath)
  if [ -n "${GOCOVERDIR:-}" ]; then
    flags+=(-cover -coverpkg=./...)
  else
    flags+=(-ldflags "-s -w")
  fi

  (cd "$CTX_ROOT" && CGO_ENABLED=0 GOOS=linux go build "${flags[@]}" -o "$dir/duva" ./cmd/duva)
  printf 'FROM scratch\nCOPY duva /duva\nENTRYPOINT ["/duva"]\nCMD ["serve"]\n' > "$dir/Dockerfile"
  docker build -q -t "$tag" "$dir" >/dev/null
  ctx_run_image "$tag"
}
