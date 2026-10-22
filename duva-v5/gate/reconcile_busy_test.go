package gate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	ociversion "github.com/Miista/homebrew-docker-pin/oci/version"
)

// busyRecorder is an applier that says what it is applying.
type busyRecorder struct {
	startRecorder
	busy []string
}

func (b *busyRecorder) Applying() []string { return b.busy }

// Midway through an apply the compose file already pins the new release, so
// a reconcile would call the entry satisfied and drop it -- recording a
// completed update as dropped, and pulling its row off the page before it
// reports how it went. On speedtest-tracker, 2026-10-21. An entry being
// applied is left to its apply; once it is not, the same entry is dropped.
func TestReconcileLeavesAnEntryBeingApplied(t *testing.T) {
	const digest = "sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	dir := t.TempDir()
	root := filepath.Join(dir, "docker-compose.yml")
	compose := "services:\n  app:\n    image: example.com/app:1.0.1@" + digest + "\n    container_name: app\n"
	if err := os.WriteFile(root, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	queue := q.NewPending()
	queue.Put(q.Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: "1.0.1",
		Tag: "1.0.1", Digest: digest, Kind: ociversion.KindPatch, Auto: q.AutoNone}, soakNow)

	app := &busyRecorder{busy: []string{"app"}}
	r := &reconciler{queue: queue, root: root, applier: app, log: zerolog.Nop()}

	r.reconcile()
	if _, ok := queue.Get("app"); !ok {
		t.Fatal("an entry being applied was dropped by the reconcile")
	}

	app.busy = nil
	r.reconcile()
	if _, ok := queue.Get("app"); ok {
		t.Error("an entry the compose file already satisfies, not being applied, was kept")
	}
}
