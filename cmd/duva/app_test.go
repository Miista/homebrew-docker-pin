package main

import (
	"path/filepath"
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
