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
# Cleaning up is two operations: bring down every stack in the testbed, then
# delete the testbed. Nothing has to be remembered as it is created, because
# everything a suite builds lives in one known place under a name derived from
# its directory -- which is also the name compose gives the project.
ctx_sweep() {
  local dir
  for dir in "${CTX_ROOT}"/testbed/"${CTX_SUITE}"*/; do
    [ -f "$dir/docker-compose.yml" ] || continue
    (cd "$dir" && docker compose down --remove-orphans --volumes --timeout 3 >/dev/null 2>&1) || true
  done

  # Containers a suite started outside compose -- the registry, the ntfy fake
  # -- carry the suite label instead.
  local stale
  stale=$(docker ps -aq --filter "label=${CTX_LABEL}=${CTX_SUITE}" 2>/dev/null || true)
  if [ -n "$stale" ]; then
    # shellcheck disable=SC2086
    docker rm -f $stale >/dev/null 2>&1 || true
  fi

  # Images this suite built, including the ones left dangling when a tag was
  # rebuilt and repointed. A dangling layer under a repo a later suite reuses
  # is exactly the stale cache these tests must not read.
  local images
  images=$(docker images -q --filter "label=${CTX_LABEL}=${CTX_SUITE}" 2>/dev/null || true)
  if [ -n "$images" ]; then
    # shellcheck disable=SC2086
    docker rmi -f $images >/dev/null 2>&1 || true
  fi

  rm -rf "${CTX_ROOT}/testbed/${CTX_SUITE}"*
  return 0
}

# ctx_dir [suffix] -- a working directory that is removed on teardown.
#
# Under the repo in testbed/, not $TMPDIR. On macOS $TMPDIR lives beneath /var,
# which is a symlink to /private/var, so docker records a resolved path in
# com.docker.compose.project.working_dir that never matches the unresolved one
# a script holds -- and any lookup by working_dir silently finds nothing. The
# repo path has no such indirection.
#
# The name is fixed, never mktemp'd. Compose derives a project name from the
# directory holding the compose file, so a random directory means a project
# name nothing can predict -- and therefore a project that teardown cannot
# find and clean up. Runs are sequential and each scenario clears the previous
# one, so there is nothing for a unique name to protect against.
#
# Safe to nest here because the repo root has no compose file of its own, so
# compose.FindFile walking upwards cannot stray out of the project.
ctx_dir() {
  local dir="${CTX_ROOT}/testbed/${CTX_SUITE}${1:+-$1}"
  rm -rf "$dir"
  mkdir -p "$dir"
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
  local dir="${CTX_ROOT}/testbed/${CTX_SUITE}${1:+-$1}"
  rm -rf "$dir"
  mkdir -p "$dir"
  CTX_RUN_DIRS+=("$dir")
  printf '%s' "$dir"
}

# ctx_docker_pin -- path to a built docker-pin, built once per run.
#
# Suites that need a pinned starting state use the real tool to produce it
# rather than writing digests into their fixtures: a fixture then holds the
# plain tag, and the digest arrives the same way it does in production. It
# also means the suite cannot disagree with docker pin about what a pinned
# line looks like.
ctx_docker_pin() {
  if [ -z "${CTX_DOCKER_PIN:-}" ]; then
    local dir
    dir="$(ctx_run_dir dockerpin)"
    CTX_DOCKER_PIN="$dir/docker-pin"
    # Instrumented when GOCOVERDIR is set, so this counts toward coverage the
    # same way the suite's other binaries do.
    if [ -n "${GOCOVERDIR:-}" ]; then
      (cd "$CTX_ROOT" && go build -cover -coverpkg=./... -o "$CTX_DOCKER_PIN" ./cmd/docker-pin)
    else
      (cd "$CTX_ROOT" && go build -o "$CTX_DOCKER_PIN" ./cmd/docker-pin)
    fi
  fi
  printf '%s' "$CTX_DOCKER_PIN"
}

# ctx_fixture <scenario> <dir> -- copy a scenario's fixture into dir.
#
# Each scenario owns a directory under hack/fixtures/<suite>/, holding the
# compose file it needs and anything that file references. A real compose file
# is readable on its own terms, whereas one assembled by echo and patched by
# sed can only be understood by running it -- and the compose file is what
# duva parses, so the fixture should be one.
#
# Nothing is substituted: every path in a fixture is a literal under testbed/,
# except ${CTX_ROOT}, which compose interpolates because the daemon needs
# absolute paths and the repo's location is all that differs per machine. The
# digest is absent by design -- the suite pins the image with docker pin, so
# the starting state is made the way a real stack's is.
#
# The copy is a git repository, because duva commits what it applies and
# refuses to act on a dirty tree.
ctx_fixture() {
  local scenario="$1" dir="$2"
  local src="${CTX_ROOT}/hack/fixtures/${CTX_SUITE#integration-}/${scenario}"
  [ -d "$src" ] || { echo "no fixture at $src" >&2; return 1; }

  mkdir -p "$dir"
  cp -R "$src/." "$dir/"

  # Fixtures include common/*.yml for what every scenario shares. Copied
  # inside the project, so the include resolves from below the compose file
  # rather than above it, where nothing is mounted.
  mkdir -p "$dir/common"
  cp -R "${CTX_ROOT}/hack/fixtures/common/." "$dir/common/"

  git -C "$dir" init -q
  git -C "$dir" add -A
  git -C "$dir" -c user.name=t -c user.email=t@t commit -qm "$scenario"
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

# ctx_compose_up <dir> [service...] -- compose up in dir, registering the
# project so it is torn down. The project is named after the directory, which
# is fixed, so two suites cannot collide.
#
# Naming services brings up only those. A project holding a one-shot service --
# duva itself, in the suites that exercise applying -- must not start it here,
# since it runs once and exits.
ctx_compose_up() {
  local dir="$1"; shift
  CTX_PROJECTS+=("$dir")
  # --wait blocks until services with a healthcheck report healthy, so a
  # caller never has to poll for the registry to start accepting pushes.
  (cd "$dir" && docker compose up -d --wait --remove-orphans "$@" >/dev/null 2>&1) || {
    echo "compose up failed in $dir:" >&2
    (cd "$dir" && docker compose up -d --wait --remove-orphans "$@" 2>&1 | tail -5 >&2)
    return 1
  }
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

  # Run from inside the directory, so compose derives the same project name it
  # derived on the way up -- the directory's own name, which is fixed. Naming
  # the project explicitly from outside would need compose to be told where
  # the file is too, and getting that wrong invents an empty project rather
  # than failing.
  for dir in "${CTX_PROJECTS[@]:-}"; do
    [ -n "$dir" ] && [ -d "$dir" ] &&
      (cd "$dir" && docker compose down --remove-orphans --volumes --timeout 3 >/dev/null 2>&1) || true
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

# ctx_build_duva_image <tag> -- builds duva from the shipped Dockerfile.
#
# The real one, deliberately: a suite that builds its own image tests an
# artifact nobody ships, and the two drift. That is not hypothetical -- while
# the test image was FROM scratch and the shipped one was not, duva gained a
# runtime dependency on git and only the shipped image would have shown it.
#
# The base images are pulled once and cached; after that this is a no-op.
ctx_build_duva_image() {
  local tag="$1" cover=""
  if [ -n "${GOCOVERDIR:-}" ]; then
    cover="--build-arg COVER=1"
  fi
  # shellcheck disable=SC2086
  docker build -q --label "${CTX_LABEL}=${CTX_SUITE}" $cover -f cmd/duva/Dockerfile -t "$tag" "$CTX_ROOT" >/dev/null
  ctx_run_image "$tag"
}
