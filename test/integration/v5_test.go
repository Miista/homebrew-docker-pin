//go:build integration

package integration

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// duva v5 against a real daemon, a real registry and real compose files.
//
// The same scenarios as v4's, run through v5's one image. They are copies on
// purpose: v4 keeps running until v5 replaces it, and its suite has to keep
// passing against what it ships, while v5's changes under it.
//
// Three processes rather than v3's one, which is the thing worth testing at
// this level: the unit tests fake every boundary between them, so what they
// cannot show is whether a finding actually reaches the queue, whether the
// queue actually reaches the updater, or whether anything is announced at
// all.

// A patch within duva.auto goes all the way through without anyone asked.
func TestV5AppliesWhatPolicyAllows(t *testing.T) {
	s := Up(t, "duva-v5/within-policy")
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

	t.Run("the relative bind survived the recreate", func(t *testing.T) {
		// The reason the project is mounted at its own path. The daemon
		// resolves a bind's source against the HOST, so the replacement
		// container only carries ./payload if the updater read the project
		// from a path the host also has -- from /compose it would have asked
		// for /compose/payload, which exists nowhere.
		// Asked of the daemon rather than of the container: the test image is
		// FROM scratch and has no shell to exec into.
		out, err := exec.Command("docker", "inspect", "app",
			"--format", "{{range .Mounts}}{{.Source}}->{{.Destination}} {{end}}").CombinedOutput()
		if err != nil {
			t.Fatalf("inspecting the replacement: %v\n%s", err, out)
		}
		got := strings.TrimSpace(string(out))
		if !strings.Contains(got, "/payload") {
			t.Fatalf("the replacement lost its bind:\n  got %s", got)
		}
		// And the source is a path the HOST has, not one inside the updater.
		if strings.Contains(got, "/compose/payload") {
			t.Errorf("the bind points inside the updater, where the daemon cannot follow it:\n  got %s", got)
		}
	})

	t.Run("nothing is left waiting for a person", func(t *testing.T) {
		if got := s.Queued(); len(got) != 0 {
			t.Errorf("queue is %v, want empty -- policy allowed this one", got)
		}
	})
}

// A soak holds back what policy allows until the release is old enough, and
// the queue's own clock -- not another notice -- lets it go.
//
// The watcher reports each release once, so if the reconciler did not
// release a soaked entry nothing else ever would. That is the path the unit
// tests fake the clock for, and this is where it is driven by a real one.
func TestV5ASoakHoldsBackWhatPolicyAllows(t *testing.T) {
	s := Up(t, "duva-v5/soak")
	// Pushed fresh rather than through PushUpdates. A soak counts from when
	// an image was built, and an identical build is served from the cache
	// with the Created time of whichever earlier run built it first -- so a
	// second run found a release minutes old, older than the short soak, and
	// applied it at once. Correctly: that is what a soak counted from
	// publication does. Content unique to this run makes the release new.
	run := fmt.Sprint(time.Now().UnixNano())
	for _, repo := range []string{"soak-held", "soak-released"} {
		s.Push(repo, "1.0.1", "1.0.1 "+run)
	}
	s.Pin("held", "released")
	s.Start()

	t.Run("both are found and soaking, not applied", func(t *testing.T) {
		for _, service := range []string{"held", "released"} {
			row := s.Pending(service)
			if row.ReleaseAt == "" {
				t.Errorf("%s is not soaking: %+v\n%s", service, row, s.Logs("duva-queue"))
			}
			if !strings.Contains(row.Why, "soaking until") {
				t.Errorf("%s's row does not say it is soaking: %q", service, row.Why)
			}
		}
		if got := s.RunningImage("soak-held"); strings.Contains(got, "1.0.1") {
			t.Errorf("held was applied before its soak was over:\n  got %s", got)
		}
	})

	t.Run("the short soak is released by the clock", func(t *testing.T) {
		s.waitFor("soak-released to run 1.0.1", 3*time.Minute,
			func() bool { return strings.Contains(s.RunningImage("soak-released"), "1.0.1") },
			func() string { return s.Logs("duva-queue") + "\n--- duva-update\n" + s.Logs("duva-update") })
		if !strings.Contains(s.Logs("duva-queue"), "released: soak is over, applying 1.0.0 -> 1.0.1") {
			t.Errorf("applied, but not by the soak ending:\n%s", s.Logs("duva-queue"))
		}
		s.waitFor("released to leave the queue", 30*time.Second,
			func() bool { return s.Pending("released").Candidate == "" },
			func() string { return s.Logs("duva-queue") })
	})

	t.Run("and committed like any auto-apply", func(t *testing.T) {
		if got := s.Log(); !strings.Contains(got, "pin released to 1.0.1") {
			t.Errorf("the change was not committed under the template:\n  got %s", got)
		}
	})

	t.Run("the long soak is still waiting", func(t *testing.T) {
		if got := s.RunningImage("soak-held"); strings.Contains(got, "1.0.1") {
			t.Errorf("held was applied before its soak was over:\n  got %s", got)
		}
		if row := s.Pending("held"); row.ReleaseAt == "" {
			t.Errorf("held is no longer soaking: %+v", row)
		}
	})

	t.Run("nobody was asked about either", func(t *testing.T) {
		// Soaking is policy working as configured, like an auto-apply: there
		// is nothing for a person to do.
		if got := s.Notifications(); len(got) != 0 {
			t.Errorf("announced:\n  %v", got)
		}
	})
}

// An auto-apply that fails tells someone, and a successful one does not.
//
// The half that was silent: policy applied something without asking, it did
// not finish, and nothing said so -- while the container may be down and the
// repository dirty. The unit tests cover each piece and nothing covered the
// path between them, which is what this is for.
func TestV5AFailedAutoApplyIsAnnounced(t *testing.T) {
	s := Up(t, "duva-v5/failed-apply")
	// The unrunnable 1.0.1 comes from the scenario's manifest, so the sandbox
	// stands up the same world this asserts on.
	s.PushUpdates()
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
		// The service, as the notification titles it. Not a bare "app"
		// substring: this fixture's repository is broken-app, so that matches
		// the body too and would pass on a notification that never named the
		// service at all.
		if !strings.Contains(failure, "app on integration") {
			t.Errorf("the announcement does not name the service and host:\n  %s", failure)
		}
	})

	t.Run("the container is not running", func(t *testing.T) {
		// Not which image it was created with -- docker reports that whether
		// or not it ever ran, which is what made the first version of this
		// assertion wrong. Whether it is up is the question.
		// By container name, via the daemon: this fixture's container_name
		// differs from its service name, and compose ps only knows services.
		// Asked of compose, an unknown name answers "nothing running" -- which
		// is what this asserts, so it passed without observing anything.
		if s.runningContainer("broken-app") {
			t.Error("the container is running, so nothing failed to start and this tests nothing")
		}
	})

	t.Run("and the repository is left dirty", func(t *testing.T) {
		// The pin was written before the recreate, so a failed recreate
		// leaves the file recording a change that did not take. Deliberate:
		// it is what blocks the next apply on this host until a person looks,
		// and the updater's own failure message says so.
		if got := s.Status(); got == "" {
			t.Error("the repository is clean, so the pin was undone -- the rollback this deliberately does not do")
		}
	})
}

// The updater updating itself: handed to a helper, which replaces it.
//
// Done in place, recreating stops the old container first, and the old
// container is the one recreating -- so it stops partway and the host is left
// with no updater. That is what happened on both hosts on 2026-10-14, and no
// unit test can show it: it is a property of the daemon, not of this code.
func TestV5TheUpdaterUpdatesItself(t *testing.T) {
	s := Up(t, "duva-v5/self-update")
	s.Pin("duva-update")
	// Published only once the stack is up. Start's `compose up` waits on
	// duva-update as a dependency, and an update found during it replaces
	// that container under compose -- which reads the old one's exit as the
	// bring-up failing. Nothing in production is mid-`up` when duva applies.
	s.Start()
	s.PushUpdates()
	s.CheckNowIn("duva-watch")

	s.waitFor("the self-update to be committed", 2*time.Minute,
		func() bool { return strings.Contains(s.Log(), "pin duva-update to 1.0.1") },
		func() string {
			return s.Log() + "\n--- duva-watch\n" + s.Logs("duva-watch") +
				"\n--- duva-queue\n" + s.Logs("duva-queue") +
				"\n--- duva-update\n" + s.Logs("duva-update")
		})

	t.Run("the updater runs the new image", func(t *testing.T) {
		if got := s.RunningImage("duva-update"); !strings.Contains(got, "1.0.1") {
			t.Errorf("duva-update was not replaced:\n  got %s", got)
		}
		if !s.runningContainer("duva-update") {
			t.Error("duva-update is not running: the host has no updater")
		}
	})

	t.Run("the queue was told it completed", func(t *testing.T) {
		// The queue followed the helper's stream, so its outcome is the
		// helper's -- not "lost the actor's stream", which is what reading
		// from the container being replaced looks like.
		logs := s.Logs("duva-queue")
		if !strings.Contains(logs, "duva-update: 1.0.0 -> 1.0.1 applied") {
			t.Errorf("the queue did not record the update as applied:\n%s", logs)
		}
		if strings.Contains(logs, "lost the actor's stream") {
			t.Errorf("the queue lost the stream:\n%s", logs)
		}
	})

	t.Run("the helper is gone", func(t *testing.T) {
		// It removes itself on success, after the queue has read the end.
		s.waitFor("the helper to remove itself", 30*time.Second,
			func() bool {
				out, _ := exec.Command("docker", "ps", "-aq",
					"--filter", "label=duva.self-update").Output()
				return strings.TrimSpace(string(out)) == ""
			},
			func() string {
				out, _ := exec.Command("docker", "ps", "-a",
					"--filter", "label=duva.self-update").CombinedOutput()
				return string(out)
			})
	})

	t.Run("and the repository is clean", func(t *testing.T) {
		if got := s.Status(); got != "" {
			t.Errorf("the repository is dirty after a successful self-update:\n%s", got)
		}
	})
}
