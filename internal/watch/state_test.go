package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Miista/homebrew-docker-pin/internal/registry"
)

func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "duva.json")
}

func TestState_RoundTrip(t *testing.T) {
	p := statePath(t)
	st := NewState()
	st.Baseline["a"] = "sha256:x"
	st.Notified["b"] = "1.2.3"
	st.Pending["c"] = Pending{
		Service: "c", Image: "x/y", CurrentTag: "1.0.0",
		Kind: KindTag, Candidate: "2.0.0", Bump: registry.KindMajor,
		Why: "major exceeds duva.auto: patch", FirstSeen: "2026-01-01T00:00:00Z",
	}
	if err := st.Save(p); err != nil {
		t.Fatal(err)
	}

	got, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Baseline["a"] != "sha256:x" || got.Notified["b"] != "1.2.3" {
		t.Errorf("round trip lost data: %+v", got)
	}
	p2 := got.Pending["c"]
	if p2.Candidate != "2.0.0" || p2.Bump != registry.KindMajor || p2.Why == "" || p2.FirstSeen == "" {
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
	if err := os.WriteFile(p, []byte(`{"baseline":{"a":"sha256:x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	st.Notified["b"] = "1.0.0"  // must not panic
	st.Pending["c"] = Pending{} // must not panic
	if st.Baseline["a"] != "sha256:x" {
		t.Error("existing section lost")
	}
}

// The state file is rewritten on every run; a crash mid-write must not leave
// a truncated file that fails to parse on the next start.
func TestSave_IsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "duva.json")
	st := NewState()
	st.Baseline["a"] = "sha256:x"
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
