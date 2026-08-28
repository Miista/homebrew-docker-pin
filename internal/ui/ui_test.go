package ui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/internal/registry"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
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
			Kind: watch.KindTag, Candidate: "1.3.0", Bump: registry.KindMinor,
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
	if !strings.Contains(body, "sha256:41b194461e4b") {
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

// The row must say why it is waiting, not just how big the change is: "minor"
// alone does not tell you whether the threshold was too low or absent.
func TestIndex_ShowsWhyItIsWaiting(t *testing.T) {
	s := &Server{Source: fakeSource{pending: []watch.Pending{
		{Service: "app", Kind: watch.KindTag, Candidate: "2.0.0",
			Bump: registry.KindMajor, Why: "major exceeds duva.auto: patch"},
	}}}
	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "major exceeds duva.auto: patch") {
		t.Errorf("explanation missing from the row:\n%s", body)
	}
	if !strings.Contains(body, "kind-major") {
		t.Error("kind class missing")
	}
}

// --- applying from the page ---------------------------------------------

type fakeApplier struct {
	applied []string
	note    string
	err     error
}

func (f *fakeApplier) Apply(service string) (string, error) {
	f.applied = append(f.applied, service)
	return f.note, f.err
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
			{Service: "app", Kind: watch.KindTag, Candidate: "2.0.0", Bump: registry.KindMajor},
		}},
		Applier: applier,
	}
}

func TestApply_RunsTheUpdate(t *testing.T) {
	fa := &fakeApplier{note: "updated to 2.0.0"}
	rec := post(t, queueWith(fa), "service=app")

	if len(fa.applied) != 1 || fa.applied[0] != "app" {
		t.Fatalf("applied = %v, want [app]", fa.applied)
	}
	// A redirect rather than a rendered page, so refreshing does not repeat
	// the update.
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "updated+to+2.0.0") {
		t.Errorf("the result should be carried back to the page: %q", loc)
	}
}

func TestApply_ReportsFailure(t *testing.T) {
	fa := &fakeApplier{err: errors.New("recreate failed")}
	rec := post(t, queueWith(fa), "service=app")

	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "level=error") {
		t.Errorf("a failure should be marked as one: %q", loc)
	}
	if !strings.Contains(loc, "recreate+failed") {
		t.Errorf("the cause should be carried back: %q", loc)
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

// And the form is not offered either, so the page cannot suggest an action
// that would 404. The form is what is asserted, not the button's label: what
// matters is whether the page can submit, and wording is free to change.
// The queue shows the rule, not only the verdict it produced. A service set
// to apply more than intended is the easy misconfiguration, and it is
// invisible if the policy is mentioned only in the explanation of what it
// rejected.
func TestIndex_ShowsThePolicy(t *testing.T) {
	s := &Server{Source: fakeSource{pending: []watch.Pending{
		{Service: "app", Kind: watch.KindTag, Candidate: "2.0.0",
			Bump: registry.KindMajor, Auto: "patch"},
	}}}
	body := get(t, s, "/").Body.String()
	if !strings.Contains(body, "duva.auto") {
		t.Errorf("the queue should name the policy it judged against:\n%s", body)
	}
	if !strings.Contains(body, ">patch<") {
		t.Errorf("the policy's value should be shown:\n%s", body)
	}
}

// An absent duva.auto means none -- the default, and the reason most services
// are in the queue at all. A blank cell would read as "unknown".
func TestIndex_AbsentPolicyReadsAsNone(t *testing.T) {
	body := get(t, queueWith(nil), "/").Body.String()
	if !strings.Contains(body, ">none<") {
		t.Errorf("an unset policy should show as none:\n%s", body)
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
			Candidate: "1.3.0", Bump: registry.KindMinor,
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
		Bump: registry.KindMajor, Remaining: "6 days", Outcome: "moves to approval",
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
