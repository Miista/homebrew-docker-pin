package queue

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/jobs"
)

func failing(n int) func() []jobs.Status {
	return func() []jobs.Status {
		return []jobs.Status{{Job: jobs.Check, FailedInARow: n,
			Last: &jobs.Run{Job: jobs.Check, Summary: "registry timeout", Failed: n > 0}}}
	}
}

// One or two failures are a registry or a network having a moment; three in
// a row are worth a look, and still a 200: degraded is not a reason to
// restart anything.
func TestAJobFailingThreeTimesInARowIsDegraded(t *testing.T) {
	for n, want := range map[int]string{0: HealthOK, 2: HealthOK, 3: HealthDegraded} {
		h := (&Server{Queue: NewPending(), Jobs: failing(n)}).Health()
		if h.Status != want {
			t.Errorf("%d in a row: %q %v, want %q", n, h.Status, h.Reasons, want)
		}
	}
	h := (&Server{Queue: NewPending(), Jobs: failing(4)}).Health()
	if len(h.Reasons) != 1 || h.Reasons[0] != "check: failed 4 times in a row (last: registry timeout)" {
		t.Errorf("reasons = %v", h.Reasons)
	}

	rec := httptest.NewRecorder()
	(&Server{Queue: NewPending(), Jobs: failing(3)}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	var got Health
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Status != HealthDegraded {
		t.Errorf("/healthz = %d %s, want 200 with status degraded", rec.Code, rec.Body)
	}
}

// A store that cannot be used is duva not working: unhealthy, and the one
// case that answers 503, which is what a container runtime restarts on.
func TestAStoreThatCannotBeUsedIsUnhealthy(t *testing.T) {
	q := NewPending()
	q.st.Close()
	s := &Server{Queue: q}
	if h := s.Health(); h.Status != HealthUnhealthy || !strings.HasPrefix(strings.Join(h.Reasons, ""), "store:") {
		t.Errorf("health = %+v", h)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/healthz = %d, want 503", rec.Code)
	}
}
