package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	ociversion "github.com/Miista/homebrew-docker-pin/oci/version"
)

// composeFile writes a real compose file and returns its path. Real files
// rather than a fake reader, because recheck's job is reading one through the
// queue's Lookup -- a mocked reader would agree with whatever this test
// assumed. What is decided about the service is q.RecheckEntry's, and tested
// there.
func composeFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// container_name matters here, not just the image: the lookup goes through
// compose.ContainerIndex, which -- like the rest of this repo's compose
// reading -- only sees services that declare one.
const appService = `
services:
  app:
    image: example.com/app:latest@sha256:aaaa
    container_name: app
`

// A digest-move entry is satisfied the moment the file already pins the
// digest it was waiting to move to -- whatever fixed it, not only this
// queue's own apply.
func TestRecheckDropsADigestAlreadyPinned(t *testing.T) {
	file := composeFile(t, appService)
	e := q.Entry{Service: "app", File: file, Tag: "latest", From: "sha256:0000", To: "sha256:aaaa", Digest: "sha256:aaaa"}

	r, err := recheck(file, e)
	if err != nil {
		t.Fatal(err)
	}
	if r.Keep {
		t.Error("kept a digest the file already pins")
	}
}

func TestRecheckKeepsADigestMoveStillWhereItStarted(t *testing.T) {
	file := composeFile(t, appService)
	e := q.Entry{Service: "app", File: file, Tag: "latest", From: "sha256:aaaa", To: "sha256:bbbb", Digest: "sha256:bbbb"}

	r, err := recheck(file, e)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Keep {
		t.Errorf("dropped a move the file has not made: %s", r.Why)
	}
}

// The case the old question missed: the file moved on by other means, so the
// entry no longer starts where the service is.
func TestRecheckDropsAVersionChangeTheFileMovedAwayFrom(t *testing.T) {
	file := composeFile(t, appService) // follows latest now
	e := q.Entry{Service: "app", File: file, Image: "example.com/app",
		From: "1.2.2", To: "1.2.3", Tag: "1.2.3", Kind: ociversion.KindPatch}

	r, err := recheck(file, e)
	if err != nil {
		t.Fatal(err)
	}
	if r.Keep {
		t.Error("kept 1.2.2 -> 1.2.3 for a service that now follows latest")
	}
	if !strings.Contains(r.Why, "latest") {
		t.Errorf("why = %q, want it to say what the file follows now", r.Why)
	}
}

// A service the file no longer declares (renamed, removed) is a read error,
// not "no longer applies" -- see Reconcile's contract: an error must not be
// read as the entry no longer being wanted.
func TestRecheckErrorsWhenServiceIsGone(t *testing.T) {
	file := composeFile(t, appService)
	e := q.Entry{Service: "gone", File: file, Digest: "sha256:aaaa"}

	if _, err := recheck(file, e); err == nil {
		t.Error("expected an error for a service the file no longer declares")
	}
}

// An entry's stored File can go stale (a rename, a restructured include:),
// but the entry is still found and correctly compared because the lookup goes
// by service name through the current tree, not by trusting what was recorded
// when the entry was queued.
func TestRecheckFindsAServiceWhoseFileMoved(t *testing.T) {
	root := composeFile(t, "services: {}\n")
	renamed := filepath.Join(filepath.Dir(root), "renamed.yml")
	if err := os.WriteFile(renamed, []byte(appService), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("include: [renamed.yml]\nservices: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// File points at a path that was never real from root's perspective --
	// standing in for a stale value recorded before the rename.
	e := q.Entry{Service: "app", File: filepath.Join(filepath.Dir(root), "old.yml"),
		Tag: "latest", From: "sha256:0000", To: "sha256:aaaa", Digest: "sha256:aaaa"}

	r, err := recheck(root, e)
	if err != nil {
		t.Fatalf("expected the service to still be found despite the stale File: %v", err)
	}
	if r.Keep {
		t.Error("kept a digest the (renamed) file already pins")
	}
}
