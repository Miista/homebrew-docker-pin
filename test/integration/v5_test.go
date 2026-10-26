//go:build integration

package integration

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
// One process now, so what this level shows is that the components are
// actually connected: that a finding reaches the queue, that the queue
// reaches the updater, and that anything is announced at all.

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

// An uncommitted edit blocks only the file it is in. Someone mid-edit in one
// compose file used to stop every apply on the host; here the edit is in
// another file, and one more is staged, and the apply goes through without
// committing either of them.
func TestV5AnEditElsewhereDoesNotBlockAnApply(t *testing.T) {
	s := Up(t, "duva-v5/within-policy")
	s.PushUpdates()
	s.Pin("app")
	s.writeFile(filepath.Join(s.Dir, "notes.md"), "notes\n")
	s.writeFile(filepath.Join(s.Dir, "staged.md"), "staged\n")
	s.Commit("notes")
	s.writeFile(filepath.Join(s.Dir, "notes.md"), "notes, half written\n")
	s.writeFile(filepath.Join(s.Dir, "staged.md"), "staged, not committed\n")
	s.git("add", "staged.md")
	s.Start()

	t.Run("the update is applied", func(t *testing.T) {
		if got := s.RunningImage("app"); !strings.Contains(got, "1.0.1") {
			t.Errorf("1.0.0 -> 1.0.1 was not applied:\n  got %s", got)
		}
	})

	t.Run("the commit holds only the compose file", func(t *testing.T) {
		if got := s.Log(); !strings.Contains(got, "pin app to 1.0.1") {
			t.Fatalf("the change was not committed:\n  got %s", got)
		}
		if got := s.git("show", "--name-only", "--format=", "HEAD"); got != "docker-compose.yml" {
			t.Errorf("the commit swept in more than its file:\n  got %s", got)
		}
	})

	t.Run("the edits are still there, uncommitted", func(t *testing.T) {
		// Status trims, so the first line loses its leading space.
		if got := "\n" + s.Status(); !strings.Contains(got, "M notes.md") || !strings.Contains(got, "\nM  staged.md") {
			t.Errorf("want notes.md modified and staged.md staged, got:\n%s", got)
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
				t.Errorf("%s is not soaking: %+v\n%s", service, row, s.Logs("duva"))
			}
			if !strings.Contains(row.Why, "delayed until") {
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
			func() string { return s.Logs("duva") })
		if !strings.Contains(s.Logs("duva"), "released: delay is over, applying 1.0.0 -> 1.0.1") {
			t.Errorf("applied, but not by the soak ending:\n%s", s.Logs("duva"))
		}
		s.waitFor("released to leave the queue", 30*time.Second,
			func() bool { return s.Pending("released").Candidate == "" },
			func() string { return s.Logs("duva") })
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

// What governs a service is the container it runs, not the file.
//
// A label edited and committed but never deployed has not taken effect: the
// container was started under the old one, and until it is recreated that is
// the policy it runs under. Reading the file would have applied a patch the
// running service never agreed to.
func TestV5PolicyComesFromTheRunningContainer(t *testing.T) {
	s := Up(t, "duva-v5/policy-from-container")
	s.PushUpdates()
	s.Pin("app")

	raw, err := os.ReadFile(filepath.Join(s.Dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// The label line, not the first mention: the fixture's header says
	// "duva.auto: none" too.
	edited := strings.Replace(string(raw), "      duva.auto: none\n", "      duva.auto: patch\n", 1)
	if edited == string(raw) {
		t.Fatal("the fixture has no duva.auto: none label to edit")
	}
	s.WriteCompose(edited)
	s.Commit("allow patches, without recreating app")
	s.Start()

	row := s.Pending("app")
	if row.Candidate != "1.0.1" {
		t.Fatalf("the patch was not queued: %+v\n%s", row, s.Logs("duva"))
	}
	if row.Auto != "none" {
		t.Errorf("decided under duva.auto: %s, want the container's none", row.Auto)
	}
	if got := s.RunningImage("policy-app"); strings.Contains(got, "1.0.1") {
		t.Errorf("applied under the file's policy:\n  got %s", got)
	}
}

// A check a person asks for says what it found and what became of it, which
// is what the page's toast reads. Start asks for one the way "Check…" does.
func TestV5AnAskedCheckSaysWhatBecameOfARelease(t *testing.T) {
	s := Up(t, "duva-v5/within-policy")
	s.PushUpdates()
	s.Pin("app")
	s.Start()

	run := s.CheckRun()
	if run.Error != "" || len(run.Failed) != 0 {
		t.Errorf("the check reports trouble: %+v", run)
	}
	if run.Checked == 0 {
		t.Errorf("the check says it looked at nothing: %+v", run)
	}
	if len(run.Findings) != 1 {
		t.Fatalf("findings = %+v, want the one patch", run.Findings)
	}
	if f := run.Findings[0]; f.Service != "app" || f.Label != "1.0.1" || f.Fate != "applying" {
		t.Errorf("finding = %+v, want app 1.0.1 applying", f)
	}
}

// What a person asked for is on their screen, so nobody is announced to as
// well: the patch is waiting for them, and the notifier says nothing. The gap
// this leaves is accepted -- a release found by an asked check is not news to
// the schedule afterwards.
func TestV5AnAskedCheckDoesNotAnnounce(t *testing.T) {
	s := Up(t, "duva-v5/policy-from-container")
	s.PushUpdates()
	s.Pin("app")
	s.Start()

	if row := s.Pending("app"); row.Candidate != "1.0.1" {
		t.Fatalf("the patch was not queued for a person: %+v\n%s", row, s.Logs("duva"))
	}
	run := s.CheckRun()
	if len(run.Findings) != 1 || run.Findings[0].Fate != "waiting" {
		t.Errorf("findings = %+v, want the patch waiting for a person", run.Findings)
	}
	if got := s.Notifications(); len(got) != 0 {
		t.Errorf("announced for a check somebody asked for:\n  %v", got)
	}
}

// A blocked release is never applied: not as found, and not when a rescan
// finds it again and its digest is only learned on pulling it.
func TestV5ABlockedReleaseIsNeverApplied(t *testing.T) {
	s := Up(t, "duva-v5/block")
	run := fmt.Sprint(time.Now().UnixNano())
	s.Push("block-app", "1.0.1", "1.0.1 "+run)
	s.Pin("app")
	s.Start()

	id := s.EntryID("app")
	if id == 0 {
		t.Fatalf("the patch was not queued to soak:\n%s", s.Logs("duva"))
	}
	s.API(fmt.Sprintf("/v1/block/%d", id), url.Values{"reason": {"missing migrations"}})

	t.Run("blocking settles what was waiting", func(t *testing.T) {
		if row := s.Pending("app"); row.Candidate != "" {
			t.Errorf("still waiting after the block: %+v", row)
		}
	})

	// Found again, it soaks again: a version change's digest is not known
	// until it is pulled, so the block is the updater's to catch. Found
	// again by looking further back than the last check, as Check… does.
	s.API("/v1/check/app", url.Values{"since": {"7d"}})
	s.waitFor("the updater to refuse the blocked release", 3*time.Minute,
		func() bool { return strings.Contains(s.Logs("duva"), "app: 1.0.0 -> 1.0.1 is blocked") },
		func() string { return s.Logs("duva") })

	t.Run("and it is not applied", func(t *testing.T) {
		if got := s.RunningImage("block-app"); strings.Contains(got, "1.0.1") {
			t.Errorf("a blocked release was applied:\n  got %s", got)
		}
		if got := s.Image("app"); strings.Contains(got, "1.0.1") {
			t.Errorf("the pin was written for a blocked release:\n  got %s", got)
		}
		if got := s.Status(); got != "" {
			t.Errorf("the repository is dirty after a refused apply:\n%s", got)
		}
	})

	t.Run("nobody is told what they decided", func(t *testing.T) {
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

// duva updating itself: handed to a helper, which replaces it.
//
// In v5 the container being replaced is all of duva -- watcher and queue
// included -- so the queue that started the apply does not live to see it
// end. The process that replaces it must learn the outcome and settle the
// entry, or the update stays queued forever after it happened.
func TestV5DuvaUpdatesItself(t *testing.T) {
	s := Up(t, "duva-v5/self-update")
	s.Pin("duva")
	// Published only once the stack is up: an update found during
	// `compose up` would replace duva under compose, which reads the old
	// container's exit as the bring-up failing.
	s.Start()
	s.PushUpdates()
	s.CheckNowIn("duva")

	s.waitFor("the self-update to be committed", 2*time.Minute,
		func() bool { return strings.Contains(s.Log(), "pin duva to 1.0.1") },
		func() string { return s.Log() + "\n--- duva\n" + s.Logs("duva") })

	t.Run("duva runs the new image", func(t *testing.T) {
		s.waitFor("duva to run 1.0.1", 30*time.Second,
			func() bool { return strings.Contains(s.RunningImage("duva"), "1.0.1") && s.runningContainer("duva") },
			func() string { return s.RunningImage("duva") + "\n" + s.Logs("duva") })
	})

	t.Run("the new duva settled the entry", func(t *testing.T) {
		s.waitFor("the entry to leave the queue", 30*time.Second,
			func() bool { return len(s.Queued()) == 0 },
			func() string { return s.Logs("duva") })
	})

	t.Run("the helper is gone", func(t *testing.T) {
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
