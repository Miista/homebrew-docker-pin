package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
)

// open is a real database in a temporary directory: what is under test is
// SQLite's behaviour as much as this package's, so there is no fake.
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "duva.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// latest is the newest migration's number.
func latest(t *testing.T) int {
	t.Helper()
	files, _ := fs.Glob(migrations, "migrations/*.sql")
	n, err := versionOf(files[len(files)-1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpenBringsTheSchemaUpToDate(t *testing.T) {
	s := open(t)
	v, err := s.Version()
	if err != nil || v != latest(t) {
		t.Errorf("version = %d %v, want %d", v, err, latest(t))
	}
	for _, table := range []string{"images", "services", "watch_cutoffs", "watch_held", "settings", "queue_entries", "queue_events", "blocked_releases"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s is missing", table)
		}
	}
}

// Opening again applies nothing twice.
func TestReopeningIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duva.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		v, _ := s.Version()
		s.Close()
		if v != latest(t) {
			t.Errorf("open %d: version %d, want %d", i, v, latest(t))
		}
	}
}

// SQLite has foreign keys off unless asked. A queue entry for a service that
// does not exist is the thing they are here to refuse.
func TestForeignKeysAreEnforced(t *testing.T) {
	s := open(t)
	_, err := s.db.Exec(`INSERT INTO queue_entries
		(service_id, file, from_ref, to_ref, tag, auto, why, found_at, status)
		VALUES (999, 'f', '1', '2', '2', 'none', 'x', '2026-10-20T00:00:00Z', 'waiting')`)
	if err == nil {
		t.Error("queued an entry for a service that does not exist")
	}
}

func TestTheJournalIsWAL(t *testing.T) {
	s := open(t)
	var mode string
	s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestATransactionThatFailsLeavesNothing(t *testing.T) {
	s := open(t)
	err := s.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO images (repo) VALUES ('example.com/app')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO images (repo) VALUES ('example.com/app')`) // UNIQUE
		return err
	})
	if err == nil {
		t.Fatal("a duplicate image was accepted")
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&n)
	if n != 0 {
		t.Errorf("%d image(s) left after a rolled-back transaction", n)
	}
}

// One standing block per digest, but an unblocked one stays as history and
// the digest can be blocked again.
func TestABlockCanBeLiftedAndRepeated(t *testing.T) {
	s := open(t)
	s.db.Exec(`INSERT INTO images (id, repo) VALUES (1, 'example.com/app')`)
	block := `INSERT INTO blocked_releases (image_id, digest, reason, blocked_by, blocked_at)
		VALUES (1, 'sha256:bad', 'missing migrations', 'soren', '2026-10-20T00:00:00Z')`
	if _, err := s.db.Exec(block); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(block); err == nil {
		t.Error("the same digest was blocked twice at once")
	}
	s.db.Exec(`UPDATE blocked_releases SET unblocked_at = '2026-10-21T00:00:00Z'`)
	if _, err := s.db.Exec(block); err != nil {
		t.Errorf("could not block again after unblocking: %v", err)
	}
}
