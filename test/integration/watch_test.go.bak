//go:build integration

package integration

import (
	"strings"
	"testing"
)

// duva noticing and announcing, without applying anything: a real registry, a
// real container, real HTTP notifications, all local.
//
// The unit tests fake the registry and the notifier, so what they cannot show
// is what duva actually announced -- whether a notification fired at all,
// fired once rather than every run, and named the right thing. That needs
// something on the other end of the request.

func TestNewerTagIsAnnouncedOnce(t *testing.T) {
	s := Up(t, "duva-watch/constrained-tag")

	s.PushAdditional()
	s.Pin("app")
	before := s.Image("app")

	s.Start()

	if got := s.Image("app"); got != before {
		t.Errorf("duva only reports here, but the compose file changed:\n  was %s\n  now %s", before, got)
	}
	if got := s.Notifications(); len(got) != 1 {
		t.Fatalf("want exactly one notification, got %d:\n%v", len(got), got)
	}
	if got := s.Notifications()[0]; !strings.Contains(got, "1.1.0-alpine") {
		t.Errorf("the notification does not name the successor tag:\n  %s", got)
	}
	if got := s.Notified("app"); got != "1.1.0-alpine" {
		t.Errorf("the successor should be recorded as notified, got %q", got)
	}

	// The same candidate on a later run must not be announced again: a tool
	// that repeats itself hourly gets muted, and then real news is missed too.
	s.CheckNow()
	if got := s.Notifications(); len(got) != 1 {
		t.Errorf("a second check repeated the notification, now %d:\n%v", len(got), got)
	}
}

func TestMovingTagRecordsABaselineSilently(t *testing.T) {
	s := Up(t, "duva-watch/moving-tag")
	s.Pin("app")

	s.Start()

	if got := s.Notifications(); len(got) != 0 {
		t.Errorf("nothing should be announced on first sight of a moving tag:\n%v", got)
	}
	baseline := s.Baseline("app")
	if !strings.HasPrefix(baseline, "sha256:") {
		t.Fatalf("a digest baseline should be recorded, got %q", baseline)
	}

	// Unchanged: still silent, and the baseline does not drift.
	s.CheckNow()
	if got := s.Notifications(); len(got) != 0 {
		t.Errorf("an unmoved tag should stay silent:\n%v", got)
	}
	if got := s.Baseline("app"); got != baseline {
		t.Errorf("the baseline moved while the tag did not:\n  was %s\n  now %s", baseline, got)
	}
}

func TestMovedDigestIsAnnounced(t *testing.T) {
	s := Up(t, "duva-watch/moving-tag")
	s.Pin("app")

	s.Start() // records the baseline
	if got := s.Notifications(); len(got) != 0 {
		t.Fatalf("the baseline run should be silent, so anything here is noise:\n%v", got)
	}

	// The tag moves: same name, new content, so a new digest.
	s.Push("app", "latest", "second")
	s.CheckNow()

	if got := s.Notifications(); len(got) != 1 {
		t.Fatalf("a moved digest should be announced once, got %d:\n%v", len(got), got)
	}
}

// Being pinned is the opt-in. A service without a digest is not watched at
// all -- no notification, no state -- so that duva is about deliberate pins
// going stale rather than generic drift-watching.
func TestUnpinnedServiceIsNotWatched(t *testing.T) {
	s := Up(t, "duva-watch/unpinned")

	s.PushAdditional()
	// Deliberately not pinned: that is what this test is about.

	s.Start()

	if got := s.Notifications(); len(got) != 0 {
		t.Errorf("nothing should be announced for an unwatched service:\n%v", got)
	}
	if got := s.Notified("app"); got != "" {
		t.Errorf("nothing should be recorded for an unwatched service, got %q", got)
	}
	if got := s.Baseline("app"); got != "" {
		t.Errorf("no baseline should be recorded for an unwatched service, got %q", got)
	}
}
