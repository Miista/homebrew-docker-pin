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

const appService = `
services:
  app:
    image: example.com/app:latest@sha256:aaaa
`

// A digest-move entry is satisfied the moment the file already pins the
// digest it was waiting to move to -- whatever fixed it, not only this
// queue's own apply.
func TestStillWantedFalseWhenDigestAlreadyPinned(t *testing.T) {
	file := composeFile(t, appService)
	e := q.Entry{Service: "app", File: file, Digest: "sha256:aaaa"}

	ok, err := stillWanted(e)
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

	ok, err := stillWanted(e)
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
	ok, err := stillWanted(satisfied)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("still wanted a tag the file already follows")
	}

	unsatisfied := q.Entry{Service: "app", File: file, To: "1.2.3"}
	ok, err = stillWanted(unsatisfied)
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

	if _, err := stillWanted(e); err == nil {
		t.Error("expected an error for a service the file no longer declares")
	}
}
