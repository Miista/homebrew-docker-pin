package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/version"
)

func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "duva.json")
}

func TestState_RoundTrip(t *testing.T) {
	p := statePath(t)
	st := NewState()
	st.Baseline["a"] = "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	st.Notified["b"] = "1.2.3"
	st.Pending["c"] = Pending{
		Service: "c", Image: "x/y", CurrentTag: "1.0.0",
		Kind: KindTag, Candidate: "2.0.0", Bump: version.KindMajor,
		Why: "major exceeds duva.auto: patch", FirstSeen: "2026-01-01T00:00:00Z",
	}
	if err := st.Save(p); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Baseline["a"] != "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" || got.Notified["b"] != "1.2.3" {
		t.Errorf("round trip lost data: %+v", got)
	}
	p2 := got.Pending["c"]
	if p2.Candidate != "2.0.0" || p2.Bump != version.KindMajor || p2.Why == "" || p2.FirstSeen == "" {
		t.Errorf("pending row lost fields: %+v", p2)
	}
}

func TestLoadState_MissingFileIsEmptyNotAnError(t *testing.T) {
	st, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("a first run has no state file; that is not an error: %v", err)
	}
	if st.Baseline == nil || st.Notified == nil || st.Pending == nil {
		t.Error("maps must be usable without a nil check")
	}
}

func TestLoadState_Corrupt(t *testing.T) {
	p := statePath(t)
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p); err == nil {
		t.Error("a corrupt state file must be reported, not silently discarded")
	}
}

// A file written by an older version, or hand-edited, can be missing whole
// sections. Callers should not have to nil-check every map.
func TestLoadState_PartialFileGetsUsableMaps(t *testing.T) {
	p := statePath(t)
	if err := os.WriteFile(p, []byte(`{"baseline":{"a":"sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	st.Notified["b"] = "1.0.0"  // must not panic
	st.Pending["c"] = Pending{} // must not panic
	if st.Baseline["a"] != "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
		t.Error("existing section lost")
	}
}

// The state file is rewritten on every run; a crash mid-write must not leave
// a truncated file that fails to parse on the next start.
func TestSave_IsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "duva.json")
	st := NewState()
	st.Baseline["a"] = "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	for i := 0; i < 3; i++ {
		if err := st.Save(p); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "duva.json" {
			t.Errorf("leftover file %q: the temp file was not renamed away", e.Name())
		}
	}

	var parsed State
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("saved file does not parse: %v", err)
	}
}

func TestSave_CreatesParentDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "deeper", "duva.json")
	if err := NewState().Save(p); err != nil {
		t.Fatalf("Save should create its directory: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Error(err)
	}
}

// --- Reconcile ----------------------------------------------------------

func available(service, candidate string) Finding {
	return Finding{
		Service: service, Status: StatusAvailable, Kind: KindTag,
		Candidate: candidate, Decision: DecideApprove, Why: "needs a human",
	}
}

func upToDate(service string) Finding {
	return Finding{Service: service, Status: StatusUpToDate}
}

func TestReconcile_AddsAndDrops(t *testing.T) {
	st := NewState()
	seen := []Finding{available("a", "2.0.0"), upToDate("b")}
	st.Reconcile([]Finding{seen[0]}, seen, "T1")

	if len(st.Pending) != 1 || st.Pending["a"].Candidate != "2.0.0" {
		t.Fatalf("expected a queued, got %+v", st.Pending)
	}

	// a is no longer waiting (applied, or superseded by the pin moving).
	st.Reconcile(nil, []Finding{upToDate("a"), upToDate("b")}, "T2")
	if len(st.Pending) != 0 {
		t.Errorf("queue should be empty, got %+v", st.Pending)
	}
}

// A stale row invites approving something that no longer exists, so a
// superseded candidate must replace the old one rather than accumulate.
func TestReconcile_SupersededCandidateReplacesTheRow(t *testing.T) {
	st := NewState()
	st.Reconcile([]Finding{available("a", "2.0.0")}, []Finding{available("a", "2.0.0")}, "T1")
	st.Reconcile([]Finding{available("a", "3.0.0")}, []Finding{available("a", "3.0.0")}, "T2")

	if len(st.Pending) != 1 {
		t.Fatalf("expected one row, got %+v", st.Pending)
	}
	if got := st.Pending["a"]; got.Candidate != "3.0.0" {
		t.Errorf("Candidate = %q, want the newer one", got.Candidate)
	}
	if st.Pending["a"].FirstSeen != "T2" {
		t.Errorf("a different candidate carries its own FirstSeen, got %q", st.Pending["a"].FirstSeen)
	}
}

func TestReconcile_FirstSeenSurvivesWhileTheCandidateDoes(t *testing.T) {
	st := NewState()
	f := available("a", "2.0.0")
	st.Reconcile([]Finding{f}, []Finding{f}, "T1")
	st.Reconcile([]Finding{f}, []Finding{f}, "T2")

	if got := st.Pending["a"].FirstSeen; got != "T1" {
		t.Errorf("FirstSeen = %q, want T1: it answers how long this has waited", got)
	}
}

// A service that errored this run was not really checked. Dropping its row
// would empty the queue on a transient registry failure and re-add it on the
// next run -- flapping a row a human is meant to act on.
func TestReconcile_ErroredServiceKeepsItsRow(t *testing.T) {
	st := NewState()
	f := available("a", "2.0.0")
	st.Reconcile([]Finding{f}, []Finding{f}, "T1")

	errored := Finding{Service: "a", Status: StatusError, Reason: "registry unreachable"}
	st.Reconcile(nil, []Finding{errored}, "T2")

	if len(st.Pending) != 1 {
		t.Errorf("a transient error must not empty the queue, got %+v", st.Pending)
	}
}

// A service removed from the compose file is not in the findings at all, so
// nothing can confirm it is gone. It keeps its row rather than being dropped
// on the strength of an absence.
func TestReconcile_UncheckedServiceKeepsItsRow(t *testing.T) {
	st := NewState()
	f := available("a", "2.0.0")
	st.Reconcile([]Finding{f}, []Finding{f}, "T1")

	st.Reconcile(nil, []Finding{upToDate("b")}, "T2")
	if _, ok := st.Pending["a"]; !ok {
		t.Error("a service absent from this run's findings should keep its row")
	}
}

func TestPendingList_SortedByService(t *testing.T) {
	st := NewState()
	for _, n := range []string{"web", "alpha", "mango"} {
		st.Pending[n] = Pending{Service: n}
	}
	got := st.PendingList()
	want := []string{"alpha", "mango", "web"}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	for i := range want {
		if got[i].Service != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestPendingList_EmptyIsEmptyNotNil(t *testing.T) {
	if got := NewState().PendingList(); got == nil {
		t.Error("PendingList should return an empty slice, not nil")
	}
}

// A soak that says nothing looks like nothing found. duva decided this update
// is wanted and is waiting on time, which is worth seeing -- not least so it
// can be taken early.
func TestReconcileSoaking_RecordsWhatIsHeldBack(t *testing.T) {
	st := NewState()
	st.ReconcileSoaking([]Finding{{
		Service: "app", File: "/compose/docker-compose.yml",
		Image: "example.com/app", CurrentTag: "1.2.0", Auto: AutoMinor,
		Soaking: &SoakingTag{
			Tag: "1.3.0", Age: 3 * 24 * time.Hour, Delay: 7 * 24 * time.Hour,
		},
	}})

	got, ok := st.Soaking["app"]
	if !ok {
		t.Fatal("the soaking candidate should be recorded")
	}
	if got.Candidate != "1.3.0" {
		t.Errorf("candidate = %q, want 1.3.0", got.Candidate)
	}
	if got.Remaining != "4 days" {
		t.Errorf("remaining = %q, want 4 days", got.Remaining)
	}
	// Within duva.auto: minor, so when the wait ends duva applies it.
	if got.Outcome != "will be applied automatically" {
		t.Errorf("outcome = %q, want it to say duva will apply it", got.Outcome)
	}
}

// The soak decides WHEN, the policy decides WHAT. A major soaking on a
// service that only auto-applies minors is not going to be applied when the
// wait ends -- saying so would be a promise duva does not keep.
func TestReconcileSoaking_OutcomeFollowsThePolicy(t *testing.T) {
	st := NewState()
	st.ReconcileSoaking([]Finding{{
		Service: "app", CurrentTag: "1.2.0", Auto: AutoMinor,
		Soaking: &SoakingTag{
			Tag: "2.0.0", Age: time.Hour, Delay: 48 * time.Hour,
		},
	}})

	if got := st.Soaking["app"].Outcome; got != "moves to approval" {
		t.Errorf("outcome = %q: a major beyond duva.auto is not applied when the soak ends", got)
	}
}

// A candidate that has soaked long enough is no longer soaking: it has moved
// to the queue or been applied, and a stale row would offer an update twice.
func TestReconcileSoaking_ForgetsWhatIsNoLongerHeld(t *testing.T) {
	st := NewState()
	st.ReconcileSoaking([]Finding{{
		Service: "app", CurrentTag: "1.2.0",
		Soaking: &SoakingTag{Tag: "1.3.0", Age: time.Hour, Delay: 48 * time.Hour},
	}})
	if len(st.Soaking) != 1 {
		t.Fatalf("expected one soaking entry, got %d", len(st.Soaking))
	}

	// The next check finds it no longer held back.
	st.ReconcileSoaking([]Finding{{Service: "app", CurrentTag: "1.2.0"}})

	if _, ok := st.Soaking["app"]; ok {
		t.Error("a candidate that finished soaking should not still be listed")
	}
}

// A service absent from a check was not looked at -- its file may not have
// been read -- which is different from having stopped soaking.
func TestReconcileSoaking_KeepsWhatWasNotChecked(t *testing.T) {
	st := NewState()
	st.ReconcileSoaking([]Finding{{
		Service: "app", CurrentTag: "1.2.0",
		Soaking: &SoakingTag{Tag: "1.3.0", Age: time.Hour, Delay: 48 * time.Hour},
	}})

	st.ReconcileSoaking([]Finding{{Service: "other", CurrentTag: "1.0.0"}})

	if _, ok := st.Soaking["app"]; !ok {
		t.Error("a service that was not checked should keep its soaking entry")
	}
}

// A check that errored says nothing about whether a service is still soaking:
// the registry was unreachable, not the release suddenly old enough. Dropping
// the row on a transient failure makes the one list an operator uses to decide
// whether to take a release early flap in and out of existence.
func TestReconcileSoaking_ErroredServiceKeepsItsRow(t *testing.T) {
	st := NewState()
	st.ReconcileSoaking([]Finding{{
		Service: "app", CurrentTag: "1.2.0",
		Soaking: &SoakingTag{Tag: "1.3.0", Age: time.Hour, Delay: 48 * time.Hour},
	}})

	st.ReconcileSoaking([]Finding{{
		Service: "app", CurrentTag: "1.2.0",
		Status: StatusError, Reason: "listing tags: registry unreachable",
	}})

	if _, ok := st.Soaking["app"]; !ok {
		t.Error("a failed check should not drop what was soaking")
	}
}

// HumanDuration is what the log and the page both say a wait is, which is why
// it is exported rather than formatted twice. These are the wordings, not
// examples of them: a soak with 40 minutes left says "less than an hour", and
// changing that changes what an operator reads.
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		// Days, singular and plural.
		{7 * 24 * time.Hour, "7 days"},
		{2 * 24 * time.Hour, "2 days"},
		{25 * time.Hour, "1 day"},

		// Under a day it switches to hours.
		{23 * time.Hour, "23 hours"},
		{2 * time.Hour, "2 hours"},

		// The last hour is not counted down: "1 hour" and "0 hours" would both
		// be wrong for a wait that is nearly over, and precision here is false
		// anyway -- the next check is what ends the soak, not the clock.
		{90 * time.Minute, "less than an hour"},
		{40 * time.Minute, "less than an hour"},
		{0, "less than an hour"},

		// A soak that finished between the check and the render is not
		// negative time; it has no time left.
		{-5 * time.Hour, "less than an hour"},
	}
	for _, c := range cases {
		if got := HumanDuration(c.d); got != c.want {
			t.Errorf("HumanDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// soakOutcome is the promise the page makes about what happens when the wait
// ends. Getting it backwards tells someone an update will apply itself when it
// will actually sit in the queue, or the reverse.
func TestSoakOutcome(t *testing.T) {
	const auto = "will be applied automatically"
	const queue = "moves to approval"

	cases := []struct {
		bump version.Kind
		auto Auto
		want string
	}{
		// Within the policy: duva applies it when the soak ends.
		{version.KindPatch, AutoPatch, auto},
		{version.KindPatch, AutoMinor, auto},
		{version.KindMinor, AutoMinor, auto},
		{version.KindMajor, AutoMajor, auto},

		// Beyond it: the soak ends and a person still has to decide.
		{version.KindMinor, AutoPatch, queue},
		{version.KindMajor, AutoMinor, queue},

		// No policy at all. Every bump waits for a human, including a patch --
		// this was the untested case, and it is the default a service has
		// until someone sets duva.auto.
		{version.KindPatch, AutoNone, queue},
		{version.KindMajor, AutoNone, queue},
	}
	for _, c := range cases {
		if got := soakOutcome(c.bump, c.auto); got != c.want {
			t.Errorf("soakOutcome(%v, %v) = %q, want %q", c.bump, c.auto, got, c.want)
		}
	}
}
