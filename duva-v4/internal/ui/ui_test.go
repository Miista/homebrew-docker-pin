package ui

import (
	"encoding/json"
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
	blocked     []Blocked
	pending     []Hosted
	unreachable []Problem
}

func (f *fakeSource) Pending() []Hosted      { return f.pending }
func (f *fakeSource) Unreachable() []Problem { return f.unreachable }
func (f *fakeSource) Blocked() []Blocked     { return f.blocked }

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
		CanApply: true,
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

// The page must render, and must carry its own first-paint data.
//
// Two things at once, because they fail the same way. A template naming a
// field the data does not carry fails mid-render, after a 200 and half a page
// have gone out -- so the status proves nothing and only executing it catches
// a rename. And the values below are found in the embedded state, not in
// markup: the rows are client-rendered now, so what this proves is that the
// page arrives knowing what to draw rather than having to ask first.
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

// A digest is 71 characters with no space or hyphen, so a browser has nowhere
// to break it. Without a rule that breaks mid-token it runs past the panel and
// off the side of the screen -- which is what an apply looked like on a phone:
// the pull step ran off the viewport and most of it could not be read.
//
// Both places that show one need it. The steps panel streams "pulling
// <image>@sha256:..." during every apply, and a failure reason carries the
// pinned image line the same way.
func TestLongDigestsAreAllowedToWrap(t *testing.T) {
	s := &Server{
		Source:   &fakeSource{},
		Approver: &fakeApprover{},
	}
	_, body := get(t, s, "/")

	for _, sel := range []string{".steps div", ".banner"} {
		rule := ruleFor(body, sel)
		if rule == "" {
			t.Errorf("no %q rule in the page at all", sel)
			continue
		}
		// overflow-wrap: break-word is not enough -- it breaks between words,
		// and a digest is one word.
		if !strings.Contains(rule, "overflow-wrap: anywhere") {
			t.Errorf("%s cannot break a digest, so it will overflow:\n  %s", sel, rule)
		}
	}
}

// ruleFor returns the declaration block for a selector, so a test can assert
// on what the page actually carries rather than on a substring appearing
// somewhere in 400 lines of markup.
func ruleFor(page, selector string) string {
	i := strings.Index(page, selector+" {")
	if i < 0 {
		return ""
	}
	rest := page[i+len(selector)+2:]
	end := strings.Index(rest, "}")
	if end < 0 {
		return ""
	}
	return strings.Join(strings.Fields(rest[:end]), " ")
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

// --- the JSON the page actually renders from ---------------------------------
//
// The page is markup plus directives now, so asserting on rendered HTML would
// be asserting on what the browser does. These check the contract the page is
// given instead, which is the thing this process is responsible for.
func stateOf(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/state = %d, want 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding /api/state: %v", err)
	}
	return out
}

// An unreachable decider must reach the page: a queue missing a host reads
// exactly like that host having nothing to do.
func TestStateReportsUnreachable(t *testing.T) {
	s := &Server{Source: &fakeSource{
		unreachable: []Problem{{Host: "pi", Err: "connection refused"}},
	}, Version: "v1"}

	got := stateOf(t, s)
	un, _ := got["unreachable"].([]any)
	if len(un) != 1 {
		t.Fatalf("unreachable = %v, want one entry", got["unreachable"])
	}
	first, _ := un[0].(map[string]any)
	if first["Host"] != "pi" || first["Err"] != "connection refused" {
		t.Errorf("unreachable[0] = %v, want pi and its reason", first)
	}
}

// Empty means empty, not null: Go encodes a nil slice as JSON null, and a
// page reading .length off it throws before rendering anything at all.
func TestStateNeverSendsNullSlices(t *testing.T) {
	s := &Server{Source: &fakeSource{}, Version: "v1"}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	body := rec.Body.String()
	for _, field := range []string{`"rows":null`, `"unreachable":null`, `"blocked":null`} {
		if strings.Contains(body, field) {
			t.Errorf("%s would throw in the browser: %s", field, body)
		}
	}
}

// Without an Approver the apply routes must be absent, not merely refusing.
func TestReadOnlyRegistersNoApplyRoutes(t *testing.T) {
	s := &Server{Source: &fakeSource{
		pending: []Hosted{entry("authelia", "1.0.0", "1.0.1", version.KindPatch)},
	}}

	for _, path := range []string{"/apply", "/api/apply/optiplex/authelia"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader("service=optiplex/authelia"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusSeeOther || rec.Code == http.StatusOK {
			t.Errorf("%s answered %d on a read-only UI", path, rec.Code)
		}
	}
	// And the page is told, so it renders no buttons at all.
	if stateOf(t, s)["can_apply"] != false {
		t.Error("a read-only UI reports can_apply true")
	}
}

// A row whose decider has no actor, or has stopped answering, must reach the
// page marked so -- the page greys the button, but only if it is told.
func TestStateMarksRowsThatCannotBeApplied(t *testing.T) {
	noActor := entry("gluetun", "1.0", "1.1", version.KindMinor)
	noActor.CanApply = false
	stale := entry("prowlarr", "1.0", "1.1", version.KindMinor)
	stale.Stale = true

	s := &Server{
		Source:   &fakeSource{pending: []Hosted{noActor, stale}, blocked: []Blocked{{Host: "optiplex", Kind: "no-actor", Reason: "no actor is configured"}}},
		Approver: &fakeApprover{}, Version: "v1",
	}

	got := stateOf(t, s)
	rows, _ := got["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m["can_apply"] != false {
			t.Errorf("%v is applyable although it should not be", m["service"])
		}
	}
	if second, _ := rows[1].(map[string]any); second["stale"] != true {
		t.Error("a row from an unreachable decider is not marked stale")
	}
	if wa, _ := got["blocked"].([]any); len(wa) != 1 {
		t.Error("the host with no actor is not named")
	}
}

// A digest move sends the tag, not 71 characters of hex.
func TestStateSendsTheTagForADigestMove(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	e := entry("caddy", digest, digest, "")
	e.Tag = "latest"
	s := &Server{Source: &fakeSource{pending: []Hosted{e}}, Version: "v1"}

	rows, _ := stateOf(t, s)["rows"].([]any)
	first, _ := rows[0].(map[string]any)
	if first["moved"] != true {
		t.Error("a digest move is not marked moved")
	}
	if first["tag"] != "latest" {
		t.Errorf("tag = %v, want latest", first["tag"])
	}
	if first["kind"] != "digest" {
		t.Errorf("kind = %v, want digest", first["kind"])
	}
}
