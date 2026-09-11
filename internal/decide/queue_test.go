package decide

import (
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/registry"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

var now = at("2026-09-11T12:00:00Z")

func entry(service, from, to string) Entry {
	return Entry{Service: service, From: from, To: to, Kind: registry.KindMinor}
}

func TestPutAndGet(t *testing.T) {
	q := NewPending()
	if changed := q.Put(entry("app", "1.0.0", "1.1.0"), now); !changed {
		t.Error("a first entry should count as changed")
	}
	got, ok := q.Get("app")
	if !ok {
		t.Fatal("the entry is missing")
	}
	if got.To != "1.1.0" {
		t.Errorf("to = %q, want 1.1.0", got.To)
	}
	if got.FirstSeen == "" {
		t.Error("first seen was not stamped")
	}
}

// Supersession, which is the reason the queue is keyed by service: a newer
// candidate replaces the pending one rather than queueing beside it, so it is
// impossible to approve something already overtaken.
func TestNewerCandidateSupersedes(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	q.Put(entry("app", "1.0.0", "1.2.0"), now)

	if q.Len() != 1 {
		t.Fatalf("got %d entries, want 1 -- the second should replace the first", q.Len())
	}
	got, _ := q.Get("app")
	if got.To != "1.2.0" {
		t.Errorf("to = %q, want the newer 1.2.0", got.To)
	}
}

// The same candidate arriving again is not a change. Under a protocol that
// re-notifies until something is done, this is the common case, and reporting
// it as a change would log and notify on every run.
func TestSameCandidateIsNotAChange(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	if changed := q.Put(entry("app", "1.0.0", "1.1.0"), now); changed {
		t.Error("re-notifying the same candidate reported a change")
	}
}

// A superseding candidate gets its own FirstSeen. Carrying the old one
// forward would make a row that has waited five minutes look like it had
// waited a week.
func TestSupersedingResetsFirstSeen(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	first, _ := q.Get("app")

	later := now.Add(48 * time.Hour)
	q.Put(entry("app", "1.0.0", "1.2.0"), later)
	second, _ := q.Get("app")

	if second.FirstSeen == first.FirstSeen {
		t.Error("the superseding entry kept the old first-seen")
	}
	if second.FirstSeen != later.UTC().Format(time.RFC3339) {
		t.Errorf("first seen = %q, want %q", second.FirstSeen, later.UTC().Format(time.RFC3339))
	}
}

// An explicit FirstSeen survives, so state restored from disk does not reset
// every row's age on startup.
func TestExplicitFirstSeenIsKept(t *testing.T) {
	q := NewPending()
	e := entry("app", "1.0.0", "1.1.0")
	e.FirstSeen = "2026-01-01T00:00:00Z"
	q.Put(e, now)

	got, _ := q.Get("app")
	if got.FirstSeen != "2026-01-01T00:00:00Z" {
		t.Errorf("first seen = %q, want the one supplied", got.FirstSeen)
	}
}

func TestRemove(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	if !q.Remove("app") {
		t.Error("removing a present entry reported nothing there")
	}
	if q.Remove("app") {
		t.Error("removing an absent entry reported something there")
	}
	if q.Len() != 0 {
		t.Errorf("len = %d, want 0", q.Len())
	}
}

func TestGetMissing(t *testing.T) {
	q := NewPending()
	if _, ok := q.Get("nothing"); ok {
		t.Error("an absent service reported present")
	}
}

// Order must be stable, or a row moves under the cursor between reloads.
func TestListIsSortedByService(t *testing.T) {
	q := NewPending()
	for _, name := range []string{"zebra", "alpha", "middle"} {
		q.Put(entry(name, "1.0.0", "1.1.0"), now)
	}
	got := q.List()
	want := []string{"alpha", "middle", "zebra"}
	for i, w := range want {
		if got[i].Service != w {
			t.Errorf("position %d = %q, want %q", i, got[i].Service, w)
		}
	}
}

func TestListEmpty(t *testing.T) {
	if got := NewPending().List(); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// --- the clock ---------------------------------------------------------------

// A soak is the same entry with a release time. Once it passes, the entry is
// due; before, it is not.
func TestDueReleasesOnTime(t *testing.T) {
	q := NewPending()
	e := entry("app", "1.0.0", "1.1.0")
	e.ReleaseAt = now.Add(time.Hour).UTC().Format(time.RFC3339)
	q.Put(e, now)

	if due := q.Due(now); len(due) != 0 {
		t.Errorf("released early: %+v", due)
	}
	if due := q.Due(now.Add(2 * time.Hour)); len(due) != 1 {
		t.Errorf("not released after the time passed, got %d", len(due))
	}
}

// Exactly at the release time counts as due -- the wait is over, not almost
// over.
func TestDueAtTheExactMoment(t *testing.T) {
	q := NewPending()
	e := entry("app", "1.0.0", "1.1.0")
	release := now.Add(time.Hour)
	e.ReleaseAt = release.UTC().Format(time.RFC3339)
	q.Put(e, now)

	if due := q.Due(release); len(due) != 1 {
		t.Errorf("not due at the exact release time, got %d", len(due))
	}
}

// An entry waiting for a person is never due. Asking when it will be would be
// asking when someone will make up their mind.
func TestEntryWaitingForAPersonIsNeverDue(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now) // no ReleaseAt
	if due := q.Due(now.Add(10 * 365 * 24 * time.Hour)); len(due) != 0 {
		t.Errorf("an entry with no release time came due: %+v", due)
	}
}

// An unreadable release time must not release anything early. It waits, and
// stays visible, which is the safe direction for a field nobody can parse.
func TestUnparseableReleaseTimeDoesNotRelease(t *testing.T) {
	q := NewPending()
	e := entry("app", "1.0.0", "1.1.0")
	e.ReleaseAt = "whenever"
	q.Put(e, now)

	if due := q.Due(now.Add(time.Hour)); len(due) != 0 {
		t.Errorf("an unparseable release time released the entry: %+v", due)
	}
	if q.Len() != 1 {
		t.Error("and it should still be queued, not dropped")
	}
}

func TestDueIsSorted(t *testing.T) {
	q := NewPending()
	past := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, name := range []string{"zebra", "alpha"} {
		e := entry(name, "1.0.0", "1.1.0")
		e.ReleaseAt = past
		q.Put(e, now)
	}
	got := q.Due(now)
	if len(got) != 2 || got[0].Service != "alpha" {
		t.Errorf("got %+v, want alpha first", got)
	}
}

// --- building a row ----------------------------------------------------------

// The tag is an instruction, not a description of what is pinned: a version
// change sets it, a digest move leaves it alone.
func TestEntryFromSetsTheTagOnAVersionChange(t *testing.T) {
	s := svc("1.0.0", "sha256:old", AutoNone)
	v := Verdict{Outcome: Queue, Kind: registry.KindMinor, From: "1.0.0", To: "1.1.0"}
	e := EntryFrom(v, s, "")
	if e.Tag != "1.1.0" {
		t.Errorf("tag = %q, want the new version", e.Tag)
	}
}

func TestEntryFromKeepsTheTagOnADigestMove(t *testing.T) {
	s := svc("latest", "sha256:old", AutoNone)
	v := Verdict{Outcome: Queue, From: "sha256:old", To: "sha256:new"} // no Kind
	e := EntryFrom(v, s, "sha256:new")
	if e.Tag != "latest" {
		t.Errorf("tag = %q, want latest -- a digest move does not change the tag", e.Tag)
	}
	if e.Digest != "sha256:new" {
		t.Errorf("digest = %q", e.Digest)
	}
}

// The policy travels with the row, so a queue can show the rule and not only
// the verdict it produced.
func TestEntryFromCarriesThePolicy(t *testing.T) {
	s := svc("1.0.0", "sha256:old", AutoPatch)
	e := EntryFrom(Verdict{Kind: registry.KindMinor, To: "1.1.0"}, s, "")
	if e.Auto != AutoPatch {
		t.Errorf("auto = %q, want patch", e.Auto)
	}
	if e.Service != "app" || e.File != "/compose/docker-compose.yml" {
		t.Errorf("identity is wrong: %+v", e)
	}
}
