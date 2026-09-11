package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/agent"
	"github.com/Miista/homebrew-docker-pin/internal/ui"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

// fakeAgent is an HTTP server answering like an agent, so the hub is tested
// through its real client and transport rather than against a stubbed
// interface. The routing bugs worth catching -- wrong host, wrong service --
// live in the URL, which a stub would not exercise.
type fakeAgent struct {
	host      string
	pending   []watch.Pending
	soaking   []watch.Soaking
	last      string
	lastExact string

	mu      sync.Mutex
	applied []string // services Apply was called for, in order
	refresh int

	// applyErr, when set, is returned as the agent's refusal.
	applyErr string
	// progress is what Progress reports; known false means never asked.
	progress ui.Progress
	known    bool
}

func (f *fakeAgent) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agent.Snapshot{
			Host:           f.host,
			Pending:        f.pending,
			Soaking:        f.soaking,
			LastCheck:      f.last,
			LastCheckExact: f.lastExact,
		})
	})
	mux.HandleFunc("/v1/apply/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.applied = append(f.applied, strings.TrimPrefix(r.URL.Path, "/v1/apply/"))
		f.mu.Unlock()
		json.NewEncoder(w).Encode(agent.StartResponse{Error: f.applyErr})
	})
	mux.HandleFunc("/v1/progress/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agent.ProgressResponse{Known: f.known, Progress: f.progress})
	})
	mux.HandleFunc("/v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.refresh++
		f.mu.Unlock()
		json.NewEncoder(w).Encode(agent.RefreshResponse{})
	})
	mux.HandleFunc("/v1/refresh-progress", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agent.RefreshProgressResponse{Done: true})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func hubOver(t *testing.T, fakes ...*fakeAgent) *Hub {
	t.Helper()
	var agents []Agent
	for _, f := range fakes {
		srv := f.server(t)
		agents = append(agents, Agent{Host: f.host, Client: &agent.Client{BaseURL: srv.URL}})
	}
	return New(agents)
}

func TestPendingIsLabelledByHost(t *testing.T) {
	a := &fakeAgent{host: "optiplex", pending: []watch.Pending{{Service: "caddy"}}}
	b := &fakeAgent{host: "pi", pending: []watch.Pending{{Service: "caddy"}}}
	h := hubOver(t, a, b)
	h.Collect()

	got := h.Pending()
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	// Both hosts run a service called caddy. Without the host they are one
	// indistinguishable row, which is the whole reason Host exists.
	if got[0].Host != "optiplex" || got[1].Host != "pi" {
		t.Errorf("want optiplex then pi, got %q then %q", got[0].Host, got[1].Host)
	}
	for _, p := range got {
		if p.Service != "caddy" {
			t.Errorf("service = %q, want caddy", p.Service)
		}
	}
}

func TestPendingSortedByHostThenService(t *testing.T) {
	a := &fakeAgent{host: "pi", pending: []watch.Pending{{Service: "zebra"}, {Service: "alpha"}}}
	b := &fakeAgent{host: "optiplex", pending: []watch.Pending{{Service: "beta"}}}
	h := hubOver(t, a, b)
	h.Collect()

	var keys []string
	for _, p := range h.Pending() {
		keys = append(keys, Key(p.Host, p.Service))
	}
	want := []string{"optiplex/beta", "pi/alpha", "pi/zebra"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", keys, want)
	}
}

// An unreachable agent must be visible as unreachable. Silently dropping it
// produces a queue identical to "that host has no updates", which is the
// failure a queue exists to prevent.
func TestUnreachableAgentIsReportedNotHidden(t *testing.T) {
	good := &fakeAgent{host: "optiplex", pending: []watch.Pending{{Service: "caddy"}}}
	srv := good.server(t)
	h := New([]Agent{
		{Host: "optiplex", Client: &agent.Client{BaseURL: srv.URL}},
		// A port nothing listens on.
		{Host: "pi", Client: &agent.Client{BaseURL: "http://127.0.0.1:1"}},
	})
	h.Collect()

	if n := len(h.Pending()); n != 1 {
		t.Errorf("want only the reachable host's row, got %d", n)
	}
	probs := h.Unreachable()
	if len(probs) != 1 || probs[0].Host != "pi" {
		t.Fatalf("want pi reported unreachable, got %+v", probs)
	}
	if probs[0].Err == "" {
		t.Error("want a reason for the failure")
	}
}

func TestApplyRoutesToTheOwningHost(t *testing.T) {
	a := &fakeAgent{host: "optiplex"}
	b := &fakeAgent{host: "pi"}
	h := hubOver(t, a, b)

	if err := h.Start(Key("pi", "caddy")); err != nil {
		t.Fatalf("Start: %v", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(a.applied) != 0 {
		t.Errorf("optiplex was asked to apply %v; only pi should have been", a.applied)
	}
	if len(b.applied) != 1 || b.applied[0] != "caddy" {
		t.Errorf("pi applied %v, want [caddy]", b.applied)
	}
}

func TestApplyToUnknownHostFails(t *testing.T) {
	h := hubOver(t, &fakeAgent{host: "optiplex"})
	if err := h.Start(Key("nosuch", "caddy")); err == nil {
		t.Fatal("want an error for an unknown host")
	}
}

func TestApplyWithoutHostKeyFails(t *testing.T) {
	h := hubOver(t, &fakeAgent{host: "optiplex"})
	// A bare service name is ambiguous across hosts, so it must be refused
	// rather than guessed at.
	if err := h.Start("caddy"); err == nil {
		t.Fatal("want an error for a key with no host")
	}
}

// The agent's own refusal ("nothing queued", "already running") must reach
// the caller as an error, not be swallowed by a 200.
func TestAgentRefusalSurfacesAsError(t *testing.T) {
	a := &fakeAgent{host: "pi", applyErr: "nothing queued under that name"}
	h := hubOver(t, a)
	err := h.Start(Key("pi", "caddy"))
	if err == nil {
		t.Fatal("want the agent's refusal as an error")
	}
	if !strings.Contains(err.Error(), "nothing queued") {
		t.Errorf("want the agent's reason, got %v", err)
	}
}

func TestRefreshAsksEveryAgent(t *testing.T) {
	a := &fakeAgent{host: "optiplex"}
	b := &fakeAgent{host: "pi"}
	h := hubOver(t, a, b)

	if err := h.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if a.refresh != 1 || b.refresh != 1 {
		t.Errorf("refresh counts: optiplex=%d pi=%d, want 1 and 1", a.refresh, b.refresh)
	}
}

// One unreachable agent must not stop the others being refreshed.
func TestRefreshContinuesPastAnUnreachableAgent(t *testing.T) {
	good := &fakeAgent{host: "optiplex"}
	srv := good.server(t)
	h := New([]Agent{
		{Host: "optiplex", Client: &agent.Client{BaseURL: srv.URL}},
		{Host: "pi", Client: &agent.Client{BaseURL: "http://127.0.0.1:1"}},
	})

	err := h.Refresh()
	if err == nil {
		t.Error("want the failure reported")
	} else if !strings.Contains(err.Error(), "pi") {
		t.Errorf("want pi named in the error, got %v", err)
	}
	good.mu.Lock()
	defer good.mu.Unlock()
	if good.refresh != 1 {
		t.Errorf("optiplex refresh = %d, want 1 despite pi being down", good.refresh)
	}
}

// The footer answers "is this recent". With several agents that has to mean
// the stalest one, or a host that stopped checking hides behind one that did.
func TestLastCheckReportsTheOldest(t *testing.T) {
	a := &fakeAgent{host: "optiplex", last: "2 minutes ago", lastExact: "2026-09-11T08:00:00Z"}
	b := &fakeAgent{host: "pi", last: "6 days ago", lastExact: "2026-09-05T08:00:00Z"}
	h := hubOver(t, a, b)
	h.Collect()

	if got := h.LastCheck(); got != "6 days ago" {
		t.Errorf("LastCheck = %q, want the oldest (6 days ago)", got)
	}
	if got := h.LastCheckExact(); got != "2026-09-05T08:00:00Z" {
		t.Errorf("LastCheckExact = %q, want the oldest timestamp", got)
	}
}

func TestLastCheckWithNoAgentsEverChecked(t *testing.T) {
	h := hubOver(t, &fakeAgent{host: "pi"})
	h.Collect()
	if got := h.LastCheck(); got != "never" {
		t.Errorf("LastCheck = %q, want never", got)
	}
}

func TestProgressRoutesToTheOwningHost(t *testing.T) {
	a := &fakeAgent{host: "optiplex", known: false}
	b := &fakeAgent{host: "pi", known: true, progress: ui.Progress{Done: true, Message: "updated"}}
	h := hubOver(t, a, b)

	p, ok := h.Progress(Key("pi", "caddy"))
	if !ok {
		t.Fatal("want pi's progress to be known")
	}
	if p.Message != "updated" {
		t.Errorf("message = %q, want updated", p.Message)
	}

	if _, ok := h.Progress(Key("optiplex", "caddy")); ok {
		t.Error("optiplex reported unknown; want ok=false")
	}
}

func TestProgressForUnknownHostIsNotKnown(t *testing.T) {
	h := hubOver(t, &fakeAgent{host: "pi"})
	if _, ok := h.Progress(Key("nosuch", "caddy")); ok {
		t.Error("want ok=false for an unrouteable key")
	}
}

func TestKeyRoundTrips(t *testing.T) {
	host, service, ok := SplitKey(Key("optiplex", "paperless-db"))
	if !ok || host != "optiplex" || service != "paperless-db" {
		t.Errorf("got %q/%q ok=%v", host, service, ok)
	}
}

func TestSplitKeyRejectsMalformed(t *testing.T) {
	for _, in := range []string{"caddy", "", "/caddy", "pi/"} {
		if _, _, ok := SplitKey(in); ok {
			t.Errorf("SplitKey(%q) accepted; want rejected", in)
		}
	}
}

// A hub configured with no agents is a valid but empty hub, not a crash.
func TestEmptyHub(t *testing.T) {
	h := New(nil)
	h.Collect()
	if len(h.Pending()) != 0 || len(h.Soaking()) != 0 {
		t.Error("want no rows")
	}
	if got := h.LastCheck(); got != "never" {
		t.Errorf("LastCheck = %q, want never", got)
	}
}

// The hub labels rows with the configured host, not the name the agent
// reports for itself: routing uses the configured one, so a disagreement
// must not produce a row that cannot be clicked.
func TestConfiguredHostWinsOverReportedHost(t *testing.T) {
	f := &fakeAgent{host: "i-am-someone-else", pending: []watch.Pending{{Service: "caddy"}}}
	srv := f.server(t)
	h := New([]Agent{{Host: "pi", Client: &agent.Client{BaseURL: srv.URL}}})
	h.Collect()

	got := h.Pending()
	if len(got) != 1 || got[0].Host != "pi" {
		t.Fatalf("want the configured host pi, got %+v", got)
	}
	// And the row must route.
	if err := h.Start(Key(got[0].Host, got[0].Service)); err != nil {
		t.Errorf("row does not route back: %v", err)
	}
}

// A read must not serve data older than MaxAge. An agent that has just
// finished a check has to appear on the next reload, or an operator
// refreshing after an update sees the state from before it and concludes
// nothing happened.
func TestReadsRecollectWhenTheCacheIsStale(t *testing.T) {
	f := &fakeAgent{host: "pi"}
	srv := f.server(t)
	h := New([]Agent{{Host: "pi", Client: &agent.Client{BaseURL: srv.URL}}})
	// Anything cached at all is already too old.
	h.MaxAge = -1

	if n := len(h.Pending()); n != 0 {
		t.Fatalf("want no rows to start, got %d", n)
	}
	// The agent finds something after the first read.
	f.pending = []watch.Pending{{Service: "caddy"}}
	if n := len(h.Pending()); n != 1 {
		t.Errorf("a stale cache was served: got %d rows, want the agent's 1", n)
	}
}

// The cache does its job too: within MaxAge a read must not fan out again,
// or every page render becomes as slow as the slowest host.
func TestReadsUseTheCacheWhenFresh(t *testing.T) {
	f := &fakeAgent{host: "pi"}
	srv := f.server(t)
	h := New([]Agent{{Host: "pi", Client: &agent.Client{BaseURL: srv.URL}}})
	h.MaxAge = time.Hour

	h.Pending()
	f.pending = []watch.Pending{{Service: "caddy"}}
	if n := len(h.Pending()); n != 0 {
		t.Errorf("want the cached (empty) answer within MaxAge, got %d rows", n)
	}
}

// A collection triggered by a page load must report an unreachable agent,
// not just the ones a background timer runs: a queue with a host missing
// from it looks like that host having nothing to do.
func TestUnreachableIsReportedOnEveryCollect(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	h := New([]Agent{{Host: "pi", Client: &agent.Client{BaseURL: "http://127.0.0.1:1"}}})
	h.MaxAge = -1 // every read re-collects
	h.OnUnreachable = func(host, err string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, host)
	}

	h.Pending() // a read, as a page load would do
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("a read that re-collected did not report the unreachable agent")
	}
	if seen[0] != "pi" {
		t.Errorf("reported %q, want pi", seen[0])
	}
}

// The callback must not run holding the hub's lock: one that reads the hub
// -- to render the problem beside the queue -- would deadlock.
func TestUnreachableCallbackCanReadTheHub(t *testing.T) {
	h := New([]Agent{{Host: "pi", Client: &agent.Client{BaseURL: "http://127.0.0.1:1"}}})
	h.MaxAge = time.Hour
	done := make(chan struct{})
	h.OnUnreachable = func(host, err string) {
		// Reading from inside the callback must not hang.
		_ = h.Unreachable()
		close(done)
	}
	h.Collect()
	select {
	case <-done:
	default:
		t.Fatal("the callback did not complete")
	}
}
