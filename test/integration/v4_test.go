//go:build integration

package integration

import (
	"strings"
	"testing"
)

// The v4 pipeline against a real daemon, a real registry and real compose
// files.
//
// Three processes rather than v3's one, which is the thing worth testing at
// this level: the unit tests fake every boundary between them, so what they
// cannot show is whether a finding actually reaches the queue, whether the
// queue actually reaches the updater, or whether anything is announced at
// all.

// A patch within duva.auto goes all the way through without anyone asked.
func TestV4AppliesWhatPolicyAllows(t *testing.T) {
	s := Up(t, "duva-v4/within-policy")
	s.PushUpdates()
	s.Pin("app")
	s.Start()

	t.Run("the update is applied", func(t *testing.T) {
		if got := s.RunningImage("app"); !strings.Contains(got, "1.0.1") {
			t.Errorf("1.0.0 -> 1.0.1 was not applied:\n  got %s", got)
		}
	})

	t.Run("the compose file records it", func(t *testing.T) {
		if got := s.Image("app"); !strings.Contains(got, "1.0.1@sha256:") {
			t.Errorf("the pin does not name what runs:\n  got %s", got)
		}
	})

	t.Run("and it is committed", func(t *testing.T) {
		// The fixture's template is:
		//   pin {{.Container}} to {{if eq .OldVersion .NewVersion}}...{{else}}{{.NewVersion}}{{end}}
		if got := s.Log(); !strings.Contains(got, "pin app to 1.0.1") {
			t.Errorf("the change was not committed under the template:\n  got %s", got)
		}
	})

	t.Run("nothing is left waiting for a person", func(t *testing.T) {
		if got := s.Queued(); len(got) != 0 {
			t.Errorf("queue is %v, want empty -- policy allowed this one", got)
		}
	})
}

// An auto-apply that fails tells someone, and a successful one does not.
//
// The half that was silent: policy applied something without asking, it did
// not finish, and nothing said so -- while the container may be down and the
// repository dirty. The unit tests cover each piece and nothing covered the
// path between them, which is what this is for.
func TestV4AFailedAutoApplyIsAnnounced(t *testing.T) {
	s := Up(t, "duva-v4/failed-apply")
	// Pulls, pins, and then will not start: FROM scratch with no entrypoint.
	s.PushUnrunnable("app", "1.0.1")
	s.Pin("app")
	s.Start()

	t.Run("someone is told", func(t *testing.T) {
		var failure string
		for _, n := range s.Notifications() {
			if strings.Contains(n, "auto-apply failed") {
				failure = n
			}
		}
		if failure == "" {
			t.Fatalf("no failure was announced:\n  %v", s.Notifications())
		}
		if !strings.Contains(failure, "app") {
			t.Errorf("the announcement does not name the service:\n  %s", failure)
		}
	})

	t.Run("it is not left looking like it worked", func(t *testing.T) {
		// Whatever else is true, the container is not running the new image:
		// that is what failing to start means.
		if got := s.RunningImage("app"); strings.Contains(got, "1.0.1") {
			t.Errorf("the container is running the image that could not start: %s", got)
		}
	})
}
