//go:build integration

package integration

import (
	"slices"
	"strings"
	"testing"
)

// duva.delay holds a candidate back until the release has been out long
// enough. The wait is a default, not a lock: an operator who knows a release
// is fine can take it early, which they cannot do if duva never mentions it.
//
// Before this, a soaking candidate was discarded during selection -- so duva
// looked like it had found nothing when it had found something and chosen to
// wait.
func TestSoakingUpdateIsShownAndCanBeTakenEarly(t *testing.T) {
	s := Up(t, "duva-soak/held-back")

	// Published as this test runs, so nothing has soaked: 1.1.0 is minutes
	// old against a 7d delay.
	s.PushUpdates()
	s.Pin("app")

	before := s.Container("app")
	if before == "" {
		t.Fatal("the service should be running before duva looks at it")
	}
	pinned := s.Image("app")

	s.Start()

	t.Run("it is not applied while it soaks", func(t *testing.T) {
		if got := s.Image("app"); got != pinned {
			t.Errorf("the soak did not hold the update back:\n  was %s\n  now %s", pinned, got)
		}
		// Not in the approval queue either: it has been decided, and is
		// waiting on time rather than on a person.
		if got := s.Queued(); slices.Contains(got, "app") {
			t.Errorf("a soaking update should not be in the approval queue, got %v", got)
		}
	})

	t.Run("the page shows what is waiting", func(t *testing.T) {
		page := s.Queue()
		// The service and its candidate: data, not copy. What the page calls
		// the section is free to change.
		for _, want := range []string{"app", "1.1.0"} {
			if !strings.Contains(page, want) {
				t.Errorf("the page should show %q", want)
			}
		}
		if !strings.Contains(page, `action="/apply"`) {
			t.Error("a soaking update should be offered for updating early")
		}
	})

	// The feature: pressing Update on a soaking row overrides the wait.
	t.Run("it can be taken early from the page", func(t *testing.T) {
		s.UpdateNow("app")

		if got := s.Image("app"); !strings.Contains(got, ":1.1.0@sha256:") {
			t.Fatalf("the update was not applied:\n  got %s", got)
		}
		after := s.Container("app")
		if after == "" || after == before {
			t.Error("the container was not replaced")
		}
		if got := s.RunningImage(after); !strings.Contains(got, "1.1.0") {
			t.Errorf("the container is not on the new image:\n  got %s", got)
		}
		if got := s.Log(); !strings.Contains(got, "app: 1.0.0 -> 1.1.0") {
			t.Errorf("the change was not committed:\n  got %s", got)
		}
	})
}
