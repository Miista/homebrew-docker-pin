//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// duva must refuse to start when it cannot keep what it finds.
//
// This is the bug that shipped: a bind mount whose host path did not exist
// is created by the daemon owned by root, while duva runs as the host user
// so git will touch the repository at all. duva then checked every service
// over the network, succeeded, and threw the result away with one warning
// buried among the findings it was about to forget -- and looked, from the
// queue, entirely healthy.
//
// The unit tests cover the check itself. What only a container can show is
// that the ownership really does end up this way and that duva really does
// stop, rather than carrying on with a warning.
func TestUnwritableStateStopsDuva(t *testing.T) {
	s := Up(t, "duva-preflight/unwritable-state")
	s.PushUpdates()
	s.Pin("app")

	// The harness creates the state directory as this user -- which is the
	// kindness that hid the bug. Put it back the way a real first
	// deployment finds it.
	s.RootOwn("data")

	s.StartExpectingFailure("duva")

	out := s.Logs("duva")
	if !strings.Contains(out, "cannot write") {
		t.Errorf("duva should say it cannot write its state, got:\n%s", out)
	}
	if !strings.Contains(out, "chown") {
		t.Errorf("the message should say how to fix it, got:\n%s", out)
	}
	// And it must not have gone on to do the work anyway.
	if strings.Contains(out, "is available, waiting for approval") {
		t.Errorf("duva ran a check it could not record the result of:\n%s", out)
	}
	if strings.Contains(out, "could not save what was found") {
		t.Errorf("duva still discovered this after the fact rather than before:\n%s", out)
	}
}

// The same duva, with the ownership a correct deployment has, starts and
// records normally. Without this the test above would pass on a duva that
// refused to start for any reason at all.
func TestWritableStateStartsNormally(t *testing.T) {
	s := Up(t, "duva-preflight/unwritable-state")
	s.PushUpdates()
	s.Pin("app")

	// No RootOwn: the directory stays as the harness made it.
	s.Start()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Queued()) > 0 {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("duva did not record what it found:\n%s", s.Logs("duva"))
}
