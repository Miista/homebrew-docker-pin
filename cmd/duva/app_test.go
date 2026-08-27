package main

import (
	"path/filepath"
	"strings"
	"testing"
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
	findings, err := check(envConfig{}, watch.Registry{
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
		svc.Labels["duva.include"] = `^\d+\.\d+\.\d+$`
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

// Remove duva.include: the service stops being a constrained one and becomes
// a moving-tag follower, which is a different question entirely. Its version
// tag never moves, so nothing is ever found.
func TestApp_RemovingTheIncludeLabel_ChangesWhatIsAsked(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.WithUpdate(f.PinnedService(), fixture.BumpPatch), "patch")

	delete(svc.Labels, "duva.include") // the one break
	svc.AvailableDigest = svc.Digest   // a moving-tag lookup now answers

	findings, _ := app(t, f.Project(svc), f.Registry(svc))
	got := findingFor(t, findings, svc.Name)
	if got.Kind == watch.KindTag {
		t.Errorf("without duva.include this is no longer a tag question: %+v", got)
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

	svc.Labels["duva.includ"] = svc.Labels["duva.include"] // the one break
	delete(svc.Labels, "duva.include")

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

	svc.Labels["duva.include"] = "^(" // the one break

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
	findings, err := checkWith(envConfig{}, watch.Registry{
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

// A moving tag's baseline advances only when the update is applied. Advancing
// it on detection would mean a move that could not be applied is forgotten,
// and the service silently stops being offered the update it needs.
func TestApp_BaselineAdvancesOnlyWhenApplied(t *testing.T) {
	f := fixture.New(t)
	svc := f.WithAuto(f.MovingTagService(false), "patch")
	a := newApp(t, f.Project(svc))

	// First run records where the tag points.
	composeDir, stateFile = filepath.Dir(a.project), a.state
	st, err := watch.LoadState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := check(envConfig{}, watch.Registry{
		RemoteDigest: f.Registry(svc).RemoteDigest,
	}, st, time.Now()); err != nil {
		t.Fatal(err)
	}
	baseline := st.Baseline[svc.Name]

	// The tag moves, and the apply fails.
	moved := svc
	moved.AvailableDigest = f.Digest()
	r := newRecorder()
	r.docker.ComposeUp = func(string, string) error { return errBoundary }

	if _, err := checkWith(envConfig{}, watch.Registry{
		RemoteDigest: f.Registry(moved).RemoteDigest,
	}, st, time.Now(), func(fd watch.Finding) Result {
		return apply(fd, r.docker, r.git, applyOptions{Host: "h"})
	}, r.docker, r.git); err != nil {
		t.Fatal(err)
	}

	if st.Baseline[svc.Name] != baseline {
		t.Error("a failed apply must not advance the baseline, or the move is forgotten")
	}
}
