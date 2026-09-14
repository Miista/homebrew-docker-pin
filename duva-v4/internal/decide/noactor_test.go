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
