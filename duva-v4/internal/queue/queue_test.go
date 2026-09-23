package queue

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/oci/version"
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
	return Entry{Service: service, From: from, To: to, Kind: version.KindMinor}
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
	v := Verdict{Outcome: Queue, Kind: version.KindMinor, From: "1.0.0", To: "1.1.0"}
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
	e := EntryFrom(Verdict{Kind: version.KindMinor, To: "1.1.0"}, s, "")
	if e.Auto != AutoPatch {
		t.Errorf("auto = %q, want patch", e.Auto)
	}
	if e.Service != "app" || e.File != "/compose/docker-compose.yml" {
		t.Errorf("identity is wrong: %+v", e)
	}
}

// Four releases arriving at once leave the newest queued, whatever the order.
//
// The watcher reports every tag published since the cutoff, in whatever order
// the registry listed them. This was last-write-wins, so authelia -- with
// 4.39.24 through 4.39.27 all outstanding -- ended up offering 4.39.24,
// because that is what arrived last. Every service with more than one release
// behind showed its oldest update.
func TestTheNewestCandidateWins(t *testing.T) {
	now := time.Now()
	for _, order := range [][]string{
		{"4.39.27", "4.39.26", "4.39.25", "4.39.24"}, // newest first, as seen
		{"4.39.24", "4.39.25", "4.39.26", "4.39.27"}, // oldest first
		{"4.39.25", "4.39.27", "4.39.24", "4.39.26"}, // arbitrary
	} {
		q := NewPending()
		for _, to := range order {
			q.Put(Entry{Service: "authelia", From: "4.39.20", To: to,
				Kind: version.KindPatch}, now)
		}
		e, ok := q.Get("authelia")
		if !ok {
			t.Fatalf("%v: nothing queued", order)
		}
		if e.To != "4.39.27" {
			t.Errorf("%v: queued %s, want the newest", order, e.To)
		}
	}
}

// An older candidate arriving is not news, so nothing is announced for it.
func TestAnOlderCandidateIsNotNews(t *testing.T) {
	now := time.Now()
	q := NewPending()

	if !q.Put(Entry{Service: "app", From: "1.0.0", To: "1.3.0", Kind: version.KindMinor}, now) {
		t.Fatal("the first candidate was not new")
	}
	if q.Put(Entry{Service: "app", From: "1.0.0", To: "1.1.0", Kind: version.KindMinor}, now) {
		t.Error("an older candidate reported itself as new, so it would notify again")
	}
	if e, _ := q.Get("app"); e.To != "1.3.0" {
		t.Errorf("queued %s, want the newer one kept", e.To)
	}
}

// A genuinely newer one still supersedes.
func TestANewerCandidateStillSupersedes(t *testing.T) {
	now := time.Now()
	q := NewPending()
	q.Put(Entry{Service: "app", From: "1.0.0", To: "1.1.0", Kind: version.KindMinor}, now)

	if !q.Put(Entry{Service: "app", From: "1.0.0", To: "1.3.0", Kind: version.KindMinor}, now) {
		t.Error("a newer candidate was not reported as new")
	}
	if e, _ := q.Get("app"); e.To != "1.3.0" {
		t.Errorf("queued %s, want the newer one", e.To)
	}
}

// A digest move has no version pair to compare, so the latest arrival wins --
// which is right: it is what the tag points at now.
func TestADigestMoveAlwaysTakesTheLatest(t *testing.T) {
	now := time.Now()
	q := NewPending()
	q.Put(Entry{Service: "app", Tag: "latest", Digest: "sha256:aaa", To: "sha256:aaa"}, now)
	q.Put(Entry{Service: "app", Tag: "latest", Digest: "sha256:bbb", To: "sha256:bbb"}, now)

	if e, _ := q.Get("app"); e.To != "sha256:bbb" {
		t.Errorf("queued %s, want what the tag points at now", e.To)
	}
}

// permutations returns every ordering of xs.
//
// Hand-written: Go has no equivalent, in slices, sort or anywhere else in the
// standard library. Recursive rather than three nested loops, so a case with
// four candidates needs no new helper.
func permutations(xs []string) [][]string {
	if len(xs) <= 1 {
		return [][]string{append([]string(nil), xs...)}
	}
	var out [][]string
	for i := range xs {
		rest := make([]string, 0, len(xs)-1)
		rest = append(rest, xs[:i]...)
		rest = append(rest, xs[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{xs[i]}, p...))
		}
	}
	return out
}

// Whatever order candidates arrive in, the queue ends up holding the same one.
//
// The property, stated as a property: the watcher reports every tag published
// since the cutoff in whatever order the registry listed them, so the queue's
// answer must not depend on that order. Every permutation, not a few chosen
// ones -- the test this replaces fed 1.1.0 then 1.2.0, ascending, where
// "keeps the newest" and "keeps the last to arrive" give the same answer and
// the bug was invisible.
func TestTheOutcomeIsTheSameWhateverTheOrder(t *testing.T) {
	tags := []string{"2.1.0", "2.3.0", "2.2.0"}
	const want = "2.3.0"

	perms := permutations(tags)
	if len(perms) != 6 {
		t.Fatalf("got %d permutations of 3, want 6", len(perms))
	}

	for _, order := range perms {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			q := NewPending()
			for _, to := range order {
				q.Put(entry("app", "2.0.0", to), now)
			}
			if q.Len() != 1 {
				t.Fatalf("got %d entries, want one -- candidates supersede rather than queue beside", q.Len())
			}
			got, ok := q.Get("app")
			if !ok {
				t.Fatal("nothing queued")
			}
			if got.To != want {
				t.Errorf("queued %s, want %s whatever the arrival order", got.To, want)
			}
		})
	}
}

// --- Reconcile ---------------------------------------------------------------

// A service satisfied by something other than this queue's own apply --
// a person running `docker pin upgrade` by hand, a hand-edited compose file --
// must not sit in the queue offering an update that already happened.
func TestReconcileDropsAnEntryTheWorldAlreadySatisfies(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)

	removed := q.Reconcile(func(e Entry) (bool, error) {
		return false, nil // the world says: not wanted any more
	}, nil)

	if len(removed) != 1 || removed[0] != "app" {
		t.Errorf("removed = %v, want [app]", removed)
	}
	if q.Len() != 0 {
		t.Errorf("entry survived reconcile: %+v", q.List())
	}
}

// An entry the world still disagrees with is left alone.
func TestReconcileKeepsAnEntryStillWanted(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)

	removed := q.Reconcile(func(e Entry) (bool, error) {
		return true, nil
	}, nil)

	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
	if q.Len() != 1 {
		t.Error("a still-wanted entry was dropped")
	}
}

// A read error is not evidence an entry is satisfied. Removing one on a
// spurious failure (a compose file briefly unreadable mid-write, say) would
// be worse than leaving a stale one an extra cycle.
func TestReconcileKeepsAnEntryOnReadError(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)

	var reported string
	removed := q.Reconcile(func(e Entry) (bool, error) {
		return false, errors.New("boom")
	}, func(service string, err error) { reported = service })

	if len(removed) != 0 {
		t.Errorf("removed = %v, want none on error", removed)
	}
	if q.Len() != 1 {
		t.Error("an entry was dropped on a read error rather than kept")
	}
	if reported != "app" {
		t.Errorf("onError called with %q, want app", reported)
	}
}

// Several entries, only some satisfied: only those leave.
func TestReconcileIsPerEntry(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	q.Put(entry("other", "2.0.0", "2.1.0"), now)

	removed := q.Reconcile(func(e Entry) (bool, error) {
		return e.Service != "app", nil // only "app" is satisfied
	}, nil)

	if len(removed) != 1 || removed[0] != "app" {
		t.Errorf("removed = %v, want [app]", removed)
	}
	if _, ok := q.Get("other"); !ok {
		t.Error("an unrelated entry was dropped")
	}
}
