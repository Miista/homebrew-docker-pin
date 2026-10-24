package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// fakeSource is a queue without queues behind it.
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
		Entry: queue.Entry{
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

	for _, sel := range []string{".steps li", ".banner"} {
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

// The queue applies one service at a time across the whole host, holding
// that lock for the entire transaction. A second click is not refused: Start
// accepts it, spawns a goroutine, and parks on the mutex -- so the row shows
// "Updating…" and a step panel while doing nothing at all, for as long as the
// first apply takes. Indistinguishable from real work.
//
// So every button on a host with an apply in flight has to be disabled, not
// only the row that started it.
func TestOtherRowsOnABusyHostAreDisabled(t *testing.T) {
	s := &Server{Source: &fakeSource{}, Approver: &fakeApprover{}}
	_, body := get(t, s, "/")

	if !strings.Contains(body, "busyHost(r.host)") {
		t.Error("the Update button does not consider whether the host is already applying")
	}
	// Scoped to the host, not the whole page: two hosts apply independently,
	// and one queue's lock says nothing about another's.
	if !strings.Contains(body, "this.running(r.key) && r.host === host") {
		t.Error("busyHost is not scoped to the host, so one host's apply would grey out every other host too")
	}
}

// On a phone the column head is hidden, and it was the head that drew the
// card's top edge -- main > .rows sets border-top: 0 for exactly that reason.
// Without it the first row's own border-top becomes the visible top of the
// card, and because a row sits inside the card's horizontal padding that
// border is inset on both sides: a short line floating above the list rather
// than the card's edge.
func TestTheCardDrawsItsOwnTopEdgeWithoutTheHead(t *testing.T) {
	s := &Server{Source: &fakeSource{}, Approver: &fakeApprover{}}
	_, body := get(t, s, "/")

	phone := mediaBlock(body, "max-width: 30rem")
	if phone == "" {
		t.Fatal("no narrow-phone media query in the page")
	}
	if !strings.Contains(phone, ".head { display: none; }") {
		t.Fatal("the head is no longer hidden on phones; this test guards the consequence of that")
	}
	if !strings.Contains(ruleFor(phone, "main > .rows"), "border-top") {
		t.Error("the card does not draw its own top edge, so the first row's inset border shows as one")
	}
	if !strings.Contains(ruleFor(phone, ".rows > li:first-child"), "border-top: 0") {
		t.Error("the first row still draws a border, which doubles the card's edge")
	}
}

// mediaBlock returns the body of the @media block whose condition contains
// cond, so a test can assert on the phone layout without matching rules that
// only apply on a desktop.
func mediaBlock(page, cond string) string {
	i := strings.Index(page, cond)
	if i < 0 {
		return ""
	}
	rest := page[i:]
	open := strings.Index(rest, "{")
	if open < 0 {
		return ""
	}
	depth, start := 0, open
	for j := open; j < len(rest); j++ {
		switch rest[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[start+1 : j]
			}
		}
	}
	return ""
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

// A click must reach the queue that owns the service, carrying the host --
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

// An unreachable queue must reach the page: a queue missing a host reads
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

// A row whose queue has no updater, or has stopped answering, must reach the
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
		t.Error("a row from an unreachable queue is not marked stale")
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

// A finished apply must stop reading as one in flight.
//
// The button and the host lock used to ask whether a row had any steps, and
// nothing ever removed them -- so a completed apply left the row saying
// "Updating…" forever and, through busyHost, held every button on that host
// grey. A terminal status is what ends a run, so that is what they ask about.
func TestAFinishedApplyIsNotStillRunning(t *testing.T) {
	s := &Server{
		Source:   &fakeSource{pending: []Hosted{entry("authelia", "4.39.20", "4.39.26", version.KindPatch)}},
		Approver: &fakeApprover{},
	}
	_, body := get(t, s, "/")

	if !strings.Contains(body, "return !!this.steps[key] && !this.finished[key]") {
		t.Error("running() does not treat a terminal status as the end of a run")
	}
	// The stale tests it replaced, in every place that decided "in flight".
	for _, stale := range []string{`!!steps[r.key] || busyHost`, `steps[r.key] ? "Updating…"`} {
		if strings.Contains(body, stale) {
			t.Errorf("a row still reads %q as running, so it stays stuck after it finishes", stale)
		}
	}
}

// A failed apply keeps its panel: the entry is still pending and that panel is
// the only account of what went wrong. A succeeded one is cleared, because its
// row has already left the queue.
func TestOnlyASucceededApplyClearsItsPanel(t *testing.T) {
	s := &Server{
		Source:   &fakeSource{pending: []Hosted{entry("authelia", "4.39.20", "4.39.26", version.KindPatch)}},
		Approver: &fakeApprover{},
	}
	_, body := get(t, s, "/")

	if !strings.Contains(body, `if ((this.finished[key] || "").startsWith("failed")) return`) {
		t.Error("a failed apply does not keep its panel, so its explanation is thrown away")
	}
}

// The registry host is dropped from an image shown to a person.
//
// Almost every image is docker.io or ghcr.io, so the prefix distinguishes
// nothing while costing width the repository and tag need. A private registry
// is kept: registry.example.com/hemma-agent is not hemma-agent.
func TestTheRegistryHostIsStrippedFromWhatIsShown(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ghcr.io/linuxserver/radarr", "linuxserver/radarr"},
		{"docker.io/happierdev/relay-server", "happierdev/relay-server"},
		// Already short, and must not be mangled.
		{"deluan/navidrome", "deluan/navidrome"},
		// A private registry is the point of its own name.
		{"registry.example.com/hemma-agent", "registry.example.com/hemma-agent"},
		// Only a prefix. A mirror that carries the host mid-path is a
		// different image, and cutting the middle out of it would name one
		// that does not exist.
		{"mirror.example.com/ghcr.io/linuxserver/radarr", "mirror.example.com/ghcr.io/linuxserver/radarr"},
	} {
		if got := shortImage(c.in); got != c.want {
			t.Errorf("shortImage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The queue can be narrowed to one host, and searched.
//
// Thirty-three rows across two hosts is a list nobody reads: the tabs answer
// "what is waiting on this box" and the search answers "where is that one
// service", which are the two questions actually asked of it.
func TestTheQueueCanBeNarrowed(t *testing.T) {
	s := &Server{
		Source:   &fakeSource{pending: []Hosted{entry("authelia", "4.39.20", "4.39.26", version.KindPatch)}},
		Approver: &fakeApprover{},
	}
	_, body := get(t, s, "/")

	// The table renders the filtered view, not the whole queue.
	if !strings.Contains(body, `v-for="r in shown()"`) {
		t.Error("the table does not render the filtered rows")
	}
	// Tabs come from the rows, so a host with nothing waiting has none.
	if !strings.Contains(body, "new Set(this.rows.map(r => r.host))") {
		t.Error("the host tabs are not derived from what is queued")
	}
	// Searching covers the fields a person would type: the service, the image
	// it runs, and either end of the change.
	for _, field := range []string{"r.service", "r.image", "r.current_tag", "r.candidate"} {
		if !strings.Contains(body, field+" +") && !strings.Contains(body, "+ "+field) {
			t.Errorf("the search does not cover %s", field)
		}
	}
}

// Filtering narrows what is shown, never what is known.
//
// busyHost asks whether any row on a host is applying, and the heading counts
// what is waiting. Both ask about the whole queue, so narrowing rows itself
// would make them answer about whatever happens to be on screen -- a second
// apply would be allowed on a host whose running one had been filtered away.
func TestFilteringDoesNotNarrowWhatIsKnown(t *testing.T) {
	s := &Server{
		Source:   &fakeSource{pending: []Hosted{entry("authelia", "4.39.20", "4.39.26", version.KindPatch)}},
		Approver: &fakeApprover{},
	}
	_, body := get(t, s, "/")

	// The count and the host lock read rows, which stays whole.
	if !strings.Contains(body, "({{ rows.length }})") {
		t.Error("the heading counts something other than the whole queue")
	}
	if !strings.Contains(body, "this.rows.some(r => this.running(r.key) && r.host === host)") {
		t.Error("busyHost no longer asks the whole queue, so a filtered-away apply would be missed")
	}
	// And poll() replaces rows wholesale rather than a filtered copy.
	if !strings.Contains(body, "this.rows = s.rows || []") {
		t.Error("poll does not refill the whole queue")
	}
}
