package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

type fakeSource struct {
	pending []watch.Pending
}

func (f fakeSource) Pending() []watch.Pending { return f.pending }
func (f fakeSource) LastCheck() string        { return "2026-01-01T00:00:00Z" }

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestIndex_RendersPendingRows(t *testing.T) {
	s := &Server{Source: fakeSource{pending: []watch.Pending{
		{Service: "app", Image: "example.com/app", CurrentTag: "1.2.0",
			Kind: watch.KindTag, Candidate: "1.3.0", Reason: "minor",
			FirstSeen: "2026-01-01T00:00:00Z"},
	}}, Host: "testhost", Version: "test"}

	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"app", "example.com/app", "1.2.0", "1.3.0", "minor", "testhost"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// A moved digest is 71 characters of hex nobody reads; the short form still
// identifies it well enough to match against a notification or a commit.
func TestIndex_ShortensDigestCandidates(t *testing.T) {
	const full = "sha256:41b194461e4bae16f9b25d68b0976ed4735b89ca625c89aad88e1c1c3b7e8860"
	s := &Server{Source: fakeSource{pending: []watch.Pending{
		{Service: "app", Kind: watch.KindDigest, Candidate: full},
	}}}

	body := get(t, s, "/").Body.String()
	if strings.Contains(body, full) {
		t.Error("full digest should not be rendered")
	}
	if !strings.Contains(body, "sha256:41b194461e4b") {
		t.Error("short digest missing")
	}
}

func TestIndex_EmptyQueue(t *testing.T) {
	s := &Server{Source: fakeSource{}}
	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "Nothing waiting") {
		t.Errorf("expected an empty state, got:\n%s", body)
	}
}

// The queue is the only page; a typo'd path is not a silent redirect to it.
func TestUnknownPathIs404(t *testing.T) {
	s := &Server{Source: fakeSource{}}
	if rec := get(t, s, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	s := &Server{Source: fakeSource{}}
	rec := get(t, s, "/healthz")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

// Service names come from a compose file the operator controls, but rendering
// them unescaped would still be a bug waiting to happen.
func TestIndex_EscapesContent(t *testing.T) {
	s := &Server{Source: fakeSource{pending: []watch.Pending{
		{Service: `<script>alert(1)</script>`, Candidate: "1.0.0"},
	}}}
	body := get(t, s, "/").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("service name rendered unescaped")
	}
}
