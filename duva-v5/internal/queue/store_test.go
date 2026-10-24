package queue

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
)

func tempStore(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "duva.db")
}

// Load is the queue kept in the database at path, as a restarted duva opens
// it: a fresh connection to the same file.
func Load(t *testing.T, path string) *Pending {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { st.Close() })
	return Over(st, func(err error) { t.Errorf("the store failed: %v", err) })
}

func anEntry(service, to string) Entry {
	return Entry{Service: service, From: "1.0.0", To: to, Image: "example.com/app"}
}

// A queue survives a restart, and what it restores is not new -- which is what
// stops a deploy re-announcing everything still waiting.
func TestAQueueIsRestoredAcrossARestart(t *testing.T) {
	path := tempStore(t)
	now := time.Now()

	before := Load(t, path)
	before.Put(anEntry("radarr", "1.1.0"), now)
	before.Put(anEntry("sonarr", "2.1.0"), now)

	after := Load(t, path)
	if got := after.Len(); got != 2 {
		t.Fatalf("restored %d entries, want 2", got)
	}

	// The whole point: the same candidate arriving again is not news, so
	// nothing is announced a second time.
	if after.Put(anEntry("radarr", "1.1.0"), now) {
		t.Error("a restored entry reported itself as new, so it would notify again")
	}
	// A superseding one still is.
	if !after.Put(anEntry("radarr", "1.2.0"), now) {
		t.Error("a newer candidate was not reported as new")
	}
}

// Approving something removes it from disk too, or it would come back on the
// next restart as an entry waiting for a decision already made.
func TestRemovingAnEntryRemovesItFromTheStore(t *testing.T) {
	path := tempStore(t)
	q := Load(t, path)
	q.Put(anEntry("radarr", "1.1.0"), time.Now())
	q.Remove("radarr")

	if got := Load(t, path).Len(); got != 0 {
		t.Errorf("restored %d entries after a removal, want 0", got)
	}
}

// FirstSeen must survive, or a row that has waited a week looks new after a
// deploy.
func TestFirstSeenSurvivesARestart(t *testing.T) {
	path := tempStore(t)
	then := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	q := Load(t, path)
	q.Put(anEntry("radarr", "1.1.0"), then)

	e, ok := Load(t, path).Get("radarr")
	if !ok {
		t.Fatal("nothing restored")
	}
	if e.FirstSeen != then.UTC().Format(time.RFC3339) {
		t.Errorf("FirstSeen = %q, want it carried across the restart", e.FirstSeen)
	}
}

// No path is a queue with no volume: it works exactly as it did before, in
// memory and writing nowhere.
func TestNoStoreIsAnOrdinaryQueue(t *testing.T) {
	q := NewPending()
	if !q.Put(anEntry("radarr", "1.1.0"), time.Now()) {
		t.Error("a queue without a store did not accept an entry")
	}
	if q.Len() != 1 {
		t.Error("a queue without a store did not hold it")
	}
}
