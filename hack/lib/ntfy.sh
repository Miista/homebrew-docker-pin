# A fake ntfy receiver, for asserting on what duva announced.
#
# Notifications cross the application boundary, so an integration test wants a
# real HTTP server on the other end rather than a stub inside the process --
# but not a real ntfy instance, which would be an upstream dependency and
# would mean querying its API to find out what it received.
#
# It runs as a container on the shared network, like everything else here: the
# harness must not assume anything is installed on the host, so the receiver is
# a static Go binary in a scratch image rather than a python or node script.
#
# Each POST is appended to a log as one line, "Title|body", so a test can count
# them and assert on their contents. Bodies are flattened because duva's
# messages are multi-line -- what changed, and why -- and a test that counts
# lines would otherwise read one notification as several.

NTFY_NAME="testntfy"
# What a container posts to. The receiver is on the same network as duva.
NTFY_INTERNAL_URL="http://${NTFY_NAME}:8080"

# ntfy_start -- builds and starts the receiver on REGISTRY_NETWORK, setting
# NTFY_LOG to the file its requests land in. Requires ctx_init and
# registry_start (for the network); torn down with the rest of the scenario.
# ntfy_setup -- builds the receiver image. Call once, with registry_setup.
ntfy_setup() {
  local dir
  dir="$(ctx_run_dir ntfyimg)"
  NTFY_IMAGE="${CTX_SUITE}-ntfy:test"

  cat > "$dir/main.go" <<'GOEOF'
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// One line per notification: "Title|body", with the body flattened so a test
// counting lines sees one notification as one line.
func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f, err := os.OpenFile("/log/requests.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s|%s\n",
			r.Header.Get("Title"),
			strings.ReplaceAll(string(body), "\n", " "))
		w.WriteHeader(http.StatusOK)
	})
	http.ListenAndServe(":8080", nil)
}
GOEOF
  (cd "$dir" && CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o receiver main.go)
  printf 'FROM scratch\nCOPY receiver /receiver\nCMD ["/receiver"]\n' > "$dir/Dockerfile"

  docker build -q --label "${CTX_LABEL}=${CTX_SUITE}" -t "$NTFY_IMAGE" "$dir" >/dev/null
  ctx_run_image "$NTFY_IMAGE"
}

# ntfy_start -- runs a fresh receiver for this scenario, so each starts with an
# empty log. Requires ntfy_setup and registry_start (for the network).
ntfy_start() {
  local dir
  dir="$(ctx_dir ntfy)"
  NTFY_LOG_DIR="$dir"
  NTFY_LOG="$dir/requests.log"
  : > "$NTFY_LOG"
  chmod 777 "$dir"

  ctx_run "$NTFY_NAME" \
    --network "$REGISTRY_NETWORK" \
    --user "$(id -u):$(id -g)" \
    -v "$dir:/log" \
    "$NTFY_IMAGE"

  # Wait for it to bind before anything tries to reach it. The container has
  # no shell to probe with, so this watches for the port to be listening from
  # the daemon's point of view instead.
  local _
  for _ in $(seq 40); do
    if docker logs "$NTFY_NAME" 2>&1 | grep -q "panic"; then
      echo "ntfy receiver failed to start:" >&2
      docker logs "$NTFY_NAME" >&2 2>&1
      return 1
    fi
    # A running container with no exit status is the best signal available.
    [ "$(docker inspect -f '{{.State.Running}}' "$NTFY_NAME" 2>/dev/null)" = "true" ] && break
    sleep 0.1
  done
  sleep 0.3
}

# ntfy_count -- how many notifications have been received.
ntfy_count() {
  wc -l < "$NTFY_LOG" 2>/dev/null | tr -d ' '
}

# ntfy_mentions <text> -- whether any notification contains text.
ntfy_mentions() {
  grep -q -- "$1" "$NTFY_LOG" 2>/dev/null
}

# ntfy_reset -- discard what has been received so far, for a scenario that
# asserts on notifications from one particular run.
ntfy_reset() {
  : > "$NTFY_LOG"
}

# ntfy_env -- docker run arguments pointing duva at the receiver.
ntfy_env() {
  printf -- '-e DUVA_NTFY_URL=%s -e DUVA_NTFY_TOPIC=integration' "$NTFY_INTERNAL_URL"
}
