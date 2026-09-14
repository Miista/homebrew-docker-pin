package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/decide"
	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// fakeSource is a queue without deciders behind it.
type fakeSource struct {
	pending     []Hosted
	unreachable []Problem
}

func (f *fakeSource) Pending() []Hosted      { return f.pending }
func (f *fakeSource) Unreachable() []Problem { return f.unreachable }

type fakeApprover struct {
	approved []string
	err      error
}

func (f *fakeApprover) Approve(key string) error {
	f.approved = append(f.approved, key)
	return f.err
}

func entry(service, from, to string, kind version.Kind) Hosted {
	return Hosted{
		Entry: decide.Entry{
			Service: service,
			Image:   "ghcr.io/example/" + service,
			From:    from,
			To:      to,
			Kind:    kind,
			Why:     "waiting for approval",
		},
		Host: "optiplex",
	}
}

func get(t *testing.T, s *Server, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// The page must render. Every field the template names has to exist on what
// the handler passes it -- html/template reports a missing field only when it
// executes, so nothing but executing it catches a rename.
func TestPageRenders(t *testing.T) {
	s := &Server{
		Source: &fakeSource{pending: []Hosted{
			entry("authelia", "4.39.20", "4.39.26", version.KindPatch),
		}},
		Approver: &fakeApprover{},
		Version:  "v1.2.3",
	}

	code, body := get(t, s, "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// A template error is written into the body mid-render, after a 200 has
	// already gone out -- so the status alone proves nothing.
	if strings.Contains(body, "template:") || strings.Contains(body, "can't evaluate") {
		t.Fatalf("the template failed to execute: %s", body)
	}
	for _, want := range []string{"authelia", "4.39.20", "4.39.26", "patch", "v1.2.3"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q", want)
		}
	}
}

// The negative of the above: with nothing queued the page must say so rather
// than render an empty table.
func TestEmptyQueueSaysSo(t *testing.T) {
	s := &Server{Source: &fakeSource{}, Version: "v1"}
	code, body := get(t, s, "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.Contains(body, "Nothing waiting for approval") {
		t.Errorf("an empty queue does not say it is empty: %s", body)
	}
}

// The one thing this page exists to prevent: a decider that could not be
// reached must be visible, because a queue missing a host reads exactly like
// that host having nothing to do.
func TestUnreachableDeciderIsShown(t *testing.T) {
	s := &Server{
		Source: &fakeSource{unreachable: []Problem{
			{Host: "pi", Err: "connection refused"},
		}},
		Version: "v1",
	}
	_, body := get(t, s, "/")
	if !strings.Contains(body, "pi") || !strings.Contains(body, "connection refused") {
		t.Fatalf("an unreachable decider is not shown: %s", body)
	}
	// And the reassurance must not be printed beside it.
	if strings.Contains(body, "Every container duva watches is where you left it") {
		t.Error("the page reassures while a decider is unreachable")
	}
}

// Without an Approver the endpoint must be absent, not merely refusing: a UI
// meant only to report should carry no route that triggers updates.
func TestReadOnlyHasNoApplyRoute(t *testing.T) {
	s := &Server{Source: &fakeSource{
		pending: []Hosted{entry("authelia", "1.0.0", "1.0.1", version.KindPatch)},
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apply",
		strings.NewReader("service=optiplex/authelia"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(rec, req)
	// It falls through to the index handler, which 404s anything but "/".
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("a read-only page applied an update")
	}

	if _, body := get(t, s, "/"); strings.Contains(body, "<button class=\"apply\"") {
		t.Error("a read-only page renders the Update button")
	}
}

// A click must reach the decider that owns the service, carrying the host --
// two hosts run services of the same name.
func TestApplyRoutesTheHostedKey(t *testing.T) {
	ap := &fakeApprover{}
	s := &Server{Source: &fakeSource{}, Approver: ap}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apply",
		strings.NewReader("service=optiplex/authelia"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if len(ap.approved) != 1 || ap.approved[0] != "optiplex/authelia" {
		t.Fatalf("approved = %v, want [optiplex/authelia]", ap.approved)
	}
}

// A failed approval must land back on the page saying why, not as a bare 500
// somebody has to read a log to understand.
func TestFailedApprovalIsReportedOnThePage(t *testing.T) {
	s := &Server{
		Source:   &fakeSource{},
		Approver: &fakeApprover{err: errString("the actor refused")},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/apply",
		strings.NewReader("service=optiplex/authelia"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(rec, req)

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if got := loc.Query().Get("level"); got != "error" {
		t.Errorf("level = %q, want error", got)
	}
	if got := loc.Query().Get("message"); !strings.Contains(got, "the actor refused") {
		t.Errorf("message = %q, want the actor's reason", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A digest move has no version pair, so the Kind column must say "digest"
// rather than leave a cell that reads as missing data.
// A moving tag that moved shows the tag, not the digest. The tag did not
// change, so there is no from/to to render, and the digest is 71 characters
// that say nothing a person can act on.
func TestDigestMoveShowsTheTagNotTheDigest(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	e := entry("caddy", digest, digest, "")
	e.Tag = "latest"
	s := &Server{Source: &fakeSource{pending: []Hosted{e}}, Version: "v1"}

	_, body := get(t, s, "/")
	if !strings.Contains(body, "digest") {
		t.Error("a digest move is not labelled")
	}
	if !strings.Contains(body, "latest") {
		t.Error("a digest move does not show the tag it follows")
	}
	// No hex at all: not the full digest, and not a shortened one either.
	if strings.Contains(body, "sha256:") {
		t.Errorf("a digest is rendered where the tag should be")
	}
}
