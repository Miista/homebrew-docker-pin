package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/fixture"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

// These exercise duva as a whole: a real compose file, the real compose
// parser, real policy, real state on a real disk. Only the registry is faked,
// because it is the one dependency that leaves the machine.
//
// Nothing here asserts on printed output or on a specific version number. The
// inputs are randomised, so what is asserted is the rule -- "a service whose
// update policy allows is not left waiting" -- rather than an example of it.

// app runs one full check against a generated project, returning the findings
// and the state duva persisted. It goes through the same code path as
// `duva run`, including loading and saving state from disk.
func app(t *testing.T, project string, reg fixture.RegistryAnswers) ([]watch.Finding, *watch.State) {
	t.Helper()
	return newApp(t, project).run(t, reg)
}

// runner keeps a project and its state file across several checks, for the
// behaviour that only exists over time -- a moving tag's baseline, a
// notification firing once, a queue entry surviving a restart.
type runner struct {
	project string
	state   string
}

// testLog discards: these tests assert on what duva did, not on what it
// said. The lines themselves are covered by TestReport.
func testLog() zerolog.Logger { return zerolog.New(io.Discard) }

func newApp(t *testing.T, project string) *runner {
	t.Helper()
	origCompose, origState := composeDir, stateFile
	t.Cleanup(func() { composeDir, stateFile = origCompose, origState })
	composeDir = filepath.Dir(project)
	stateFile = filepath.Join(t.TempDir(), "duva.json")
	return &runner{project: project, state: stateFile}
}

// run performs one check, loading and saving state exactly as `duva run`
// does, so state genuinely round-trips through disk between runs.
func (r *runner) run(t *testing.T, reg fixture.RegistryAnswers) ([]watch.Finding, *watch.State) {
	t.Helper()
	composeDir, stateFile = filepath.Dir(r.project), r.state

	st, err := watch.LoadState(r.state)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := check(envConfig{}, testLog(), watch.Registry{
		ListMatchingTags: reg.ListMatchingTags,
		RemoteDigest:     reg.RemoteDigest,
		TagCreated:       reg.TagCreated,
	}, st, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(r.state); err != nil {
		t.Fatal(err)
	}
	return findings, st
}

func findingFor(t *testing.T, findings []watch.Finding, service string) watch.Finding {
	t.Helper()
	for _, f := range findings {
		if f.Service == service {
			return f
		}
	}
	t.Fatalf("no finding for %q in %+v", service, findings)
	return watch.Finding{}
}

// Given a service with an update policy allows, duva must not leave it
// waiting for anyone.
func TestApp_AutoApplicableUpdateIsNotQueued(t *testing.T) {
	f := fixture.New(t)
	for _, tc := range []struct {
		bump fixture.Bump
		auto string
	}{
		{fixture.BumpPatch, "patch"},
		{fixture.BumpPatch, "minor"},
		{fixture.BumpPatch, "major"},
		{fixture.BumpMinor, "minor"},
		{fixture.BumpMinor, "major"},
		{fixture.BumpMajor, "major"},
	} {
		svc := f.WithAuto(f.WithUpdate(f.PinnedService(), tc.bump), tc.auto)
		findings, st := app(t, f.Project(svc), f.Registry(svc))

		got := findingFor(t, findings, svc.Name)
		if !got.AutoApplies() {
			t.Errorf("%s bump under duva.auto: %s should apply, got %s (%s)",
				tc.bump, tc.auto, got.Decision, got.Why)
		}
		if _, queued := st.Pending[svc.Name]; queued {
			t.Errorf("%s bump under duva.auto: %s must not be queued", tc.bump, tc.auto)
		}
	}
}

// And the converse: an update policy does not allow must be waiting, with a
// reason attached.
func TestApp_UpdateBeyondPolicyIsQueuedWithAReason(t *testing.T) {
	f := fixture.New(t)
	for _, tc := range []struct {
		bump fixture.Bump
		auto string
	}{
		{fixture.BumpPatch, "none"},
		{fixture.BumpMinor, "patch"},
		{fixture.BumpMajor, "patch"},
		{fixture.BumpMajor, "minor"},
	} {
		svc := f.WithAuto(f.WithUpdate(f.PinnedService(), tc.bump), tc.auto)
		findings, st := app(t, f.Project(svc), f.Registry(svc))

		got := findingFor(t, findings, svc.Name)
		if !got.NeedsApproval() {
			t.Errorf("%s bump under duva.auto: %s should need approval, got %s",
				tc.bump, tc.auto, got.Decision)
		}
		row, queued := st.Pending[svc.Name]
		if !queued {
			t.Fatalf("%s bump under duva.auto: %s should be queued", tc.bump, tc.auto)
		}
		if row.Why == "" {
			t.Error("a queued row must say why it is waiting")
		}
		if row.Candidate == "" || row.CurrentTag == "" {
			t.Errorf("a queued row must describe the change: %+v", row)
		}
	}
}

// A service with nothing newer is not waiting and not reported, whatever its
// policy says.
func TestApp_NoUpdateIsNeverQueued(t *testing.T) {
	f := fixture.New(t)
	for _, auto := range []string{"none", "patch", "minor", "major"} {
		svc := f.WithAuto(f.PinnedService(), auto)
		svc.Labels["duva.include_tags"] = `^\d+\.\d+\.\d+$`
		findings, st := app(t, f.Project(svc), f.Registry(svc))

		if got := findingFor(t, findings, svc.Name); got.Available() {
			t.Errorf("duva.auto: %s reported an update where none exists: %+v", auto, got)
		}
		if len(st.Pending) != 0 {
			t.Errorf("duva.auto: %s queued something: %+v", auto, st.Pending)
		}
	}
}

// Services duva does not watch must not appear anywhere: not as available,
// not in the queue.
func TestApp_UnwatchedServicesAreSkipped(t *testing.T) {
	f := fixture.New(t)
	unpinned, built := f.UnpinnedService(), f.BuiltService()
	findings, st := app(t, f.Project(unpinned, built), f.Registry(unpinned, built))

	for _, name := range []string{unpinned.Name, built.Name} {
		got := findingFor(t, findings, name)
		if got.Status != watch.StatusSkipped {
			t.Errorf("%s: Status = %q, want skipped (%s)", name, got.Status, got.Reason)
		}
	}
	if len(st.Pending) != 0 {
		t.Errorf("an unwatched service was queued: %+v", st.Pending)
	}
}

// The queue is keyed by service, so a project full of services must produce
// exactly the rows policy says need a human -- no more, no fewer.
func TestApp_QueueMatchesPolicyAcrossAProject(t *testing.T) {
	f := fixture.New(t)

	auto := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")
	needs := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpMajor), "patch")
	quiet := f.PinnedService()
	skipped := f.UnpinnedService()
	services := []fixture.Service{auto, needs, quiet, skipped}

	findings, st := app(t, f.Project(services...), f.Registry(services...))
	if len(findings) != len(services) {
		t.Fatalf("expected a finding per service, got %d", len(findings))
	}

	if len(st.Pending) != 1 {
		t.Fatalf("expected exactly one queued row, got %+v", st.Pending)
	}
	if _, ok := st.Pending[needs.Name]; !ok {
		t.Errorf("the wrong service is queued: %+v", st.Pending)
	}
}

// A moving tag reports a digest move once. The first check only records where
// the tag points -- otherwise every newly watched service would report an
// update on day one -- so a move is only detectable on a later run.
func TestApp_MovingTagMoveIsReportedAndPersists(t *testing.T) {
	f := fixture.New(t)
	svc := f.MovingTagService(false) // starts where the compose file pins it
	svc = f.WithAuto(svc, "none")
	a := newApp(t, f.Project(svc))

	if _, st := a.run(t, f.Registry(svc)); len(st.Pending) != 0 {
		t.Fatalf("the first check must only record a baseline, got %+v", st.Pending)
	}

	// The tag now points somewhere else.
	moved := svc
	moved.AvailableDigest = f.Digest()

	findings, st := a.run(t, f.Registry(moved))
	got := findingFor(t, findings, svc.Name)
	if !got.Available() || got.Kind != watch.KindDigest {
		t.Fatalf("a moved tag should be reported as a digest move, got %+v", got)
	}
	if got.Bump != "" {
		t.Errorf("a moving tag has no version pair to classify, got Bump=%q", got.Bump)
	}
	if _, queued := st.Pending[svc.Name]; !queued {
		t.Error("duva.auto: none means the move waits for a human")
	}
}

// An unmoved tag is silent after the first check records where it points.
func TestApp_UnmovedTagIsSilent(t *testing.T) {
	f := fixture.New(t)
	svc := f.MovingTagService(false)
	findings, st := app(t, f.Project(svc), f.Registry(svc))

	if got := findingFor(t, findings, svc.Name); got.Available() {
		t.Errorf("an unmoved tag must not be reported: %+v", got)
	}
	if len(st.Pending) != 0 {
		t.Errorf("nothing should be queued: %+v", st.Pending)
	}
}

// The soak is a precondition, not a policy: a candidate that has not aged
// enough is not a candidate, whatever duva.auto says.
func TestApp_SoakWithholdsFreshCandidatesRegardlessOfPolicy(t *testing.T) {
	f := fixture.New(t)
	for _, auto := range []string{"patch", "minor", "major"} {
		svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), auto)
		svc.Labels["duva.delay"] = "7d"
		svc.Published = time.Now().Add(-2 * time.Hour)

		findings, st := app(t, f.Project(svc), f.Registry(svc))
		if got := findingFor(t, findings, svc.Name); got.Available() {
			t.Errorf("duva.auto: %s adopted a two-hour-old tag against a 7d soak: %+v", auto, got)
		}
		if len(st.Pending) != 0 {
			t.Errorf("duva.auto: %s queued a tag still soaking", auto)
		}
	}
}

// One unreachable registry must not blind duva to the rest of the project.
func TestApp_OneBrokenServiceDoesNotHideTheOthers(t *testing.T) {
	f := fixture.New(t)
	broken := f.WithUpdate(f.PinnedService(), fixture.BumpMajor)
	fine := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpMajor), "none")

	reg := f.Registry(broken, fine)
	reg.Fail = map[string]error{broken.Image: errTest}

	findings, st := app(t, f.Project(broken, fine), reg)

	if got := findingFor(t, findings, broken.Name); got.Status != watch.StatusError {
		t.Errorf("broken service: Status = %q, want error", got.Status)
	}
	if got := findingFor(t, findings, fine.Name); !got.NeedsApproval() {
		t.Errorf("the healthy service should still have been checked: %+v", got)
	}
	if _, ok := st.Pending[fine.Name]; !ok {
		t.Error("the healthy service's update should be queued")
	}
}

var errTest = &testError{"registry unreachable"}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }

// --- valid data, one thing broken --------------------------------------
//
// The generator produces a service that works. Each of these takes that
// working service, breaks exactly one thing, and asserts the consequence of
// that break -- so a failure names the cause rather than leaving you to guess
// which part of an invalid fixture mattered.

// Baseline: the unmodified generated service behaves. If this fails, every
// test below is meaningless, so it is asserted rather than assumed.
func TestApp_GeneratedServiceIsValid(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	findings, _ := app(t, f.Project(svc), f.Registry(svc))
	got := findingFor(t, findings, svc.Name)
	if got.Status != watch.StatusAvailable || !got.AutoApplies() {
		t.Fatalf("the generated fixture must work before anything is broken: %+v", got)
	}
}

// Remove the digest: pin status is duva's opt-in, so the service stops being
// watched entirely -- not an error, just invisible.
func TestApp_BreakingThePin_StopsItBeingWatched(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	svc.Pinned = false // the one break

	findings, st := app(t, f.Project(svc), f.Registry(svc))
	got := findingFor(t, findings, svc.Name)
	if got.Status != watch.StatusSkipped {
		t.Errorf("Status = %q, want skipped: an unpinned service has made no decision to watch", got.Status)
	}
	if len(st.Pending) != 0 {
		t.Errorf("nothing should be queued for an unwatched service: %+v", st.Pending)
	}
}

// Remove duva.include_tags: the service stops being a constrained one and becomes
// a moving-tag follower, which is a different question entirely. Its version
// tag never moves, so nothing is ever found.
func TestApp_RemovingTheIncludeLabel_ChangesWhatIsAsked(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	delete(svc.Labels, "duva.include_tags") // the one break
	svc.AvailableDigest = svc.Digest   // a moving-tag lookup now answers

	findings, _ := app(t, f.Project(svc), f.Registry(svc))
	got := findingFor(t, findings, svc.Name)
	if got.Kind == watch.KindTag {
		t.Errorf("without duva.include_tags this is no longer a tag question: %+v", got)
	}
}

// Corrupt duva.auto: the service errors rather than silently falling back to
// a default. A typo that quietly means "none" is discovered months later,
// when an update that should have applied itself never did.
func TestApp_CorruptingTheAutoLabel_IsAnErrorNotADefault(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	svc.Labels["duva.auto"] = "pathc" // the one break

	findings, st := app(t, f.Project(svc), f.Registry(svc))
	got := findingFor(t, findings, svc.Name)
	if got.Status != watch.StatusError {
		t.Errorf("Status = %q, want error: a misspelled threshold must not read as a default", got.Status)
	}
	if len(st.Pending) != 0 {
		t.Errorf("a service that could not be evaluated must not be queued: %+v", st.Pending)
	}
}

// Misspell the label name itself: same reasoning. Silently ignoring an
// unknown duva.* label looks exactly like duva working.
func TestApp_MisspellingALabelName_IsAnError(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	svc.Labels["duva.includ"] = svc.Labels["duva.include_tags"] // the one break
	delete(svc.Labels, "duva.include_tags")

	findings, _ := app(t, f.Project(svc), f.Registry(svc))
	if got := findingFor(t, findings, svc.Name); got.Status != watch.StatusError {
		t.Errorf("Status = %q, want error for an unknown duva.* label", got.Status)
	}
}

// Break the include regex: an unparseable rule is an error for that service,
// not a rule that quietly matches nothing.
func TestApp_BreakingTheIncludeRegex_IsAnError(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	svc.Labels["duva.include_tags"] = "^(" // the one break

	findings, _ := app(t, f.Project(svc), f.Registry(svc))
	if got := findingFor(t, findings, svc.Name); got.Status != watch.StatusError {
		t.Errorf("Status = %q, want error for an uncompilable regex", got.Status)
	}
}

// Add a build: key. A locally built image's digest exists only on the daemon
// that built it, so pinning one produces a reference no other host can pull.
func TestApp_AddingABuildKey_StopsItBeingWatched(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	svc.Built = true // the one break

	findings, st := app(t, f.Project(svc), f.Registry(svc))
	got := findingFor(t, findings, svc.Name)
	if got.Status != watch.StatusSkipped {
		t.Errorf("Status = %q, want skipped for a locally built image", got.Status)
	}
	if len(st.Pending) != 0 {
		t.Errorf("a built service must never be queued: %+v", st.Pending)
	}
}

// Add a delay the candidate cannot satisfy: the update is withheld rather
// than adopted, and nothing is queued -- a soaking tag is not a candidate at
// all, so there is nothing for a human to approve yet either.
func TestApp_AddingAnUnsatisfiedDelay_WithholdsTheUpdate(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	svc.Labels["duva.delay"] = "30d"               // the one break
	svc.Published = time.Now().Add(-1 * time.Hour) // ...which this makes bite

	findings, st := app(t, f.Project(svc), f.Registry(svc))
	if got := findingFor(t, findings, svc.Name); got.Available() {
		t.Errorf("a one-hour-old tag must not satisfy a 30d soak: %+v", got)
	}
	if len(st.Pending) != 0 {
		t.Errorf("a soaking candidate is not yet a candidate: %+v", st.Pending)
	}
}

// Take the registry away: that service errors, and says why.
func TestApp_BreakingTheRegistry_IsAnErrorThatExplainsItself(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	reg := f.Registry(svc)
	reg.Fail = map[string]error{svc.Image: errTest} // the one break

	findings, _ := app(t, f.Project(svc), reg)
	got := findingFor(t, findings, svc.Name)
	if got.Status != watch.StatusError {
		t.Fatalf("Status = %q, want error when the registry is unreachable", got.Status)
	}
	if got.Reason == "" {
		t.Error("an error finding must carry the cause")
	}
}

// --- applying what policy allows ----------------------------------------
//
// These drive the whole application with the acting half wired in, so what is
// asserted is the rule -- "duva applies what it may, and stops waiting for it"
// -- rather than the transaction's internals, which transaction_test covers.

// appApply is app() with an actor, so the run applies rather than only
// reporting. docker and git are faked; everything else is real.
func appApply(t *testing.T, project string, reg fixture.RegistryAnswers, r *recorder) ([]watch.Finding, *watch.State) {
	t.Helper()
	a := newApp(t, project)
	composeDir, stateFile = filepath.Dir(a.project), a.state

	st, err := watch.LoadState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	act := func(f watch.Finding) Result {
		return apply(f, r.docker, r.git, applyOptions{Host: "testhost"})
	}
	findings, err := checkWith(envConfig{}, testLog(), watch.Registry{
		ListMatchingTags: reg.ListMatchingTags,
		RemoteDigest:     reg.RemoteDigest,
		TagCreated:       reg.TagCreated,
	}, st, time.Now(), act, r.docker, r.git)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(a.state); err != nil {
		t.Fatal(err)
	}
	return findings, st
}

// An update policy allows is applied, and then it is not waiting for anyone.
func TestApp_AutoApplicableUpdateIsApplied(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")
	r := newRecorder()

	findings, st := appApply(t, f.Project(svc), f.Registry(svc), r)

	if !r.did("up") {
		t.Errorf("the container should have been recreated: %v", r.calls)
	}
	if !r.did("commit") {
		t.Errorf("the change should have been committed: %v", r.calls)
	}
	got := findingFor(t, findings, svc.Name)
	if got.Available() {
		t.Errorf("an applied update is no longer available: %+v", got)
	}
	if len(st.Pending) != 0 {
		t.Errorf("nothing should be queued: %+v", st.Pending)
	}
}

// An update needing approval must not be applied behind the human's back.
func TestApp_ApprovalNeededIsNotApplied(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpMajor), "patch")
	r := newRecorder()

	_, st := appApply(t, f.Project(svc), f.Registry(svc), r)

	if r.did("pull") || r.did("up") {
		t.Errorf("nothing should have been applied: %v", r.calls)
	}
	if _, queued := st.Pending[svc.Name]; !queued {
		t.Error("it should still be waiting for a human")
	}
}

// A failed apply leaves the service reported as an error rather than quietly
// looking up to date, so the next run tries again.
func TestApp_FailedApplyIsReportedAsAnError(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")
	r := newRecorder()
	r.docker.ComposeUp = func(string, string) error { return errBoundary }

	findings, _ := appApply(t, f.Project(svc), f.Registry(svc), r)

	got := findingFor(t, findings, svc.Name)
	if got.Status != watch.StatusError {
		t.Errorf("Status = %q, want error (%s)", got.Status, got.Reason)
	}
	if !strings.Contains(got.Reason, string(StepRecreate)) {
		t.Errorf("the reason should name the step that failed: %q", got.Reason)
	}
}

// A moving tag whose apply fails must still be offered again on the next
// check -- the compose file's own pin is what "up to date" means, and it was
// never rewritten (the transaction failed before writePin), so the file
// still disagrees with the registry and is reported regardless of what
// baseline holds. baseline itself just tracks what was last seen from the
// registry, on every check, whether or not an apply that run attempted
// succeeded.
func TestApp_FailedApplyIsStillOfferedNextCheck(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.MovingTagService(false), "patch")
	a := newApp(t, f.Project(svc))

	// First run records where the tag points.
	composeDir, stateFile = filepath.Dir(a.project), a.state
	st, err := watch.LoadState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := check(envConfig{}, testLog(), watch.Registry{
		RemoteDigest: f.Registry(svc).RemoteDigest,
	}, st, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The tag moves, and the apply fails.
	moved := svc
	moved.AvailableDigest = f.Digest()
	r := newRecorder()
	r.docker.ComposeUp = func(string, string) error { return errBoundary }

	if _, err := checkWith(envConfig{}, testLog(), watch.Registry{
		RemoteDigest: f.Registry(moved).RemoteDigest,
	}, st, time.Now(), func(fd watch.Finding) Result {
		return apply(fd, r.docker, r.git, applyOptions{Host: "h"})
	}, r.docker, r.git); err != nil {
		t.Fatal(err)
	}
	if st.Baseline[svc.Name] != moved.AvailableDigest {
		t.Errorf("baseline should track what was last seen, got %q", st.Baseline[svc.Name])
	}

	// A later check against the same unmoved (failed) target must still
	// report it: the file never changed, so it still disagrees with the
	// registry.
	findings, err := checkWith(envConfig{}, testLog(), watch.Registry{
		RemoteDigest: f.Registry(moved).RemoteDigest,
	}, st, time.Now(), nil, Docker{}, Git{})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, findings, svc.Name); !got.Available() {
		t.Errorf("a moved tag whose apply failed must still be offered, got %+v", got)
	}
}

// --- approving from the page -------------------------------------------
//
// The UI tests use a fake Applier, so these cover duva's own: that approving
// runs the real transaction, and that the queue and state reflect the result.

// storeWith builds a store holding one queued update, as the page would see
// after a check found something needing approval.
func storeWith(t *testing.T, f *fixture.Fixture, r *recorder) (*store, fixture.Service) {
	t.Helper()
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpMajor), "patch")
	file := f.Project(svc)

	a := newApp(t, file)
	st, err := watch.LoadState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	st.Pending[svc.Name] = watch.Pending{
		Service:    svc.Name,
		File:       file,
		Image:      svc.Image,
		CurrentTag: svc.Tag,
		Kind:       watch.KindTag,
		Candidate:  svc.AvailableTags[len(svc.AvailableTags)-1],
		Why:        "major exceeds duva.auto: patch",
	}
	st.Notified[svc.Name] = st.Pending[svc.Name].Candidate

	return &store{state: st, act: func(fd watch.Finding, extra func(string, ...any)) Result {
		return apply(fd, r.docker, r.git, applyOptions{Host: "testhost", Log: extra})
	}}, svc
}

// applySync starts an update and waits for it to finish, returning the same
// (message, error) shape the old synchronous Apply did -- so tests written
// against that shape keep asserting the same thing. Safe to poll tightly:
// everything under it is fakes, so "still running" never takes long enough
// for the sleep to matter.
func applySync(t *testing.T, s *store, service string) (string, error) {
	t.Helper()
	if err := s.Start(service); err != nil {
		return "", err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := s.Progress(service); ok && p.Done {
			if p.Failed {
				return "", fmt.Errorf("%s", p.Message)
			}
			return p.Message, nil
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s: Start did not finish within the test deadline", service)
	return "", nil
}

// Approving applies the update and the row stops waiting -- it reached the
// container, so there is nothing left to approve.
func TestStoreApply_AppliesAndClearsTheRow(t *testing.T) {
	f := fixture.New(t)
	r := newRecorder()
	s, svc := storeWith(t, f, r)

	msg, err := applySync(t, s, svc.Name)
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if msg == "" {
		t.Error("the result should say what happened")
	}
	if !r.did("up") || !r.did("commit") {
		t.Errorf("the transaction should have run: %v", r.calls)
	}
	if _, still := s.state.Pending[svc.Name]; still {
		t.Error("an applied update is no longer waiting")
	}
	if _, still := s.state.Notified[svc.Name]; still {
		t.Error("the notification record should be cleared too")
	}
}

// A failure leaves the row in place: it still needs a human, and the next
// visit to the page should still offer it.
func TestStoreApply_FailureKeepsTheRow(t *testing.T) {
	f := fixture.New(t)
	r := newRecorder()
	r.docker.ComposeUp = func(string, string) error { return errBoundary }
	s, svc := storeWith(t, f, r)

	if _, err := applySync(t, s, svc.Name); err == nil {
		t.Fatal("expected a failure")
	}
	if _, still := s.state.Pending[svc.Name]; !still {
		t.Error("a failed update is still waiting for a human")
	}
}

func TestStoreApply_UnknownService(t *testing.T) {
	f := fixture.New(t)
	s, _ := storeWith(t, f, newRecorder())

	if _, err := applySync(t, s, "nosuchservice"); err == nil {
		t.Error("approving something that is not queued must be an error")
	}
}

// Approving a moving-tag update advances the baseline, since the container
// now runs that digest.
func TestStoreApply_MovingTagAdvancesTheBaseline(t *testing.T) {
	f := fixture.New(t)
	svc := f.MovingTagService(true)
	file := f.Project(svc)
	a := newApp(t, file)
	st, err := watch.LoadState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	st.Baseline[svc.Name] = svc.Digest
	st.Pending[svc.Name] = watch.Pending{
		Service: svc.Name, File: file, Image: svc.Image,
		CurrentTag: svc.Tag, Kind: watch.KindDigest, Candidate: svc.AvailableDigest,
	}

	r := newRecorder()
	s := &store{state: st, act: func(fd watch.Finding, extra func(string, ...any)) Result {
		return apply(fd, r.docker, r.git, applyOptions{Host: "h"})
	}}

	if _, err := applySync(t, s, svc.Name); err != nil {
		t.Fatal(err)
	}
	if st.Baseline[svc.Name] != svc.AvailableDigest {
		t.Error("the baseline should advance once the container runs the new digest")
	}
}

// --- notifications ------------------------------------------------------

// notifyingConfig points duva at a receiver that records what it was sent.
func notifyingConfig(t *testing.T) (envConfig, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Header.Get("Title")+"|"+string(body))
	}))
	t.Cleanup(srv.Close)
	return envConfig{NtfyURL: srv.URL, NtfyTopic: "test", Hostname: "testhost"}, &got
}

// An applied update is announced, naming what it moved between.
func TestNotify_AppliedUpdate(t *testing.T) {
	f := fixture.New(t)
	cfg, got := notifyingConfig(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")
	r := newRecorder()

	a := newApp(t, f.Project(svc))
	composeDir, stateFile = filepath.Dir(a.project), a.state
	st, _ := watch.LoadState(a.state)
	if _, err := checkWith(cfg, testLog(), watch.Registry{
		ListMatchingTags: f.Registry(svc).ListMatchingTags,
		TagCreated:       f.Registry(svc).TagCreated,
	}, st, time.Now(), func(fd watch.Finding) Result {
		return apply(fd, r.docker, r.git, applyOptions{Host: "testhost"})
	}, r.docker, r.git); err != nil {
		t.Fatal(err)
	}

	if len(*got) != 1 {
		t.Fatalf("expected one notification, got %d: %v", len(*got), *got)
	}
	// The service and the version it moved to: what a reader acts on. The
	// verb around them is free to change.
	if !strings.Contains((*got)[0], svc.Name) {
		t.Errorf("it should name the service: %q", (*got)[0])
	}
	if want := svc.AvailableTags[len(svc.AvailableTags)-1]; !strings.Contains((*got)[0], want) {
		t.Errorf("it should name the version it updated to (%s): %q", want, (*got)[0])
	}
}

// A failed update is announced too, and says which step failed -- "compose up
// failed" and "push failed" call for different responses.
func TestNotify_FailedUpdateNamesTheStep(t *testing.T) {
	f := fixture.New(t)
	cfg, got := notifyingConfig(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")
	r := newRecorder()
	r.docker.ComposeUp = func(string, string) error { return errBoundary }

	a := newApp(t, f.Project(svc))
	composeDir, stateFile = filepath.Dir(a.project), a.state
	before := imageLine(t, a.project, svc.Name)
	st, _ := watch.LoadState(a.state)
	if _, err := checkWith(cfg, testLog(), watch.Registry{
		ListMatchingTags: f.Registry(svc).ListMatchingTags,
		TagCreated:       f.Registry(svc).TagCreated,
	}, st, time.Now(), func(fd watch.Finding) Result {
		return apply(fd, r.docker, r.git, applyOptions{Host: "testhost"})
	}, r.docker, r.git); err != nil {
		t.Fatal(err)
	}

	if len(*got) != 1 {
		t.Fatalf("expected one notification, got %d: %v", len(*got), *got)
	}
	// The step is a typed constant, so naming it is a contract rather than
	// wording: "compose up failed" and "push failed" call for different
	// responses, and a notification that does not distinguish them is no use.
	if msg := (*got)[0]; !strings.Contains(msg, string(StepRecreate)) {
		t.Errorf("it should name the step that failed: %q", msg)
	}

	// That the file was put back is asserted on the file, not on the sentence
	// describing it: the compose file must not keep claiming an image the
	// container refused.
	if got := imageLine(t, a.project, svc.Name); got != before {
		t.Errorf("the compose file was not put back:\n  was %s\n  now %s", before, got)
	}
}

// Taking a soaking update early is the one place a human overrides a safety
// control, so it must leave nothing behind: a row that survived being applied
// would go on offering an update that has already happened.
func TestStoreApply_TakesASoakingUpdateEarly(t *testing.T) {
	f := fixture.New(t)
	r := newRecorder()
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpMinor), "patch")
	file := f.Project(svc)

	a := newApp(t, file)
	st, err := watch.LoadState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	candidate := svc.AvailableTags[len(svc.AvailableTags)-1]
	st.Soaking[svc.Name] = watch.Soaking{
		Service: svc.Name, File: file, Image: svc.Image,
		CurrentTag: svc.Tag, Candidate: candidate,
		Remaining: "4 days", Outcome: "moves to approval",
	}
	s := &store{state: st, act: func(fd watch.Finding, extra func(string, ...any)) Result {
		return apply(fd, r.docker, r.git, applyOptions{Host: "testhost"})
	}}

	msg, err := applySync(t, s, svc.Name)
	if err != nil {
		t.Fatalf("a soaking update should be applyable: %v", err)
	}
	if len(st.Soaking) != 0 {
		t.Errorf("the soaking row should be gone, still holds %v", st.Soaking)
	}
	// The record should show the operator chose to override the wait, not that
	// duva decided the release was old enough.
	if !strings.Contains(msg, "soak") {
		t.Errorf("the result should say the soak was overridden: %q", msg)
	}
	if !r.did("up") {
		t.Errorf("the container should have been recreated: %v", r.calls)
	}
}

// The other half of the soak: a candidate old enough is adopted, and nothing
// is left recorded as waiting.
//
// Without this, a soak that withheld unconditionally -- a swapped comparison,
// or a delay parsed as the wrong magnitude -- would pass every other test.
// Services with a delay would quietly never update, which is exactly the kind
// of failure that is discovered months later.
func TestApp_AgedCandidateIsReleasedBySoak(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")
	svc.Labels["duva.delay"] = "7d"
	svc.Published = time.Now().Add(-30 * 24 * time.Hour)

	findings, st := app(t, f.Project(svc), f.Registry(svc))

	got := findingFor(t, findings, svc.Name)
	if !got.Available() {
		t.Fatalf("a 30-day-old tag should satisfy a 7d soak: %+v", got)
	}
	if !got.AutoApplies() {
		t.Errorf("duva.auto: patch covers this patch, so it should apply: %s", got.Why)
	}
	// Released, so it is no longer something duva is waiting on.
	if len(st.Soaking) != 0 {
		t.Errorf("an adopted candidate should not still be soaking: %v", st.Soaking)
	}
}

// A soak holds the candidate back but says so: the update is recorded as
// waiting on time, not discarded. Duva reporting nothing would look like
// nothing was found.
func TestApp_WithheldCandidateIsRecordedAsSoaking(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")
	svc.Labels["duva.delay"] = "7d"
	svc.Published = time.Now().Add(-2 * time.Hour)

	_, st := app(t, f.Project(svc), f.Registry(svc))

	row, ok := st.Soaking[svc.Name]
	if !ok {
		t.Fatalf("a withheld candidate should be recorded, state holds %v", st.Soaking)
	}
	if row.Candidate != svc.AvailableTags[len(svc.AvailableTags)-1] {
		t.Errorf("the soaking row names the wrong candidate: %+v", row)
	}
	if row.Remaining == "" || row.Outcome == "" {
		t.Errorf("a soaking row should say how long and what happens then: %+v", row)
	}
}

// A busy repository defers rather than fails, and the finding must stay
// available so the next run tries again.
//
// The branch that decides this is discriminated by a note with no outcome. If
// that misfired the update would fall through to "already at this digest" --
// silently forgotten, dropped from the queue, and the service would stop
// updating while reporting itself healthy.
func TestApp_BusyRepositoryLeavesTheUpdateOutstanding(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")
	file := f.Project(svc)
	before := imageLine(t, file, svc.Name)

	r := newRecorder()
	r.git.IsClean = func(string) (bool, error) { return false, ErrRepoBusy }

	a := newApp(t, file)
	composeDir, stateFile = filepath.Dir(a.project), a.state
	st, _ := watch.LoadState(a.state)
	findings, err := checkWith(loadEnvConfig(), testLog(), watch.Registry{
		ListMatchingTags: f.Registry(svc).ListMatchingTags,
		TagCreated:       f.Registry(svc).TagCreated,
	}, st, time.Now(), func(fd watch.Finding) Result {
		return apply(fd, r.docker, r.git, applyOptions{Host: "testhost"})
	}, r.docker, r.git)
	if err != nil {
		t.Fatal(err)
	}

	if got := findingFor(t, findings, svc.Name); !got.Available() {
		t.Errorf("a deferred update should still be available next run: %+v", got)
	}
	if got := imageLine(t, a.project, svc.Name); got != before {
		t.Errorf("nothing should have been written:\n  was %s\n  now %s", before, got)
	}
	if r.did("up") || r.did("commit") {
		t.Errorf("nothing should have been attempted: %v", r.calls)
	}
}
