package decide

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Clicking Update with no actor configured must say so, not panic. The
// handler dereferenced a nil Applier, so the connection died mid-request and
// the page got nothing at all.
func TestApplyWithNoActorSaysSo(t *testing.T) {
	q := NewPending()
	q.Put(Entry{Service: "gluetun", Tag: "latest"}, time.Now())
	s := &Server{Queue: q, Host: "test"} // no Applier

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/apply/gluetun", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Message string `json:"message"`
	}
	json.NewDecoder(rec.Body).Decode(&out)
	if !strings.Contains(out.Message, "no actor") {
		t.Errorf("message = %q, want it to mention no actor", out.Message)
	}
	// And the entry must still be queued: it was never applied.
	if _, ok := q.Get("gluetun"); !ok {
		t.Error("the entry left the queue for an apply that never happened")
	}
}

// fakeApplier is an Applier that can report readiness.

// An actor that is there but would refuse must reach the page as not-applyable,
// with its own words. The reference actor refuses on an uncommitted
// repository, because applying commits -- and a button that looks live until
// clicked is the failure this reporting exists to prevent.
func TestSnapshotRelaysWhyAnActorWouldRefuse(t *testing.T) {
	const reason = "the repository has uncommitted changes, and applying commits"
	s := &Server{
		Queue:   NewPending(),
		Applier: &fakeApplier{ready: boolPtr(false), reason: reason},
		Host:    "optiplex",
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/snapshot", nil))

	var snap Snapshot
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if snap.CanApply {
		t.Error("an actor that would refuse is reported as able to apply")
	}
	if snap.WhyNot != reason {
		t.Errorf("why_not = %q, want the actor's own words", snap.WhyNot)
	}
}

// The positive: a ready actor reports applyable with nothing to explain.
func TestSnapshotSaysApplyableWhenTheActorIsReady(t *testing.T) {
	s := &Server{
		Queue:   NewPending(),
		Applier: &fakeApplier{ready: boolPtr(true)},
		Host:    "optiplex",
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/snapshot", nil))

	var snap Snapshot
	json.NewDecoder(rec.Body).Decode(&snap)
	if !snap.CanApply {
		t.Error("a ready actor is reported as unable to apply")
	}
	if snap.WhyNot != "" {
		t.Errorf("why_not = %q, want empty when ready", snap.WhyNot)
	}
}

func boolPtr(b bool) *bool { return &b }

// The two reasons a click cannot land carry different kinds, so a page can
// style them apart without matching on prose the actor is free to reword.
func TestSnapshotDistinguishesTheTwoBlockers(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *Server
		want Blocker
	}{
		{"no actor at all",
			&Server{Queue: NewPending(), Host: "optiplex"},
			NoActor},
		{"an actor that would refuse",
			&Server{Queue: NewPending(), Host: "optiplex",
				Applier: &fakeApplier{ready: boolPtr(false), reason: "uncommitted changes"}},
			ActorNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/snapshot", nil))
			var snap Snapshot
			json.NewDecoder(rec.Body).Decode(&snap)

			if snap.CanApply {
				t.Fatal("reported as able to apply")
			}
			if snap.Blocker != tc.want {
				t.Errorf("blocker = %q, want %q", snap.Blocker, tc.want)
			}
			if snap.WhyNot == "" {
				t.Error("no reason to show a person")
			}
		})
	}
}

// A ready actor carries neither, so the page renders no banner at all.
func TestSnapshotCarriesNoBlockerWhenReady(t *testing.T) {
	s := &Server{Queue: NewPending(), Host: "optiplex",
		Applier: &fakeApplier{ready: boolPtr(true)}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/snapshot", nil))

	var snap Snapshot
	json.NewDecoder(rec.Body).Decode(&snap)
	if !snap.CanApply || snap.Blocker != "" || snap.WhyNot != "" {
		t.Errorf("a ready actor reported can_apply=%v blocker=%q why=%q",
			snap.CanApply, snap.Blocker, snap.WhyNot)
	}
}
