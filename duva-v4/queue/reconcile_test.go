package main

import (
	"os"
	"path/filepath"
	"testing"

	q "github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
)

// composeFile writes a real compose file and returns its path. Real files
// rather than a fake reader, because stillWanted's whole job is reading one
// through compose.RawImage -- a mocked reader would agree with whatever this
// test assumed.
func composeFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// container_name matters here, not just the image: stillWanted now resolves
// a service's current file via compose.ContainerIndex, which -- like the
// rest of this repo's compose reading -- only sees services that declare one.
const appService = `
services:
  app:
    image: example.com/app:latest@sha256:aaaa
    container_name: app
`

// A digest-move entry is satisfied the moment the file already pins the
// digest it was waiting to move to -- whatever fixed it, not only this
// queue's own apply.
func TestStillWantedFalseWhenDigestAlreadyPinned(t *testing.T) {
	file := composeFile(t, appService)
	e := q.Entry{Service: "app", File: file, Digest: "sha256:aaaa"}

	ok, err := stillWanted(file, e)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("still wanted a digest the file already pins")
	}
}

func TestStillWantedTrueWhenDigestDiffers(t *testing.T) {
	file := composeFile(t, appService)
	e := q.Entry{Service: "app", File: file, Digest: "sha256:bbbb"}

	ok, err := stillWanted(file, e)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("wanted a digest the file does not yet pin, but stillWanted said no")
	}
}

// A version-change entry with no digest yet (the updater resolves it by
// pulling the tag) is compared on the tag alone.
func TestStillWantedComparesTagWhenNoDigestKnown(t *testing.T) {
	file := composeFile(t, appService)

	satisfied := q.Entry{Service: "app", File: file, To: "latest"} // matches the file's tag
	ok, err := stillWanted(file, satisfied)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("still wanted a tag the file already follows")
	}

	unsatisfied := q.Entry{Service: "app", File: file, To: "1.2.3"}
	ok, err = stillWanted(file, unsatisfied)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("wanted a tag the file does not yet follow, but stillWanted said no")
	}
}

// A service the file no longer declares (renamed, removed) is a read error,
// not "satisfied" -- see Reconcile's contract: an error must not be read as
// the entry no longer being wanted.
func TestStillWantedErrorsWhenServiceIsGone(t *testing.T) {
	file := composeFile(t, appService)
	e := q.Entry{Service: "gone", File: file, Digest: "sha256:aaaa"}

	if _, err := stillWanted(file, e); err == nil {
		t.Error("expected an error for a service the file no longer declares")
	}
}

// The whole point: an entry's stored File can go stale (a rename, a
// restructured include:), but the entry is still found and correctly
// compared because the lookup goes by service name through the current tree,
// not by trusting what was recorded when the entry was queued.
func TestStillWantedFindsAServiceWhoseFileMoved(t *testing.T) {
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
	e := q.Entry{Service: "app", File: filepath.Join(filepath.Dir(root), "old.yml"), Digest: "sha256:aaaa"}

	ok, err := stillWanted(root, e)
	if err != nil {
		t.Fatalf("expected the service to still be found despite the stale File: %v", err)
	}
	if ok {
		t.Error("still wanted a digest the (renamed) file already pins")
	}
}
