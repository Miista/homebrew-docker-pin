package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// v4's files, as v4 wrote them, become the database's state, and are kept
// renamed rather than deleted.
func TestV4StateIsImportedOnce(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "docker-compose.yml")
	write(t, root, "services:\n  radarr:\n    image: example.com/radarr:1.0.0@sha256:a\n    container_name: radarr-main\n")
	write(t, filepath.Join(dir, "queue.json"), `{
	  "radarr": {"service": "radarr", "file": "f", "image": "example.com/radarr",
	             "from": "1.0.0", "to": "1.1.0", "tag": "1.1.0", "kind": "minor",
	             "why": "minor; duva.auto is none", "auto": "none",
	             "first_seen": "2026-09-01T12:00:00Z"}}`)
	write(t, filepath.Join(dir, "detector.json"), `{"services": {
	  "radarr": {"cutoff": "2026-10-01T03:00:00Z",
	             "pending": {"1.2.0": {"Service": "radarr", "Tag": "1.2.0"}}}}}`)

	st, err := store.Open(filepath.Join(dir, "duva.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := importJSON(st, dir, root, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}

	e, ok := q.Over(st, nil).Get("radarr")
	if !ok || e.To != "1.1.0" || e.FirstSeen != "2026-09-01T12:00:00Z" {
		t.Errorf("imported entry = %+v %v, want 1.1.0 with its age kept", e, ok)
	}
	if e.Container != "radarr-main" {
		t.Errorf("container = %q, want the one the compose file names", e.Container)
	}
	var cutoffs, held int
	st.DB().QueryRow(`SELECT COUNT(*) FROM watch_cutoffs`).Scan(&cutoffs)
	st.DB().QueryRow(`SELECT COUNT(*) FROM watch_held`).Scan(&held)
	if cutoffs != 1 || held != 1 {
		t.Errorf("cutoffs %d, held %d; want 1 and 1", cutoffs, held)
	}
	for _, f := range []string{"queue.json", "detector.json"} {
		if _, err := os.Stat(filepath.Join(dir, f+".migrated")); err != nil {
			t.Errorf("%s was not kept as .migrated", f)
		}
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s is still there to be imported again", f)
		}
	}
}

// A file appearing later must not overwrite a database that has state.
func TestALateFileDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "duva.db"))
	defer st.Close()
	q.Over(st, nil).Put(q.Entry{Service: "radarr", Image: "example.com/radarr", From: "1.0.0", To: "2.0.0"}, time.Now())
	write(t, filepath.Join(dir, "queue.json"), `{"radarr": {"service": "radarr", "image": "example.com/radarr", "from": "1.0.0", "to": "1.1.0"}}`)

	if err := importJSON(st, dir, filepath.Join(dir, "none.yml"), zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	if e, _ := q.Over(st, nil).Get("radarr"); e.To != "2.0.0" {
		t.Errorf("a late queue.json overwrote the database: %+v", e)
	}
}
