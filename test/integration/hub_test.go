//go:build integration

package integration

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A hub over two agents.
//
// The unit tests fake the transport, so what they cannot show is whether a
// real hub reaches real agents over a real network, whether the token is
// actually required on the wire, or whether a click on a hub's row recreates
// a container on the agent that owns it.
//
// Both agents watch the same project here -- there is one host in the test
// world -- but they run as separately named hosts, which is the thing the hub
// has to keep apart.

func TestHubShowsEveryAgentsQueue(t *testing.T) {
	s := Up(t, "duva-hub/two-agents")

	s.PushUpdates()
	s.Pin("shared")
	s.Start()

	// Both agents found the same service name; the hub must show it twice,
	// once per host, rather than collapsing them into one row.
	// Both agents must appear; wait for the slower of the two rather than
	// reading once and racing the hub's refresh.
	s.QueueContaining(`value="alpha/shared"`)
	body := s.QueueContaining(`value="beta/shared"`)
	if got := strings.Count(body, `value="alpha/shared"`); got != 1 {
		t.Errorf("want one routable row for alpha/shared, got %d", got)
	}
	if got := strings.Count(body, `value="beta/shared"`); got != 1 {
		t.Errorf("want one routable row for beta/shared, got %d", got)
	}
}

// The hub holds no docker socket, no compose mount and no repository, and
// still serves the queue. This is the whole point of the split: the
// network-facing half needs none of the privilege the work needs.
func TestHubHoldsNoPrivilege(t *testing.T) {
	s := Up(t, "duva-hub/two-agents")
	s.PushUpdates()
	s.Pin("shared")
	s.Start()

	// It answered, so it is serving.
	s.QueueContaining("shared")

	container := s.Container("duva-hub")
	if container == "" {
		t.Fatal("the hub is not running")
	}
	// Nothing mounted from the host: no socket to become root with, no
	// compose file to rewrite, no repository to commit to.
	for _, dest := range []string{"/var/run/docker.sock", "/compose", "/data"} {
		if src := s.BindSource(container, dest); src != "" {
			t.Errorf("the hub has %s mounted from %s; it should hold none of these", dest, src)
		}
	}
}

// Pressing Update on a hub's row must recreate the container on the agent
// that owns it -- the full path, over the network, through the agent's own
// transaction.
func TestHubUpdateReachesTheOwningAgent(t *testing.T) {
	s := Up(t, "duva-hub/two-agents")

	s.PushUpdates()
	s.Pin("shared")
	s.Start()

	before := s.Container("shared")
	if before == "" {
		t.Fatal("the service should be running before the hub is asked")
	}

	s.UpdateNow("alpha/shared")

	// The agent applies asynchronously and the page polls; wait for the
	// container to actually be replaced rather than for the POST to return.
	deadline := time.Now().Add(90 * time.Second)
	var after string
	for time.Now().Before(deadline) {
		after = s.Container("shared")
		if after != "" && after != before {
			break
		}
		time.Sleep(time.Second)
	}
	if after == "" || after == before {
		t.Fatalf("the container was not replaced\n%s", s.Logs("duva-alpha"))
	}
	if got := s.RunningImage(after); !strings.Contains(got, "1.0.1") {
		t.Errorf("the container is not on the new image:\n  got %s", got)
	}
	if got := s.Image("shared"); !strings.Contains(got, ":1.0.1@sha256:") {
		t.Errorf("the compose file was not repinned:\n  got %s", got)
	}
}

// Routing is by host, not by a search across agents. A key naming a host that
// does not exist must be refused rather than applied to whichever agent
// happens to have a service of that name.
func TestHubRefusesAnUnknownHost(t *testing.T) {
	s := Up(t, "duva-hub/two-agents")
	s.PushUpdates()
	s.Pin("shared")
	s.Start()

	before := s.Container("shared")

	resp, err := http.PostForm("http://localhost:"+queuePort+"/apply",
		url.Values{"service": {"nosuch/shared"}})
	if err != nil {
		t.Fatalf("posting: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "nosuch") {
		t.Errorf("the refusal should name the host it could not find:\n%s", body)
	}

	// And nothing was touched.
	time.Sleep(3 * time.Second)
	if after := s.Container("shared"); after != before {
		t.Error("a key naming an unknown host still recreated a container")
	}
}

// The agent's token is what separates "the hub asked" from "anything that can
// reach the port asked". /apply is root on that host, so an unauthenticated
// call must be refused on the wire, not merely in the unit tests.
func TestAgentRefusesAnUntokenedCaller(t *testing.T) {
	s := Up(t, "duva-hub/two-agents")
	s.PushUpdates()
	s.Pin("shared")
	s.Start()

	// Reach the agent directly, from inside the network the hub uses, with
	// no Authorization header.
	out := s.docker("run", "--rm", "--network", s.Network("duva-alpha"),
		"curlimages/curl:8.11.1", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"-X", "POST", "http://duva-alpha:8080/v1/apply/shared")
	if strings.TrimSpace(out) != "401" {
		t.Errorf("an untokened apply got %q, want 401", strings.TrimSpace(out))
	}

	// The same call with the token is accepted, so the 401 above is the
	// token doing its job and not the endpoint being broken.
	out = s.docker("run", "--rm", "--network", s.Network("duva-alpha"),
		"curlimages/curl:8.11.1", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"-H", "Authorization: Bearer integration-token",
		"-X", "POST", "http://duva-alpha:8080/v1/apply/shared")
	if strings.TrimSpace(out) != "200" {
		t.Errorf("a tokened apply got %q, want 200", strings.TrimSpace(out))
	}
}

// An agent that cannot be reached must render as unreachable. A hub that
// silently drops a host shows a queue indistinguishable from one where that
// host has nothing waiting -- which is the exact failure a queue exists to
// prevent.
func TestHubReportsAnUnreachableAgent(t *testing.T) {
	s := Up(t, "duva-hub/two-agents")
	s.PushUpdates()
	s.Pin("shared")
	s.Start()

	s.QueueContaining(`value="beta/shared"`)

	s.stop("duva-beta")

	// The hub re-collects on its own timer; wait for it to notice.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if !strings.Contains(s.Queue(), `value="beta/shared"`) {
			break
		}
		time.Sleep(2 * time.Second)
	}

	body := s.Queue()
	if strings.Contains(body, `value="beta/shared"`) {
		t.Error("beta's rows are still offered after it went away")
	}
	// alpha is unaffected: one host being down does not empty the queue.
	if !strings.Contains(body, `value="alpha/shared"`) {
		t.Errorf("alpha's row went missing when beta did:\n%s", body)
	}
	if got := s.Logs("duva-hub"); !strings.Contains(got, "unreachable") {
		t.Errorf("the hub should say it cannot reach beta:\n%s", got)
	}
}
