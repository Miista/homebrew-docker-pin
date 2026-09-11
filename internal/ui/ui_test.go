package ui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/internal/watch"
	"github.com/Miista/homebrew-docker-pin/oci/version"
)

type fakeSource struct {
	pending []watch.Pending
	soaking []watch.Soaking
}

func (f fakeSource) Pending() []watch.Pending { return f.pending }
func (f fakeSource) Soaking() []watch.Soaking { return f.soaking }
func (f fakeSource) LastCheck() string        { return "2 hours ago" }
func (f fakeSource) LastCheckExact() string   { return "2026-01-01 00:00:00 CET" }

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestIndex_RendersPendingRows(t *testing.T) {
	s := &Server{Source: fakeSource{pending: []watch.Pending{
		{Service: "app", Image: "example.com/app", CurrentTag: "1.2.0",
			Kind: watch.KindTag, Candidate: "1.3.0", Bump: version.KindMinor,
			Why:       "minor exceeds duva.auto: patch",
			FirstSeen: "2026-01-01T00:00:00Z", Auto: "patch"},
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
	if !strings.Contains(body, "sha256:"+strings.TrimPrefix(full, "sha256:")[:12]) {
		t.Error("short digest missing")
	}
}

func TestIndex_EmptyQueue(t *testing.T) {
	// With an Applier, a row would carry a form -- so its absence means there
	// are no rows, which is what "empty" means to a reader. The sentence
	// saying so is free to change.
	s := &Server{Source: fakeSource{}, Applier: &fakeApplier{}}
	body := get(t, s, "/").Body.String()

	if strings.Contains(body, `action="/apply"`) {
		t.Errorf("an empty queue should offer nothing to update:\n%s", body)
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

// --- applying from the page ---------------------------------------------

type fakeApplier struct {
	applied  []string
	note     string
	err      error
	startErr error
	// progress, once set, is returned verbatim -- for tests that care about
	// an update still in flight. Tests that only care about the eventual
	// result leave it unset and get one synthesized from note/err instead.
	progress    Progress
	hasProgress bool
	hasStart    bool
}

func (f *fakeApplier) Start(service string) error {
	f.applied = append(f.applied, service)
	f.hasStart = true
	return f.startErr
}

func (f *fakeApplier) Progress(service string) (Progress, bool) {
	if !f.hasStart {
		return Progress{}, false
	}
	if f.hasProgress {
		return f.progress, true
	}
	p := Progress{Done: true, Message: f.note, Failed: f.err != nil}
	if f.err != nil {
		p.Message = f.err.Error()
	}
	return p, true
}

func post(t *testing.T, s *Server, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/apply", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func queueWith(applier Applier) *Server {
	return &Server{
		Source: fakeSource{pending: []watch.Pending{
			{Service: "app", Kind: watch.KindTag, Candidate: "2.0.0", Bump: version.KindMajor},
		}},
		Applier: applier,
	}
}

func TestApply_StartsTheUpdate(t *testing.T) {
	fa := &fakeApplier{note: "updated to 2.0.0"}
	rec := post(t, queueWith(fa), "service=app")

	if len(fa.applied) != 1 || fa.applied[0] != "app" {
		t.Fatalf("applied = %v, want [app]", fa.applied)
	}
	// A redirect rather than a rendered page, so refreshing does not repeat
	// the update. Unlike the old synchronous handler, the result is not in
	// the redirect: Start only reports whether the update *began*, and the
	// page's poller learns the outcome from /progress.
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "service=app") {
		t.Errorf("the redirect should name the service, for the poller: %q", loc)
	}
}

// A Start-time error -- nothing queued under that name, or one already
// running -- is a synchronous failure to *begin*, distinct from the
// transaction itself failing partway through, which only /progress reports.
func TestApply_ReportsAStartFailure(t *testing.T) {
	fa := &fakeApplier{startErr: errors.New("app has no update waiting")}
	rec := post(t, queueWith(fa), "service=app")

	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "level=error") {
		t.Errorf("a failure to start should be marked as one: %q", loc)
	}
	if !strings.Contains(loc, "no+update+waiting") {
		t.Errorf("the cause should be carried back: %q", loc)
	}
}

// --- polling for progress ------------------------------------------------

func TestProgress_ReportsStepsAsTheyArrive(t *testing.T) {
	fa := &fakeApplier{hasStart: true, hasProgress: true, progress: Progress{Steps: []string{"pulling app for app"}}}
	s := queueWith(fa)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/progress?service=app", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pulling app for app") {
		t.Errorf("body missing the in-progress step: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"done":true`) {
		t.Error("an update mid-transaction should not report done")
	}
}

func TestProgress_UnknownServiceIs404(t *testing.T) {
	s := queueWith(&fakeApplier{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/progress?service=nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestProgress_EndpointAbsentWhenReadOnly(t *testing.T) {
	s := queueWith(nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/progress?service=app", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// The banner names the service and says what happened, so the page is not
// silently identical after a click.
func TestIndex_ShowsTheApplyResult(t *testing.T) {
	s := queueWith(&fakeApplier{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/?service=app&level=ok&message=updated+to+2.0.0", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "updated to 2.0.0") {
		t.Errorf("the result should be shown:\n%s", body)
	}
}

// A link a browser might prefetch must never restart a container.
func TestApply_RefusesGET(t *testing.T) {
	fa := &fakeApplier{}
	s := queueWith(fa)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/apply?service=app", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if len(fa.applied) != 0 {
		t.Error("a GET must not apply anything")
	}
}

func TestApply_RequiresAService(t *testing.T) {
	fa := &fakeApplier{}
	if rec := post(t, queueWith(fa), ""); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if len(fa.applied) != 0 {
		t.Error("nothing should have been applied")
	}
}

// With no Applier the endpoint is absent, not merely refusing: an endpoint
// that restarts containers should not exist on a duva meant only to report.
func TestApply_EndpointAbsentWhenReadOnly(t *testing.T) {
	s := queueWith(nil)
	if rec := post(t, s, "service=app"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestIndex_NoFormWhenReadOnly(t *testing.T) {
	body := get(t, queueWith(nil), "/").Body.String()
	if strings.Contains(body, `action="/apply"`) {
		t.Errorf("a read-only queue should offer no way to submit:\n%s", body)
	}
}

func TestIndex_FormWhenApplierPresent(t *testing.T) {
	body := get(t, queueWith(&fakeApplier{}), "/").Body.String()
	if !strings.Contains(body, `action="/apply"`) {
		t.Error("the form should post to /apply, where the handler is")
	}
}

// Soaking updates are shown apart from the queue: a pending one is waiting
// for a decision, a soaking one has been decided and is waiting for time.
// They look the same to act on, which is the point -- the soak is a default,
// not a lock.
func TestIndex_ShowsWhatIsSoaking(t *testing.T) {
	s := &Server{
		Source: fakeSource{soaking: []watch.Soaking{{
			Service: "app", Image: "example.com/app", CurrentTag: "1.2.0",
			Candidate: "1.3.0", Bump: version.KindMinor,
			Remaining: "4 days", Outcome: "will be applied automatically",
		}}},
		Applier: &fakeApplier{},
	}
	body := get(t, s, "/").Body.String()

	// The service and its candidate, which are data rather than copy: a
	// reader cannot act on a row that does not say what it offers.
	for _, want := range []string{"app", "1.3.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page should show %q:\n%s", want, body)
		}
	}
	// Actionable: taking one early is the reason it is shown at all.
	if !strings.Contains(body, `action="/apply"`) {
		t.Error("a soaking update should be offered for updating early")
	}
}

// What happens when the wait ends depends on the policy, so the page must not
// promise an automatic apply for something that will need approval.
func TestIndex_SoakingOutcomeIsNotAssumed(t *testing.T) {
	s := &Server{Source: fakeSource{soaking: []watch.Soaking{{
		Service: "app", CurrentTag: "1.2.0", Candidate: "2.0.0",
		Bump: version.KindMajor, Remaining: "6 days", Outcome: "moves to approval",
	}}}}
	body := get(t, s, "/").Body.String()

	// The outcome comes from the state, not from the template: what the page
	// must not do is invent one. Whatever Soaking.Outcome said is what
	// appears, so a policy change shows up here without the page deciding
	// anything.
	if !strings.Contains(body, "moves to approval") {
		t.Errorf("the page should render the recorded outcome:\n%s", body)
	}
	if !strings.Contains(body, "6 days") {
		t.Errorf("the page should render the recorded remaining time:\n%s", body)
	}
}

// The two lists are separate, so an empty queue must not hide a soaking one.
func TestIndex_SoakingShowsWithAnEmptyQueue(t *testing.T) {
	s := &Server{Source: fakeSource{soaking: []watch.Soaking{{
		Service: "app", CurrentTag: "1.2.0", Candidate: "1.3.0",
		Remaining: "2 days", Outcome: "moves to approval",
	}}}}
	body := get(t, s, "/").Body.String()

	// The soaking candidate is rendered even though the approval queue has
	// nothing in it: they are two lists, and one being empty must not hide
	// the other.
	if !strings.Contains(body, "1.3.0") {
		t.Errorf("the soaking list should still be shown:\n%s", body)
	}
}

// --- refreshing on demand -------------------------------------------------

type fakeRefresher struct {
	calls       int
	err         error
	running     bool
	done        bool
	progressErr string
}

func (f *fakeRefresher) Refresh() error {
	f.calls++
	return f.err
}

func (f *fakeRefresher) RefreshProgress() (running, done bool, err string) {
	return f.running, f.done, f.progressErr
}

func postRefresh(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/refresh", nil))
	return rec
}

func TestRefresh_RunsACheck(t *testing.T) {
	fr := &fakeRefresher{}
	s := &Server{Source: fakeSource{}, Refresher: fr}
	rec := postRefresh(t, s)

	if fr.calls != 1 {
		t.Fatalf("Refresh called %d times, want 1", fr.calls)
	}
	// A redirect rather than a rendered page, so refreshing the browser does
	// not repeat the check.
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
}

// Refresh only errors synchronously when a check is already running -- a
// failure in the check itself surfaces through RefreshProgress instead,
// since Refresh does not wait for it.
func TestRefresh_ReportsAStartFailure(t *testing.T) {
	fr := &fakeRefresher{err: errors.New("a check is already running")}
	s := &Server{Source: fakeSource{}, Refresher: fr}
	rec := postRefresh(t, s)

	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "level=error") {
		t.Errorf("a failure to start should be marked as one: %q", loc)
	}
	if !strings.Contains(loc, "already+running") {
		t.Errorf("the cause should be carried back: %q", loc)
	}
}

func TestRefreshProgress_ReportsRunning(t *testing.T) {
	fr := &fakeRefresher{running: true}
	s := &Server{Source: fakeSource{}, Refresher: fr}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refresh-progress", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"running":true`) {
		t.Errorf("body should report running: %s", rec.Body.String())
	}
}

func TestRefreshProgress_ReportsFailure(t *testing.T) {
	fr := &fakeRefresher{done: true, progressErr: "registry unreachable"}
	s := &Server{Source: fakeSource{}, Refresher: fr}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refresh-progress", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `"done":true`) || !strings.Contains(body, "registry unreachable") {
		t.Errorf("body should report the failure: %s", body)
	}
}

func TestRefreshProgress_EndpointAbsentWithoutRefresher(t *testing.T) {
	s := &Server{Source: fakeSource{}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/refresh-progress", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// A link a browser might prefetch must never trigger a check.
func TestRefresh_RefusesGET(t *testing.T) {
	fr := &fakeRefresher{}
	s := &Server{Source: fakeSource{}, Refresher: fr}
	rec := get(t, s, "/refresh")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if fr.calls != 0 {
		t.Error("a GET must not trigger a check")
	}
}

// With no Refresher the endpoint is absent, not merely refusing: the same
// contract Applier has when duva is meant only to report.
func TestRefresh_EndpointAbsentWithoutRefresher(t *testing.T) {
	s := &Server{Source: fakeSource{}}
	rec := postRefresh(t, s)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestIndex_NoRefreshButtonWithoutRefresher(t *testing.T) {
	body := get(t, &Server{Source: fakeSource{}}, "/").Body.String()
	if strings.Contains(body, `action="/refresh"`) {
		t.Errorf("no Refresher means no button:\n%s", body)
	}
}

func TestIndex_RefreshButtonWhenRefresherPresent(t *testing.T) {
	body := get(t, &Server{Source: fakeSource{}, Refresher: &fakeRefresher{}}, "/").Body.String()
	if !strings.Contains(body, `action="/refresh"`) {
		t.Error("the button should post to /refresh, where the handler is")
	}
}

// --- embedded images -------------------------------------------------------

func TestLogo_ServesThePNG(t *testing.T) {
	rec := get(t, &Server{Source: fakeSource{}}, "/logo.png")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("logo.png served an empty body")
	}
}

func TestFavicon_ServesThePNG(t *testing.T) {
	rec := get(t, &Server{Source: fakeSource{}}, "/favicon.png")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("favicon.png served an empty body")
	}
}

func TestIndex_ReferencesTheLogoAndFavicon(t *testing.T) {
	body := get(t, &Server{Source: fakeSource{}}, "/").Body.String()
	if !strings.Contains(body, `src="/logo.png"`) {
		t.Error("the header should show the logo")
	}
	if !strings.Contains(body, `href="/favicon.png"`) {
		t.Error("the page should link the favicon")
	}
}

// A hub's rows carry the host they came from, and post it back as part of the
// key -- otherwise two services named the same on different hosts are one
// indistinguishable row, and clicking either applies to whichever the
// Applier happened to find first.
func TestIndex_HubRowsCarryHostAndRouteByKey(t *testing.T) {
	s := &Server{Source: fakeSource{pending: []watch.Pending{
		{Service: "caddy", Host: "optiplex", Image: "caddy", CurrentTag: "2.11.4-alpine",
			Kind: watch.KindTag, Candidate: "2.12.0-alpine", Bump: version.KindMinor},
		{Service: "caddy", Host: "pi", Image: "caddy", CurrentTag: "2.11.4-alpine",
			Kind: watch.KindTag, Candidate: "2.12.0-alpine", Bump: version.KindMinor},
	}}, Host: "hub", Version: "test", Applier: &fakeApplier{}}

	body := get(t, s, "/").Body.String()
	for _, want := range []string{
		`value="optiplex/caddy"`,
		`value="pi/caddy"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q -- the row must post back a routable key", want)
		}
	}
	// And the host must be visible, not merely encoded in the form.
	if strings.Count(body, "optiplex") == 0 || strings.Count(body, ">pi<") == 0 {
		t.Error("the host should be shown on the row")
	}
}

// A single-host duva posts the bare service name, exactly as before: its
// Applier knows nothing about hosts, and a key it did not expect would not
// route.
func TestIndex_LocalRowsPostBareServiceName(t *testing.T) {
	s := &Server{Source: fakeSource{pending: []watch.Pending{
		{Service: "caddy", Image: "caddy", CurrentTag: "2.11.4-alpine",
			Kind: watch.KindTag, Candidate: "2.12.0-alpine", Bump: version.KindMinor},
	}}, Host: "optiplex", Version: "test", Applier: &fakeApplier{}}

	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, `value="caddy"`) {
		t.Error(`want value="caddy" -- a local duva must post the bare service name`)
	}
	if strings.Contains(body, `value="optiplex/caddy"`) {
		t.Error("a local duva must not post a host-qualified key")
	}
}
