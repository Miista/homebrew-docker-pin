package queue

import (
	"encoding/json"
	"fmt"
	"github.com/Miista/homebrew-docker-pin/oci/version"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// actionServer is a queue API with the watcher's actions and a resolver
// faked, recording what they were asked.
type actionServer struct {
	srv     *httptest.Server
	q       *Pending
	mu      sync.Mutex
	checked []string
	rescans []string
}

func newActionServer(t *testing.T) *actionServer {
	t.Helper()
	a := &actionServer{q: NewPending()}
	s := &Server{
		Queue: a.q, Applier: &fakeApplier{}, Token: "s3cret",
		Check:   func(svc string) error { a.mu.Lock(); a.checked = append(a.checked, svc); a.mu.Unlock(); return nil },
		Rescan:  func(svc string) error { a.mu.Lock(); a.rescans = append(a.rescans, svc); a.mu.Unlock(); return nil },
		Resolve: func(image, tag string) (string, error) { return "sha256:resolved-" + tag, nil },
	}
	a.srv = httptest.NewServer(s.Handler())
	t.Cleanup(a.srv.Close)
	return a
}

// post sends an action as the UI would: the token, and who it is for.
func (a *actionServer) post(t *testing.T, path string, form url.Values) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, a.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("X-Duva-Actor", "soren")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s", path, resp.Status)
	}
	var out struct{ Message string }
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Message
}

func TestDiscardOverTheAPI(t *testing.T) {
	a := newActionServer(t)
	e, _ := a.q.PutEntry(releaseOf("app", "example.com/app", "1.0.1"), now)

	if msg := a.post(t, fmt.Sprintf("/v1/discard/%d", e.ID), nil); msg != "" {
		t.Fatalf("discard: %s", msg)
	}
	if _, ok := a.q.Get("app"); ok {
		t.Error("still waiting after a discard")
	}
	if got := lastActor(t, a.q, "discarded"); got != "soren" {
		t.Errorf("discarded by %q, want the X-Duva-Actor", got)
	}
}

// A version change carries no digest; blocking it resolves what its tag
// points at now, and blocks that.
func TestBlockingAVersionChangeResolvesItsDigest(t *testing.T) {
	a := newActionServer(t)
	e, _ := a.q.PutEntry(releaseOf("app", "example.com/app", "1.0.1"), now)

	if msg := a.post(t, fmt.Sprintf("/v1/block/%d", e.ID), url.Values{"reason": {"missing migrations"}}); msg != "" {
		t.Fatalf("block: %s", msg)
	}
	if reason, blocked := a.q.IsBlocked("example.com/app", "sha256:resolved-1.0.1"); !blocked || reason != "missing migrations" {
		t.Errorf("IsBlocked = %q %v", reason, blocked)
	}

	if msg := a.post(t, "/v1/unblock", url.Values{"image": {"example.com/app"}, "digest": {"sha256:resolved-1.0.1"}}); msg != "" {
		t.Fatalf("unblock: %s", msg)
	}
	if _, blocked := a.q.IsBlocked("example.com/app", "sha256:resolved-1.0.1"); blocked {
		t.Error("still blocked after unblocking")
	}
}

func TestABlockWithoutAReasonIsRefused(t *testing.T) {
	a := newActionServer(t)
	e, _ := a.q.PutEntry(releaseOf("app", "example.com/app", "1.0.1"), now)
	if msg := a.post(t, fmt.Sprintf("/v1/block/%d", e.ID), nil); !strings.Contains(msg, "reason") {
		t.Errorf("message = %q, want a refusal asking for a reason", msg)
	}
}

// Check and rescan reach the watcher, once per service per cooldown: a
// person clicking twice must not spend a registry's rate limit.
func TestCheckAndRescanReachTheWatcherOnce(t *testing.T) {
	a := newActionServer(t)
	if msg := a.post(t, "/v1/check/app", nil); msg != "" {
		t.Fatalf("check: %s", msg)
	}
	if msg := a.post(t, "/v1/check/app", nil); !strings.Contains(msg, "a moment ago") {
		t.Errorf("a second check at once = %q, want the cooldown", msg)
	}
	if msg := a.post(t, "/v1/rescan/db", nil); msg != "" {
		t.Fatalf("rescan: %s", msg)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		done := len(a.checked) == 1 && len(a.rescans) == 1
		a.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.checked) != 1 || a.checked[0] != "app" || len(a.rescans) != 1 || a.rescans[0] != "db" {
		t.Errorf("checked %v, rescanned %v", a.checked, a.rescans)
	}
}

// Every action holds the token: each is reached from another host.
func TestActionsRequireTheToken(t *testing.T) {
	a := newActionServer(t)
	for _, path := range []string{"/v1/discard/1", "/v1/block/1", "/v1/unblock", "/v1/check/app", "/v1/rescan/app"} {
		resp, err := http.Post(a.srv.URL+path, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without the token = %d, want 401", path, resp.StatusCode)
		}
	}
}

// Approving by entry applies that entry, not the service's newest.
func TestApprovingAnEntryAppliesThatOne(t *testing.T) {
	app := &fakeApplier{}
	q := NewPending()
	soak, _ := q.PutEntry(soakingEntry("1.0.1", now.Add(time.Hour)), now)
	q.Put(Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: "1.1.0", Tag: "1.1.0", Kind: version.KindMinor}, now)
	srv := httptest.NewServer((&Server{Queue: q, Applier: app}).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+fmt.Sprintf("/v1/apply/app?entry=%d", soak.ID), "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(app.started) != 1 || app.started[0].To != "1.0.1" {
		t.Errorf("started %+v, want the soaking 1.0.1 asked for", app.started)
	}
}
