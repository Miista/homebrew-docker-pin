package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
)

// fakeQueue stands up one queue's two endpoints.
type fakeQueue struct {
	queue    []queue.Entry
	canApply bool
	token    string
	applied  []string
	// refusal, when set, is what /v1/apply answers with: a 200 and a message,
	// which is how a queue says "nothing is queued under that name".
	refusal string
}

func (f *fakeQueue) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool {
		return f.token == "" || r.Header.Get("Authorization") == "Bearer "+f.token
	}
	mux.HandleFunc("/v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(queue.Snapshot{Pending: f.queue, CanApply: f.canApply})
	})
	mux.HandleFunc("/v1/apply/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.applied = append(f.applied, strings.TrimPrefix(r.URL.Path, "/v1/apply/"))
		json.NewEncoder(w).Encode(map[string]string{"message": f.refusal})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestCollectorGathersEveryQueue(t *testing.T) {
	a := &fakeQueue{queue: []queue.Entry{{Service: "authelia"}}}
	b := &fakeQueue{queue: []queue.Entry{{Service: "caddy"}}}
	sa, sb := a.serve(t), b.serve(t)

	c := NewCollector([]Upstream{
		{Host: "pi", URL: sb.URL},
		{Host: "optiplex", URL: sa.URL},
	})

	got := c.Pending()
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// Sorted by host, so a row keeps its place between reloads however the
	// queues happened to answer.
	if got[0].Host != "optiplex" || got[0].Service != "authelia" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Host != "pi" || got[1].Service != "caddy" {
		t.Errorf("second = %+v", got[1])
	}
}

// One queue being down must not cost the others' queues, and must be
// reported rather than dropped.
func TestUnreachableQueueIsReportedNotDropped(t *testing.T) {
	up := (&fakeQueue{queue: []queue.Entry{{Service: "authelia"}}}).serve(t)

	var told []string
	c := NewCollector([]Upstream{
		{Host: "optiplex", URL: up.URL},
		// A port nothing listens on: refused immediately rather than hanging
		// the test on a timeout.
		{Host: "pi", URL: "http://127.0.0.1:1"},
	})
	c.OnUnreachable = func(host, err string) { told = append(told, host) }

	if got := c.Pending(); len(got) != 1 || got[0].Host != "optiplex" {
		t.Fatalf("pending = %+v, want only optiplex's", got)
	}
	bad := c.Unreachable()
	if len(bad) != 1 || bad[0].Host != "pi" {
		t.Fatalf("unreachable = %+v, want pi", bad)
	}
	if bad[0].Err == "" {
		t.Error("an unreachable queue carries no reason")
	}
	if len(told) == 0 || told[0] != "pi" {
		t.Errorf("OnUnreachable told = %v, want pi", told)
	}
}

// The routing this exists for: two hosts run services of the same name, and a
// click must reach the one whose row was clicked.
func TestApproveGoesToTheNamedHost(t *testing.T) {
	a := &fakeQueue{}
	b := &fakeQueue{}
	sa, sb := a.serve(t), b.serve(t)

	c := NewCollector([]Upstream{
		{Host: "optiplex", URL: sa.URL},
		{Host: "pi", URL: sb.URL},
	})
	if err := c.Approve(Key("pi", "caddy")); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(a.applied) != 0 {
		t.Errorf("optiplex was asked to apply %v", a.applied)
	}
	if len(b.applied) != 1 || b.applied[0] != "caddy" {
		t.Fatalf("pi applied %v, want [caddy]", b.applied)
	}
}

// A queue's refusal is a 200 with a message. It must surface as an error to
// the page, or a click that did nothing would report success.
func TestRefusalSurfacesAsAnError(t *testing.T) {
	d := &fakeQueue{refusal: "nothing is queued for caddy"}
	s := d.serve(t)
	c := NewCollector([]Upstream{{Host: "pi", URL: s.URL}})

	err := c.Approve(Key("pi", "caddy"))
	if err == nil {
		t.Fatal("a refusal was reported as success")
	}
	if !strings.Contains(err.Error(), "nothing is queued") {
		t.Errorf("err = %v, want the queue's own words", err)
	}
}

func TestApproveRejectsUnknownHostAndBadKey(t *testing.T) {
	c := NewCollector([]Upstream{{Host: "pi", URL: "http://127.0.0.1:1"}})
	if err := c.Approve("caddy"); err == nil {
		t.Error("a key with no host was accepted")
	}
	if err := c.Approve(Key("nowhere", "caddy")); err == nil {
		t.Error("an unknown host was accepted")
	}
}

// The token must be presented, or every queue that minted one is silently
// unreachable.
func TestTokenIsPresented(t *testing.T) {
	d := &fakeQueue{token: "sekrit", queue: []queue.Entry{{Service: "authelia"}}}
	s := d.serve(t)

	withToken := NewCollector([]Upstream{{Host: "pi", URL: s.URL, Token: "sekrit"}})
	if got := withToken.Pending(); len(got) != 1 {
		t.Fatalf("with the token: got %d entries, want 1", len(got))
	}

	without := NewCollector([]Upstream{{Host: "pi", URL: s.URL}})
	if got := without.Pending(); len(got) != 0 {
		t.Errorf("without the token: got %d entries, want none", len(got))
	}
	if len(without.Unreachable()) != 1 {
		t.Error("a rejected token is not reported as unreachable")
	}
}

func TestSplitKey(t *testing.T) {
	host, service, ok := SplitKey("optiplex/authelia")
	if !ok || host != "optiplex" || service != "authelia" {
		t.Errorf("SplitKey = %q, %q, %v", host, service, ok)
	}
	for _, bad := range []string{"", "authelia", "/authelia", "optiplex/"} {
		if _, _, ok := SplitKey(bad); ok {
			t.Errorf("SplitKey(%q) was accepted", bad)
		}
	}
}

// A queue that stops answering keeps its queue: what was waiting is still
// waiting, and the queue being unreachable is a fact about the queue, not
// about its queue. Dropping the rows would show an empty page, which says the
// opposite of the truth.
func TestAnUnreachableQueueKeepsItsLastQueue(t *testing.T) {
	d := &fakeQueue{queue: []queue.Entry{{Service: "gluetun"}}, canApply: true}
	srv := d.serve(t)
	c := NewCollector([]Upstream{{Host: "optiplex", URL: srv.URL}})

	if got := c.Pending(); len(got) != 1 || got[0].Stale {
		t.Fatalf("first collection: %+v, want one fresh row", got)
	}

	// The queue goes away.
	srv.Close()
	c.MaxAge = time.Nanosecond // force a re-collect

	got := c.Pending()
	if len(got) != 1 {
		t.Fatalf("after it went away: %d rows, want the 1 it had", len(got))
	}
	if !got[0].Stale {
		t.Error("a row from an unreachable queue is not marked stale")
	}
	// And must not be applyable: the route to the updater is through the queue.
	if got[0].CanApply {
		t.Error("a row from an unreachable queue is still applyable")
	}
	if len(c.Unreachable()) != 1 {
		t.Error("the queue is not reported unreachable")
	}
}
