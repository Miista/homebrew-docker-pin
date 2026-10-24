package queue

import (
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

func soakingEntry(to string, releaseAt time.Time) Entry {
	return Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: to, Tag: to,
		Kind: version.KindPatch, Auto: AutoPatch, ReleaseAt: releaseAt.UTC().Format(time.RFC3339)}
}

// statuses is every entry's status for a service, by what it offered.
func statuses(t *testing.T, q *Pending, service string) map[string]string {
	t.Helper()
	rows, err := q.st.DB().Query(`SELECT e.to_ref, e.status FROM queue_entries e
		JOIN services s ON s.id = e.service_id WHERE s.name = ?`, service)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var to, status string
		rows.Scan(&to, &status)
		out[to] = status
	}
	return out
}

// The reason for a row per release. A project releasing faster than its soak
// used to restart the wait with every release, and never applied; now the
// older release comes due on its own clock while the newer one soaks.
func TestAnOlderReleaseComesDueWhileANewerOneSoaks(t *testing.T) {
	q := NewPending()
	q.Put(soakingEntry("1.0.1", now.Add(time.Hour)), now)
	q.Put(soakingEntry("1.0.2", now.Add(3*time.Hour)), now)

	// What a person sees is the newest.
	if e, _ := q.Get("app"); e.To != "1.0.2" {
		t.Errorf("Get = %s, want the newest", e.To)
	}

	due := q.Due(now.Add(2 * time.Hour))
	if len(due) != 1 || due[0].To != "1.0.1" {
		t.Fatalf("due = %+v, want 1.0.1 alone", due)
	}
	released, ok := q.Release(due[0], now.Add(2*time.Hour))
	if !ok {
		t.Fatal("not released")
	}
	q.Complete(released, now.Add(2*time.Hour))

	got := statuses(t, q, "app")
	if got["1.0.1"] != "completed" || got["1.0.2"] != "waiting" {
		t.Errorf("statuses = %v, want 1.0.1 completed and 1.0.2 still soaking", got)
	}
}

// Applying a release settles everything older for that service, and nothing
// newer.
func TestCompletingSupersedesOnlyWhatIsOlder(t *testing.T) {
	q := NewPending()
	q.Put(soakingEntry("1.0.1", now.Add(time.Hour)), now)
	mid, _ := q.PutEntry(soakingEntry("1.0.2", now.Add(time.Hour)), now)
	q.Put(soakingEntry("1.0.3", now.Add(time.Hour)), now)

	q.Complete(mid, now)

	got := statuses(t, q, "app")
	want := map[string]string{"1.0.1": "superseded", "1.0.2": "completed", "1.0.3": "waiting"}
	for to, status := range want {
		if got[to] != status {
			t.Errorf("%s is %s, want %s", to, got[to], status)
		}
	}
}

// For a person, only the newest matters: approving it makes the rest moot,
// and a row that is already overtaken invites approving something stale.
func TestANewerReleaseForAPersonSupersedesTheOlder(t *testing.T) {
	q := NewPending()
	q.Put(Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: "1.1.0", Kind: version.KindMinor}, now)
	q.Put(Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: "1.2.0", Kind: version.KindMinor}, now)

	got := statuses(t, q, "app")
	if got["1.1.0"] != "superseded" || got["1.2.0"] != "waiting" {
		t.Errorf("statuses = %v", got)
	}
	if q.Len() != 1 {
		t.Errorf("Len = %d, want one service waiting", q.Len())
	}
}

// Every change is in the history, with why.
func TestTheHistoryRecordsWhatHappened(t *testing.T) {
	q := NewPending()
	q.Put(Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: "1.1.0", Kind: version.KindMinor}, now)
	e, _ := q.PutEntry(Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: "1.2.0", Kind: version.KindMinor}, now)
	q.Complete(e, now)

	rows, err := q.st.DB().Query(`SELECT kind FROM queue_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		kinds = append(kinds, k)
	}
	want := []string{"found", "found", "superseded", "completed"}
	if len(kinds) != len(want) {
		t.Fatalf("history = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("history = %v, want %v", kinds, want)
			break
		}
	}
}

// A service that leaves and comes back is the same row, so the same history.
// Back is the sync's to say, from the compose file and the containers; a
// finding does not bring a removed service back.
func TestAServiceThatComesBackKeepsItsRow(t *testing.T) {
	q := NewPending()
	app := []Declared{{Container: "my-app", Service: "app", Image: "example.com/app", Tag: "1"}}
	mine := []Container{{Name: "my-app", Service: "app"}}
	q.SyncServices(app, mine, now)
	q.Put(Entry{Service: "app", Container: "my-app", Image: "example.com/app", From: "1.0.0", To: "1.1.0"}, now)
	q.SyncServices(nil, nil, now)
	if !q.Removed("my-app") {
		t.Fatal("gone from the file with no container, and not removed")
	}
	q.SyncServices(app, mine, now)

	var services int
	var removed *string
	q.st.DB().QueryRow(`SELECT COUNT(*) FROM services`).Scan(&services)
	q.st.DB().QueryRow(`SELECT removed_at FROM services`).Scan(&removed)
	if services != 1 || removed != nil {
		t.Errorf("services = %d, removed_at = %v; want one row, enabled again", services, removed)
	}
}

// What waits for a person and what soaks are two lists. A service can be in
// both: a patch soaking, and a minor waiting for someone.
func TestWaitingAndSoakingAreApart(t *testing.T) {
	q := NewPending()
	q.Put(soakingEntry("1.0.1", now.Add(2*time.Hour)), now)
	q.Put(soakingEntry("1.0.2", now.Add(time.Hour)), now)
	q.Put(Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: "1.1.0", Tag: "1.1.0", Kind: version.KindMinor}, now)
	q.Put(Entry{Service: "db", Image: "postgres", From: "16", To: "17", Tag: "17", Kind: version.KindMajor}, now)

	waiting := q.Waiting()
	if len(waiting) != 2 || waiting[0].To != "1.1.0" || waiting[1].Service != "db" {
		t.Errorf("waiting = %+v", waiting)
	}
	soaking := q.Soaking()
	if len(soaking) != 2 || soaking[0].To != "1.0.2" || soaking[1].To != "1.0.1" {
		t.Errorf("soaking = %+v, want both, the soonest released first", soaking)
	}
}
