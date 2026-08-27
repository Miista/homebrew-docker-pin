package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/registry"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

// setupFixture creates a temp dir with a compose file and points the
// composeDir / stateFile package variables at fixtures (in the container
// these are the fixed /compose and /data/duva.json mounts).
func setupFixture(t *testing.T, composeContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeContent), 0o644); err != nil {
		t.Fatal(err)
	}
	origCompose, origState := composeDir, stateFile
	t.Cleanup(func() { composeDir, stateFile = origCompose, origState })
	composeDir = dir
	stateFile = filepath.Join(dir, "state.json")
	return dir
}

const pinnedConstrainedService = `services:
  app:
    image: example.com/app:1.2.0@sha256:aaa
    labels:
      duva.include: '^\d+\.\d+\.\d+$'
`

const pinnedUnconstrainedService = `services:
  app:
    image: example.com/app:latest@sha256:aaa
`

const unpinnedService = `services:
  app:
    image: example.com/app:1.2.0
`

const builtService = `services:
  app:
    build: ./app
    image: app:local
`

func tagReg(tags []string) watch.Registry {
	return watch.Registry{
		ListMatchingTags: func(string, *regexp.Regexp, *regexp.Regexp, string) ([]string, error) {
			return tags, nil
		},
	}
}

func digestReg(digest string) watch.Registry {
	return watch.Registry{
		RemoteDigest: func(string, string) (string, error) { return digest, nil },
	}
}

// only returns the single finding for the fixture's one service.
func only(t *testing.T, findings []watch.Finding) watch.Finding {
	t.Helper()
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	return findings[0]
}

func check1(t *testing.T, reg watch.Registry, st *watch.State) watch.Finding {
	t.Helper()
	findings, err := check(envConfig{}, reg, st, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return only(t, findings)
}

// --- detection ---

func TestConstrained_FindsNewerTag(t *testing.T) {
	setupFixture(t, pinnedConstrainedService)
	f := check1(t, tagReg([]string{"1.2.0", "1.3.0"}), watch.NewState())

	if !f.Available() || f.Candidate != "1.3.0" || f.Kind != watch.KindTag {
		t.Fatalf("expected 1.3.0 available as a tag, got %+v", f)
	}
}

func TestConstrained_NoNewerTag(t *testing.T) {
	setupFixture(t, pinnedConstrainedService)
	f := check1(t, tagReg([]string{"1.2.0"}), watch.NewState())

	if f.Available() {
		t.Fatalf("nothing newer exists, got %+v", f)
	}
}

// The first sight of a moving-tag service records where the tag points and
// says nothing: otherwise every newly watched service reports an "update" on
// day one regardless of whether anything moved.
func TestMovingTag_FirstCheckRecordsBaselineSilently(t *testing.T) {
	setupFixture(t, pinnedUnconstrainedService)
	st := watch.NewState()

	f := check1(t, digestReg("sha256:current"), st)
	if f.Available() {
		t.Fatalf("first check must not report, got %+v", f)
	}
	if st.Baseline["app"] != "sha256:current" {
		t.Fatalf("baseline = %q, want sha256:current", st.Baseline["app"])
	}
	if len(st.Pending) != 0 {
		t.Fatalf("nothing should be pending, got %+v", st.Pending)
	}
}

func TestMovingTag_DigestMoveIsReported(t *testing.T) {
	setupFixture(t, pinnedUnconstrainedService)
	st := watch.NewState()
	st.Baseline["app"] = "sha256:old"

	f := check1(t, digestReg("sha256:new"), st)
	if !f.Available() || f.Kind != watch.KindDigest || f.Candidate != "sha256:new" {
		t.Fatalf("expected a digest move, got %+v", f)
	}
}

func TestMovingTag_SameDigestIsNotReported(t *testing.T) {
	setupFixture(t, pinnedUnconstrainedService)
	st := watch.NewState()
	st.Baseline["app"] = "sha256:same"

	if f := check1(t, digestReg("sha256:same"), st); f.Available() {
		t.Fatalf("unchanged digest must not report, got %+v", f)
	}
}

// Pin status is the opt-in: an unpinned service has made no versioning
// decision to watch over.
func TestSkips_UnpinnedService(t *testing.T) {
	setupFixture(t, unpinnedService)
	f := check1(t, tagReg([]string{"9.9.9"}), watch.NewState())

	if f.Status != watch.StatusSkipped || f.Reason != "not pinned" {
		t.Fatalf("expected skipped/not pinned, got %+v", f)
	}
}

// A locally built image's digest is local to one daemon, so there is nothing
// a registry could tell us about it.
func TestSkips_LocallyBuiltService(t *testing.T) {
	setupFixture(t, builtService)
	f := check1(t, tagReg([]string{"9.9.9"}), watch.NewState())

	if f.Status != watch.StatusSkipped {
		t.Fatalf("expected a built service to be skipped, got %+v", f)
	}
}

// --- pending queue ---

func TestPending_AddedThenClearedWhenGone(t *testing.T) {
	setupFixture(t, pinnedConstrainedService)
	st := watch.NewState()

	check1(t, tagReg([]string{"1.2.0", "1.3.0"}), st)
	if len(st.Pending) != 1 {
		t.Fatalf("expected 1 pending, got %+v", st.Pending)
	}
	p := st.Pending["app"]
	if p.Candidate != "1.3.0" || p.CurrentTag != "1.2.0" || p.Kind != watch.KindTag {
		t.Fatalf("pending row wrong: %+v", p)
	}

	// Registry now offers nothing newer (e.g. the pin was upgraded): the row
	// must disappear, or the queue accumulates updates that no longer exist.
	check1(t, tagReg([]string{"1.2.0"}), st)
	if len(st.Pending) != 0 {
		t.Fatalf("pending should be cleared, got %+v", st.Pending)
	}
}

// FirstSeen answers "how long has this been waiting", so it must not reset
// every run while the same candidate is outstanding.
func TestPending_FirstSeenIsStableAcrossRuns(t *testing.T) {
	setupFixture(t, pinnedConstrainedService)
	st := watch.NewState()
	reg := tagReg([]string{"1.2.0", "1.3.0"})

	if _, err := check(envConfig{}, reg, st, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	first := st.Pending["app"].FirstSeen

	if _, err := check(envConfig{}, reg, st, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := st.Pending["app"].FirstSeen; got != first {
		t.Errorf("FirstSeen moved from %q to %q for the same candidate", first, got)
	}
}

func TestPending_NewCandidateResetsFirstSeen(t *testing.T) {
	setupFixture(t, pinnedConstrainedService)
	st := watch.NewState()

	if _, err := check(envConfig{}, tagReg([]string{"1.2.0", "1.3.0"}), st,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	first := st.Pending["app"].FirstSeen

	if _, err := check(envConfig{}, tagReg([]string{"1.2.0", "1.3.0", "1.4.0"}), st,
		time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	p := st.Pending["app"]
	if p.Candidate != "1.4.0" {
		t.Fatalf("candidate = %q, want 1.4.0", p.Candidate)
	}
	if p.FirstSeen == first {
		t.Error("a different candidate should carry its own FirstSeen")
	}
}

// --- notification dedupe ---

func TestNotified_RecordedOncePerCandidate(t *testing.T) {
	setupFixture(t, pinnedConstrainedService)
	st := watch.NewState()
	reg := tagReg([]string{"1.2.0", "1.3.0"})

	check1(t, reg, st)
	if st.Notified["app"] != "1.3.0" {
		t.Fatalf("Notified = %q, want 1.3.0", st.Notified["app"])
	}

	// Same candidate again: still recorded, so no second notification fires.
	check1(t, reg, st)
	if st.Notified["app"] != "1.3.0" {
		t.Fatalf("Notified = %q, want 1.3.0", st.Notified["app"])
	}

	// Nothing outstanding: forget it, so the same tag reappearing later is
	// announced again rather than silently swallowed.
	check1(t, tagReg([]string{"1.2.0"}), st)
	if _, ok := st.Notified["app"]; ok {
		t.Error("Notified should be cleared once nothing is outstanding")
	}
}

// --- state persistence ---

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st := watch.NewState()
	st.Baseline["a"] = "sha256:x"
	st.Notified["b"] = "1.2.3"
	st.Pending["c"] = watch.Pending{Service: "c", Candidate: "2.0.0", Kind: watch.KindTag}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}

	got, err := watch.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Baseline["a"] != "sha256:x" || got.Notified["b"] != "1.2.3" {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.Pending["c"].Candidate != "2.0.0" {
		t.Errorf("pending lost: %+v", got.Pending)
	}
}

func TestLoadState_MissingFileIsEmpty(t *testing.T) {
	st, err := watch.LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("a missing state file is not an error: %v", err)
	}
	if len(st.Baseline) != 0 || len(st.Pending) != 0 {
		t.Errorf("expected empty state, got %+v", st)
	}
}

// --- reporting ---

func TestReport(t *testing.T) {
	var buf bytes.Buffer
	report(&buf, []watch.Finding{
		{Service: "a", Status: watch.StatusAvailable, Candidate: "1.3.0"},
		{Service: "b", Status: watch.StatusSkipped, Reason: "not pinned"},
		{Service: "c", Status: watch.StatusUpToDate},
		{Service: "d", Status: watch.StatusError, Reason: "boom"},
	})
	for _, want := range []string{
		"a: 1.3.0 available",
		"b: not pinned, skipping",
		"c: up to date",
		"d: error: boom",
	} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("missing %q in:\n%s", want, buf.String())
		}
	}
}

// --- classification reaches the finding and the queue ---

func TestClassification_TagCandidatesCarryABump(t *testing.T) {
	setupFixture(t, pinnedConstrainedService) // pinned at 1.2.0
	for _, tc := range []struct {
		newest string
		want   registry.Kind
	}{
		{"1.2.1", registry.KindPatch},
		{"1.3.0", registry.KindMinor},
		{"2.0.0", registry.KindMajor},
	} {
		st := watch.NewState()
		f := check1(t, tagReg([]string{"1.2.0", tc.newest}), st)
		if f.Bump != tc.want {
			t.Errorf("1.2.0 -> %s: Bump = %q, want %q", tc.newest, f.Bump, tc.want)
		}
		if st.Pending["app"].Bump != tc.want {
			t.Errorf("1.2.0 -> %s: pending Bump = %q, want %q", tc.newest, st.Pending["app"].Bump, tc.want)
		}
	}
}

// A moving tag has no version pair, so it carries no bump at all. That is a
// different thing from a pair that could not be classified.
func TestClassification_DigestCandidatesHaveNoBump(t *testing.T) {
	setupFixture(t, pinnedUnconstrainedService)
	st := watch.NewState()
	st.Baseline["app"] = "sha256:old"

	f := check1(t, digestReg("sha256:new"), st)
	if f.Bump != "" {
		t.Errorf("Bump = %q, want empty for a digest candidate", f.Bump)
	}
}

// --- policy: what reaches the queue ---

// A project where policy must reach a different verdict per service, all in
// one pass: the interesting bug is one service's rules bleeding into another.
const mixedPolicyProject = `services:
  autopatch:
    image: example.com/a:1.0.0@sha256:aaa
    labels:
      duva.include: '^\d+\.\d+\.\d+$'
      duva.auto: patch
  strict:
    image: example.com/b:1.0.0@sha256:bbb
    labels:
      duva.include: '^\d+\.\d+\.\d+$'
      duva.auto: none
  permissive:
    image: example.com/c:1.0.0@sha256:ccc
    labels:
      duva.include: '^\d+\.\d+\.\d+$'
      duva.auto: major
`

func TestPolicy_QueueHoldsOnlyWhatNeedsAHuman(t *testing.T) {
	setupFixture(t, mixedPolicyProject)
	st := watch.NewState()

	// Every service is offered the same patch bump: 1.0.0 -> 1.0.1.
	findings, err := check(envConfig{}, tagReg([]string{"1.0.0", "1.0.1"}), st, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(findings))
	}

	byName := map[string]watch.Finding{}
	for _, f := range findings {
		byName[f.Service] = f
	}
	if !byName["autopatch"].AutoApplies() {
		t.Errorf("autopatch: a patch within duva.auto: patch should apply, got %+v", byName["autopatch"])
	}
	if !byName["permissive"].AutoApplies() {
		t.Errorf("permissive: duva.auto: major covers a patch, got %+v", byName["permissive"])
	}
	if !byName["strict"].NeedsApproval() {
		t.Errorf("strict: duva.auto: none must never apply, got %+v", byName["strict"])
	}

	// Only the one needing a human is queued. An update policy allows is not
	// "waiting" for anything.
	if len(st.Pending) != 1 {
		t.Fatalf("expected only strict pending, got %+v", st.Pending)
	}
	if _, ok := st.Pending["strict"]; !ok {
		t.Errorf("wrong service queued: %+v", st.Pending)
	}
}

// A bump that exceeds the threshold must fall back to approval, not silently
// apply because the same service is auto for smaller changes.
func TestPolicy_ExceedingTheThresholdQueues(t *testing.T) {
	setupFixture(t, mixedPolicyProject)
	st := watch.NewState()

	// 1.0.0 -> 2.0.0 is major: beyond patch, beyond none, within major.
	if _, err := check(envConfig{}, tagReg([]string{"1.0.0", "2.0.0"}), st, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"autopatch", "strict"} {
		if _, ok := st.Pending[want]; !ok {
			t.Errorf("%s should be queued for a major, got %+v", want, st.Pending)
		}
	}
	if _, ok := st.Pending["permissive"]; ok {
		t.Error("permissive has duva.auto: major and should not be queued")
	}
}

// The pending row must carry the explanation, so the UI and a notification
// can both say why without recomputing policy.
func TestPolicy_PendingCarriesWhy(t *testing.T) {
	setupFixture(t, mixedPolicyProject)
	st := watch.NewState()
	if _, err := check(envConfig{}, tagReg([]string{"1.0.0", "1.0.1"}), st, time.Now()); err != nil {
		t.Fatal(err)
	}
	if why := st.Pending["strict"].Why; why == "" {
		t.Error("pending row carries no explanation")
	}
}

// A bad label is that service's problem alone: the rest of the project must
// still be checked. One typo should not blind duva to everything else.
func TestPolicy_BadLabelDoesNotStopOtherServices(t *testing.T) {
	setupFixture(t, `services:
  broken:
    image: example.com/a:1.0.0@sha256:aaa
    labels:
      duva.auto: nonsense
  fine:
    image: example.com/b:1.0.0@sha256:bbb
    labels:
      duva.include: '^\d+\.\d+\.\d+$'
      duva.auto: none
`)
	st := watch.NewState()
	findings, err := check(envConfig{}, tagReg([]string{"1.0.0", "1.0.1"}), st, time.Now())
	if err != nil {
		t.Fatalf("one bad service must not fail the run: %v", err)
	}

	byName := map[string]watch.Finding{}
	for _, f := range findings {
		byName[f.Service] = f
	}
	if byName["broken"].Status != watch.StatusError {
		t.Errorf("broken should be an error finding, got %+v", byName["broken"])
	}
	if !byName["fine"].NeedsApproval() {
		t.Errorf("fine should still have been checked, got %+v", byName["fine"])
	}
}
