package updater

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/update"
)

// Against real git, like git_test.go: what these hold is what git does with the
// commands isolate.go builds, which a fake says nothing about.

const sharedFile = `# the media stack
x-common: &common
  environment:
    TZ: Europe/Copenhagen
services:
  radarr:
    <<: *common
    image: example.com/radarr:1.0.0@sha256:old
    env_file: radarr.env
    volumes:
      - ./radarr:/config
  bookshelf:
    image: example.com/bookshelf:1@sha256:bookold
    restart: unless-stopped
`

// sharedRepo is a repository with one subdirectory holding a compose file with
// two services, committed, as a host's checkout is. The file is returned as an
// absolute path, and dir is the directory the updater runs git in.
func sharedRepo(t *testing.T) (root, dir, file string) {
	t.Helper()
	_, root, dir, file = sharedRepoWithOrigin(t)
	return root, dir, file
}

// sharedRepoWithOrigin is sharedRepo with the bare origin the checkout pushes
// to, for what happens when git.push is on.
func sharedRepoWithOrigin(t *testing.T) (origin, root, dir, file string) {
	t.Helper()
	origin, root = repo(t)
	dir = filepath.Join(root, "media")
	file = filepath.Join(dir, "media.yml")
	write(t, file, sharedFile)
	write(t, filepath.Join(dir, "radarr.env"), "A=1\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "media")
	return origin, root, dir, file
}

func edit(t *testing.T, file, old, new string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%q is not in the file", old)
	}
	write(t, file, strings.Replace(string(b), old, new, 1))
}

func TestServiceUntouched(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, root, dir, file string)
		ok      bool
		why     string
	}{
		"an edit to another service": {
			prepare: func(t *testing.T, _, _, file string) {
				edit(t, file, "unless-stopped", "always")
			},
			ok: true,
		},
		"another service's image": {
			prepare: func(t *testing.T, _, _, file string) {
				edit(t, file, "bookshelf:1@sha256:bookold", "ghcr.io/miista/bookshelf:0.1.1")
			},
			ok: true,
		},
		"only a comment": {
			prepare: func(t *testing.T, _, _, file string) {
				edit(t, file, "# the media stack", "# the media stack, edited")
			},
			ok: true,
		},
		"a new service": {
			prepare: func(t *testing.T, _, _, file string) {
				edit(t, file, "  bookshelf:", "  third:\n    image: example.com/third\n  bookshelf:")
			},
			ok: true,
		},
		"its own image line": {
			prepare: func(t *testing.T, _, _, file string) {
				edit(t, file, "radarr:1.0.0@sha256:old", "radarr:2.0.0")
			},
			why: "image line",
		},
		"its own volumes": {
			prepare: func(t *testing.T, _, _, file string) {
				edit(t, file, "./radarr:/config", "./radarr:/config\n      - ./x:/x")
			},
			why: "has an edit",
		},
		"the anchor it borrows from": {
			prepare: func(t *testing.T, _, _, file string) {
				edit(t, file, "Europe/Copenhagen", "UTC")
			},
			why: "has an edit",
		},
		"its env_file": {
			prepare: func(t *testing.T, _, dir, _ string) {
				write(t, filepath.Join(dir, "radarr.env"), "A=2\n")
			},
			why: "radarr.env",
		},
		"something staged in the file": {
			prepare: func(t *testing.T, root, _, file string) {
				edit(t, file, "unless-stopped", "always")
				git(t, root, "add", file)
			},
			why: "staged",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, dir, file := sharedRepo(t)
			tc.prepare(t, root, dir, file)
			ok, why, err := serviceUntouched(dir, file, "radarr")
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.ok {
				t.Fatalf("ok = %v (%q), want %v", ok, why, tc.ok)
			}
			if !strings.Contains(why, tc.why) {
				t.Errorf("why = %q, want it to mention %q", why, tc.why)
			}
		})
	}

	t.Run("a file git does not track", func(t *testing.T) {
		_, dir, _ := sharedRepo(t)
		fresh := filepath.Join(dir, "new.yml")
		write(t, fresh, "services:\n  radarr:\n    image: x\n")
		ok, why, err := serviceUntouched(dir, fresh, "radarr")
		if err != nil || ok || !strings.Contains(why, "not tracked") {
			t.Errorf("got %v %q %v", ok, why, err)
		}
	})

	t.Run("a service added since the last commit", func(t *testing.T) {
		_, dir, file := sharedRepo(t)
		edit(t, file, "  bookshelf:", "  newone:\n    image: example.com/newone\n  bookshelf:")
		ok, why, err := serviceUntouched(dir, file, "newone")
		if err != nil || ok || !strings.Contains(why, "not in the committed file") {
			t.Errorf("got %v %q %v", ok, why, err)
		}
	})

	t.Run("a file that does not parse is an error, not a yes", func(t *testing.T) {
		_, dir, file := sharedRepo(t)
		write(t, file, "services: [unclosed")
		if ok, _, err := serviceUntouched(dir, file, "radarr"); ok || err == nil {
			t.Errorf("ok = %v, err = %v: a file nobody can read must not be called untouched", ok, err)
		}
	})
}

// The whole point: the pin goes in alone, and what somebody was doing is left
// exactly as they had it.
func TestCommitPinOnly(t *testing.T) {
	root, dir, file := sharedRepo(t)
	// Somebody's edits: one to another service, one staged elsewhere, one to a
	// file nobody tracks.
	edit(t, file, "bookshelf:1@sha256:bookold", "ghcr.io/miista/bookshelf:0.1.1")
	other := filepath.Join(root, "b", "compose.yml")
	write(t, other, "b: staged by a person\n")
	git(t, root, "add", other)
	write(t, filepath.Join(root, "scratch.txt"), "x\n")
	// The updater has already written the pin to the working file by now.
	const pinned = "example.com/radarr:1.0.0@sha256:new"
	edit(t, file, "example.com/radarr:1.0.0@sha256:old", pinned)
	before := git(t, root, "rev-parse", "HEAD")

	if err := commitPinOnly(dir, file, "radarr", pinned, "radarr: 1.0.0 moved to sha256:new"); err != nil {
		t.Fatal(err)
	}

	t.Run("one new commit with the message", func(t *testing.T) {
		if got := git(t, root, "rev-parse", "HEAD~1"); got != before {
			t.Errorf("HEAD~1 = %s, want the commit it was made on top of", got)
		}
		if got := git(t, root, "log", "-1", "--format=%s"); got != "radarr: 1.0.0 moved to sha256:new" {
			t.Errorf("subject = %q", got)
		}
	})

	t.Run("it changes the pin line and nothing else", func(t *testing.T) {
		if got := git(t, root, "show", "--name-only", "--format=", "HEAD"); got != "media/media.yml" {
			t.Errorf("files = %q", got)
		}
		diff := git(t, root, "show", "-U0", "--format=", "HEAD")
		var changed []string
		for _, l := range strings.Split(diff, "\n") {
			if (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")) && !strings.HasPrefix(l, "+++") && !strings.HasPrefix(l, "---") {
				changed = append(changed, l)
			}
		}
		if len(changed) != 2 || !strings.Contains(changed[0], "sha256:old") || !strings.Contains(changed[1], "sha256:new") {
			t.Errorf("want one line out and one in, got %q", changed)
		}
		if strings.Contains(git(t, root, "show", "HEAD:media/media.yml"), "ghcr.io") {
			t.Error("somebody's edit to bookshelf was committed with the pin")
		}
	})

	t.Run("the working tree keeps both the pin and the edit", func(t *testing.T) {
		got, _ := os.ReadFile(file)
		if !strings.Contains(string(got), pinned) || !strings.Contains(string(got), "ghcr.io/miista/bookshelf:0.1.1") {
			t.Errorf("the file lost one of them:\n%s", got)
		}
	})

	t.Run("what is left uncommitted is the edit alone", func(t *testing.T) {
		// Their edit is a diff against the new HEAD; the pin is not part of it.
		diff := git(t, root, "diff", "-U0", "--", file)
		if !strings.Contains(diff, "bookshelf") || strings.Contains(diff, "radarr") {
			t.Errorf("the pin shows as uncommitted, or the edit is gone:\n%s", diff)
		}
		if got := git(t, root, "diff", "--cached", "--name-only"); got != "b/compose.yml" {
			t.Errorf("the index should hold only what the person staged, got %q", got)
		}
		if got := git(t, root, "status", "--porcelain"); !strings.Contains(got, "?? scratch.txt") {
			t.Errorf("the untracked file is gone from status:\n%s", got)
		}
	})

	t.Run("the next apply of radarr sees nothing in its way", func(t *testing.T) {
		ok, why, err := serviceUntouched(dir, file, "radarr")
		if err != nil || !ok {
			t.Errorf("the pin was committed, so radarr is untouched again: %v %q %v", ok, why, err)
		}
	})
}

func TestCommitPinOnly_KeepsTheFileMode(t *testing.T) {
	root, dir, file := sharedRepo(t)
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", file)
	git(t, root, "commit", "-q", "-m", "executable")
	edit(t, file, "unless-stopped", "always")
	edit(t, file, "sha256:old", "sha256:new")

	if err := commitPinOnly(dir, file, "radarr", "example.com/radarr:1.0.0@sha256:new", "m"); err != nil {
		t.Fatal(err)
	}
	if got := git(t, root, "ls-tree", "HEAD", "--", "media/media.yml"); !strings.HasPrefix(got, "100755") {
		t.Errorf("the mode changed: %s", got)
	}
}

// A repository's commit-msg hook is its convention, and the commit made here
// is subject to it like any other.
func TestCommitPinOnly_RunsTheCommitMsgHook(t *testing.T) {
	root, dir, file := sharedRepo(t)
	hook := filepath.Join(root, ".git", "hooks", "commit-msg")
	write(t, hook, "#!/bin/sh\ngrep -q '^media/' \"$1\" || { echo 'subject must start with media/'; exit 1; }\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	edit(t, file, "unless-stopped", "always")
	edit(t, file, "sha256:old", "sha256:new")
	before := git(t, root, "rev-parse", "HEAD")
	const pinned = "example.com/radarr:1.0.0@sha256:new"

	err := commitPinOnly(dir, file, "radarr", pinned, "radarr: no prefix")
	if err == nil || !strings.Contains(err.Error(), "subject must start with media/") {
		t.Fatalf("a message the hook refuses should fail with the hook's words, got %v", err)
	}
	if got := git(t, root, "rev-parse", "HEAD"); got != before {
		t.Error("a commit was made past the hook")
	}

	if err := commitPinOnly(dir, file, "radarr", pinned, "media/radarr: ok"); err != nil {
		t.Errorf("a message the hook accepts should commit: %v", err)
	}
}

func TestCommitPinOnly_Refusals(t *testing.T) {
	t.Run("a file git does not track", func(t *testing.T) {
		_, dir, _ := sharedRepo(t)
		fresh := filepath.Join(dir, "new.yml")
		write(t, fresh, "services:\n  radarr:\n    image: x\n")
		if err := commitPinOnly(dir, fresh, "radarr", "x:1", "m"); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("a service the committed file does not have", func(t *testing.T) {
		root, dir, file := sharedRepo(t)
		before := git(t, root, "rev-parse", "HEAD")
		if err := commitPinOnly(dir, file, "nope", "x:1", "m"); err == nil {
			t.Error("want an error")
		}
		if git(t, root, "rev-parse", "HEAD") != before {
			t.Error("a commit was made")
		}
	})
}

// A thin transaction on top: with the real seams, an apply to radarr while the
// file has an edit to bookshelf commits the pin alone.
func TestTransaction_RealGitAppliesAroundAnEditToAnotherService(t *testing.T) {
	root, _, file := sharedRepo(t)
	edit(t, file, "unless-stopped", "always")

	w := newWorld(t, "services:\n  app:\n    image: x\n")
	resolveFile = func(string) (string, error) { return file, nil }
	w.digest = "sha256:new"

	req := update.Request{
		Service: "radarr", Image: "example.com/radarr", Tag: "1.0.0",
		From: "sha256:old", To: "sha256:new", Digest: "sha256:new",
	}
	step, _ := steps()
	status, reason := transaction(req, step, w.docker(), realGit, false, "radarr: moved to {{.NewDigest}}")
	if status != update.Completed || reason != "" {
		t.Fatalf("status = %q, reason = %q", status, reason)
	}
	if got := git(t, root, "show", "--name-only", "--format=", "HEAD"); got != "media/media.yml" {
		t.Errorf("files = %q", got)
	}
	if got := git(t, root, "show", "HEAD:media/media.yml"); !strings.Contains(got, "sha256:new") || !strings.Contains(got, "unless-stopped") {
		t.Errorf("the commit should hold the pin and not the edit:\n%s", got)
	}
	if got := git(t, root, "status", "--porcelain"); got != "M media/media.yml" {
		t.Errorf("status = %q, want only the edit left", got)
	}
}

// The other direction: an edit to radarr itself waits, and nothing is touched.
func TestTransaction_RealGitWaitsForAnEditToTheServiceItself(t *testing.T) {
	root, _, file := sharedRepo(t)
	edit(t, file, "./radarr:/config", "./radarr:/config\n      - ./x:/x")

	w := newWorld(t, "services:\n  app:\n    image: x\n")
	resolveFile = func(string) (string, error) { return file, nil }

	req := update.Request{Service: "radarr", Image: "example.com/radarr", Tag: "1.0.0", Digest: "sha256:new", From: "sha256:old", To: "sha256:new"}
	step, _ := steps()
	before := git(t, root, "rev-parse", "HEAD")
	status, reason := transaction(req, step, w.docker(), realGit, false, "")
	if status != update.Deferred || !strings.Contains(reason, "radarr") || !strings.Contains(reason, "media.yml") {
		t.Errorf("status = %q, reason = %q: want it deferred, naming the file and the service", status, reason)
	}
	if len(w.pulled) != 0 || len(w.recreated) != 0 {
		t.Error("work was started on a service somebody is editing")
	}
	if git(t, root, "rev-parse", "HEAD") != before {
		t.Error("a commit was made")
	}
}

// --- the transaction's decisions, with git faked ---

// apart is a git whose file is dirty, with the two seams a dirty file needs.
func (w *world) apart(untouched bool, why string, pinned *[]string) Git {
	g := w.git()
	w.clean = false
	g.ServiceUntouched = func(dir, file, service string) (bool, string, error) { return untouched, why, nil }
	g.CommitPin = func(dir, file, service, newRaw, msg string) error {
		*pinned = append(*pinned, service+"|"+newRaw+"|"+msg)
		return nil
	}
	return g
}

func TestADirtyFileIsAppliedAroundWhenTheServiceIsUntouched(t *testing.T) {
	w := newWorld(t, versionPin)
	var pinned []string
	g := w.apart(true, "", &pinned)
	step, _ := steps()

	status, reason := transaction(versionRequest(), step, w.docker(), g, false, "")
	if status != update.Completed || reason != "" {
		t.Fatalf("status = %q, reason = %q", status, reason)
	}
	if len(w.recreated) != 1 {
		t.Errorf("recreated = %v", w.recreated)
	}
	if len(pinned) != 1 || !strings.HasPrefix(pinned[0], "app|example.com/app:1.1.0@sha256:newdigest|") {
		t.Errorf("the pin alone should be committed, got %v", pinned)
	}
	if len(w.added) != 0 || len(w.committed) != 0 {
		t.Errorf("the whole file was staged or committed on top of somebody's edit: added %v, committed %v", w.added, w.committed)
	}
}

// A clean file is committed the usual way: there is nobody's edit to keep out
// of the commit.
func TestACleanFileIsCommittedTheUsualWay(t *testing.T) {
	w := newWorld(t, versionPin)
	g := w.git()
	g.ServiceUntouched = func(dir, file, service string) (bool, string, error) { return true, "", nil }
	g.CommitPin = func(dir, file, service, newRaw, msg string) error {
		t.Error("committed the pin apart although the file is clean")
		return nil
	}
	step, _ := steps()
	if status, reason := transaction(versionRequest(), step, w.docker(), g, false, ""); status != update.Completed {
		t.Fatalf("status = %q, reason = %q", status, reason)
	}
	if len(w.added) != 1 || len(w.committed) != 1 {
		t.Errorf("a clean file is committed the usual way: added %v, committed %v", w.added, w.committed)
	}
}

// What the service reads is asked about even when the compose file is clean:
// an edit to an env_file does not make it dirty.
func TestACleanFileStillWaitsForWhatTheServiceReads(t *testing.T) {
	w := newWorld(t, versionPin)
	g := w.git()
	g.ServiceUntouched = func(dir, file, service string) (bool, string, error) {
		return false, "reads app.env, which has uncommitted changes", nil
	}
	g.CommitPin = func(dir, file, service, newRaw, msg string) error { return nil }
	step, _ := steps()
	status, reason := transaction(versionRequest(), step, w.docker(), g, false, "")
	if status != update.Deferred || !strings.Contains(reason, "app.env") {
		t.Errorf("status = %q, reason = %q", status, reason)
	}
	if len(w.pulled) != 0 || len(w.recreated) != 0 {
		t.Error("work was started")
	}
}

func TestADirtyFileWaitsWhenTheServiceIsInTheWay(t *testing.T) {
	for _, why := range []string{"has an edit", "has an edit to its image line", "reads app.env, which has uncommitted changes"} {
		t.Run(why, func(t *testing.T) {
			w := newWorld(t, versionPin)
			var pinned []string
			g := w.apart(false, why, &pinned)
			step, _ := steps()

			status, reason := transaction(versionRequest(), step, w.docker(), g, false, "")
			if status != update.Deferred {
				t.Fatalf("status = %q, want deferred", status)
			}
			for _, want := range []string{"docker-compose.yml", "app", why, "again"} {
				if !strings.Contains(reason, want) {
					t.Errorf("reason = %q, want it to mention %q", reason, want)
				}
			}
			if len(w.pulled) != 0 || len(w.recreated) != 0 || len(pinned) != 0 {
				t.Error("work was started on a service somebody is editing")
			}
			if got := w.image(t); !strings.Contains(got, "olddigest") {
				t.Errorf("the file was written: %s", got)
			}
		})
	}
}

func TestADirtyFileWithoutTheSeamsWaitsAsBefore(t *testing.T) {
	w := newWorld(t, versionPin)
	w.clean = false
	step, _ := steps()
	status, reason := transaction(versionRequest(), step, w.docker(), w.git(), false, "")
	if status != update.Deferred || !strings.Contains(reason, "uncommitted") {
		t.Errorf("status = %q, reason = %q", status, reason)
	}
}

func TestAnAnswerThatFailsIsNotAYes(t *testing.T) {
	t.Run("busy is told to try again", func(t *testing.T) {
		w := newWorld(t, versionPin)
		var pinned []string
		g := w.apart(true, "", &pinned)
		g.ServiceUntouched = func(dir, file, service string) (bool, string, error) { return true, "", errRepoBusy }
		step, _ := steps()
		status, reason := transaction(versionRequest(), step, w.docker(), g, false, "")
		if status != update.Deferred || !strings.Contains(reason, "busy") {
			t.Errorf("status = %q, reason = %q", status, reason)
		}
		if len(w.pulled) != 0 {
			t.Error("work was started")
		}
	})
	t.Run("another error fails before anything is done", func(t *testing.T) {
		w := newWorld(t, versionPin)
		var pinned []string
		g := w.apart(true, "", &pinned)
		g.ServiceUntouched = func(dir, file, service string) (bool, string, error) {
			return true, "", errors.New("git exploded")
		}
		step, _ := steps()
		status, reason := transaction(versionRequest(), step, w.docker(), g, false, "")
		if status != update.Failed || !strings.Contains(reason, "git exploded") {
			t.Errorf("status = %q, reason = %q", status, reason)
		}
		if len(w.pulled) != 0 || len(w.recreated) != 0 {
			t.Error("work was started on an answer that was an error")
		}
	})
}

// The commit that fails is the same story as before: the update is true, the
// record is not, and it is said so.
func TestAFailedPinCommitStillCompletesAndSaysSo(t *testing.T) {
	w := newWorld(t, versionPin)
	var pinned []string
	g := w.apart(true, "", &pinned)
	g.CommitPin = func(dir, file, service, newRaw, msg string) error { return errors.New("the hook said no") }
	step, _ := steps()
	status, reason := transaction(versionRequest(), step, w.docker(), g, false, "")
	if status != update.Completed || !strings.Contains(reason, "could not be committed") || !strings.Contains(reason, "the hook said no") {
		t.Errorf("status = %q, reason = %q", status, reason)
	}
}

func TestAPinCommittedApartIsPushedLikeAnyOther(t *testing.T) {
	w := newWorld(t, versionPin)
	var pinned []string
	g := w.apart(true, "", &pinned)
	pulled := 0
	g.PullRebase = func(dir string) error { pulled++; return nil }
	step, _ := steps()
	if status, reason := transaction(versionRequest(), step, w.docker(), g, true, ""); status != update.Completed || reason != "" {
		t.Fatalf("status = %q, reason = %q", status, reason)
	}
	if pulled != 1 || w.pushed != 1 {
		t.Errorf("pulled %d, pushed %d", pulled, w.pushed)
	}
}

// A failed recreate leaves the pin written and uncommitted. The next apply of
// that service must find its image line differs from HEAD and wait -- the
// block the design keeps -- while another service in the file carries on.
func TestAFailedRecreateStillBlocksItsOwnServiceOnly(t *testing.T) {
	root, _, file := sharedRepo(t)
	w := newWorld(t, "services:\n  app:\n    image: x\n")
	// After newWorld, which points the lookup at its own file.
	resolveFile = func(string) (string, error) { return file, nil }
	w.recreateErr = errors.New("would not start")
	req := update.Request{Service: "radarr", Image: "example.com/radarr", Tag: "1.0.0", Digest: "sha256:new", From: "sha256:old", To: "sha256:new"}
	step, _ := steps()
	if status, _ := transaction(req, step, w.docker(), realGit, false, ""); status != update.Failed {
		t.Fatalf("status = %q, want failed", status)
	}
	head := git(t, root, "rev-parse", "HEAD")

	t.Run("radarr waits", func(t *testing.T) {
		w.recreateErr = nil
		req.Digest, req.To = "sha256:newer", "sha256:newer"
		w.digest = "sha256:newer"
		status, reason := transaction(req, step, w.docker(), realGit, false, "")
		if status != update.Deferred || !strings.Contains(reason, "image line") {
			t.Errorf("status = %q, reason = %q", status, reason)
		}
	})

	t.Run("bookshelf carries on", func(t *testing.T) {
		w.recreateErr = nil
		w.digest = "sha256:bookshelfnew"
		other := update.Request{Service: "bookshelf", Image: "example.com/bookshelf", Tag: "1", Digest: "sha256:bookshelfnew", From: "sha256:bookold", To: "sha256:bookshelfnew"}
		status, reason := transaction(other, step, w.docker(), realGit, false, "")
		if status != update.Completed || reason != "" {
			t.Fatalf("status = %q, reason = %q", status, reason)
		}
		if git(t, root, "rev-parse", "HEAD") == head {
			t.Error("bookshelf's pin was not committed")
		}
		if got := git(t, root, "show", "HEAD:media/media.yml"); strings.Contains(got, "sha256:new\n") || !strings.Contains(got, "bookshelfnew") {
			t.Errorf("the commit should hold bookshelf's pin and not radarr's failed one:\n%s", got)
		}
	})
}

// --- the repository's index is locked by somebody else ---

func TestIsRepoBusy(t *testing.T) {
	for stderr, want := range map[string]bool{
		"fatal: Unable to create '/r/.git/index.lock': File exists.": true,
		"fatal: Another git process seems to be running":             true,
		"fatal: not a git repository":                                false,
		"":                                                           false,
	} {
		if got := isRepoBusy(stderr); got != want {
			t.Errorf("isRepoBusy(%q) = %v, want %v", stderr, got, want)
		}
	}
}

// pinned is a repository with radarr's pin written to the working file and an
// edit to bookshelf beside it, ready for commitPinOnly.
func pinned(t *testing.T) (root, dir, file string) {
	t.Helper()
	root, dir, file = sharedRepo(t)
	edit(t, file, "unless-stopped", "always")
	edit(t, file, "sha256:old", "sha256:new")
	return root, dir, file
}

const radarrPinned = "example.com/radarr:1.0.0@sha256:new"

// Somebody else's git command holds the index for a moment. The commit is made
// without the real index, so the lock only matters for the step that catches
// it up -- and that waits it out.
func TestCommitPinOnly_WaitsOutALockOnTheIndex(t *testing.T) {
	root, dir, file := pinned(t)
	lock := filepath.Join(root, ".git", "index.lock")
	write(t, lock, "")
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Remove(lock)
	}()

	if err := commitPinOnly(dir, file, "radarr", radarrPinned, "radarr: pinned"); err != nil {
		t.Fatalf("a lock that clears should not fail the commit: %v", err)
	}
	if got := git(t, root, "status", "--porcelain"); got != "M media/media.yml" {
		t.Errorf("the index should be level with HEAD, leaving only the edit: %q", got)
	}
}

// A lock that does not clear. The commit is there and the update is true; what
// is not done is bringing the index level, and the error says exactly that
// rather than that the commit failed.
func TestCommitPinOnly_SaysWhatIsLeftWhenTheLockStays(t *testing.T) {
	root, dir, file := pinned(t)
	prevRetries, prevWait := indexRetries, indexWait
	indexRetries, indexWait = 3, time.Millisecond
	t.Cleanup(func() { indexRetries, indexWait = prevRetries, prevWait })
	write(t, filepath.Join(root, ".git", "index.lock"), "")

	err := commitPinOnly(dir, file, "radarr", radarrPinned, "radarr: pinned")
	if !errors.Is(err, errIndexBehind) || !strings.Contains(err.Error(), "media/media.yml") {
		t.Fatalf("want errIndexBehind naming the file, got %v", err)
	}
	if got := git(t, root, "log", "-1", "--format=%s"); got != "radarr: pinned" {
		t.Errorf("the commit should exist: %q", got)
	}

	t.Run("and one `git reset` puts it right", func(t *testing.T) {
		os.Remove(filepath.Join(root, ".git", "index.lock"))
		git(t, root, "reset", "-q", "--", "media/media.yml")
		if got := git(t, root, "status", "--porcelain"); got != "M media/media.yml" {
			t.Errorf("status = %q", got)
		}
	})
}

func TestABehindIndexIsReportedAsCommittedNotAsFailed(t *testing.T) {
	w := newWorld(t, versionPin)
	var pinnedCalls []string
	g := w.apart(true, "", &pinnedCalls)
	g.CommitPin = func(dir, file, service, newRaw, msg string) error {
		return fmt.Errorf("%w: docker-compose.yml: fatal: index.lock", errIndexBehind)
	}
	step, lines := steps()
	status, reason := transaction(versionRequest(), step, w.docker(), g, true, "")
	if status != update.Completed {
		t.Errorf("status = %q, want completed: the update is live", status)
	}
	if !strings.Contains(reason, "committed") || !strings.Contains(reason, "git reset") || strings.Contains(reason, "could not be committed") {
		t.Errorf("reason = %q: it should say the commit is there and what is left", reason)
	}
	if w.pushed != 0 {
		t.Error("it pushed with the repository in a half-level state")
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "git reset") {
		t.Errorf("the stream never said what to do: %v", *lines)
	}
}

// --- git.push on, with a pin committed apart ---

// With push on, the pull before the push carries somebody's uncommitted edit
// across with --autostash when origin's new commits leave it alone.
func TestTransaction_RealGitPushesAPinCommittedApart(t *testing.T) {
	origin, root, _, file := sharedRepoWithOrigin(t)
	git(t, root, "push", "-q", "origin", "HEAD:main")
	pushFromElsewhere(t, origin, "b/compose.yml", "b: from another host\n")
	edit(t, file, "unless-stopped", "always")

	w := newWorld(t, "services:\n  app:\n    image: x\n")
	resolveFile = func(string) (string, error) { return file, nil }
	w.digest = "sha256:new"
	req := update.Request{Service: "radarr", Image: "example.com/radarr", Tag: "1.0.0", Digest: "sha256:new", From: "sha256:old", To: "sha256:new"}
	step, lines := steps()

	status, reason := transaction(req, step, w.docker(), realGit, true, "radarr: pinned")
	if status != update.Completed || reason != "" {
		t.Fatalf("status = %q, reason = %q\n%v", status, reason, *lines)
	}
	if got := git(t, origin, "log", "-1", "--format=%s", "main"); got != "radarr: pinned" {
		t.Errorf("origin's newest commit = %q: the pin was not pushed", got)
	}
	if got := git(t, origin, "show", "main:media/media.yml"); strings.Contains(got, "always") || !strings.Contains(got, "sha256:new") {
		t.Errorf("origin should have the pin and not the edit:\n%s", got)
	}
	if got := git(t, root, "log", "--format=%s", "-3"); !strings.Contains(got, "elsewhere") {
		t.Errorf("origin's commit from the other host should have been pulled in:\n%s", got)
	}
	if got := git(t, root, "status", "--porcelain"); got != "M media/media.yml" {
		t.Errorf("status = %q, want only the edit left", got)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "always") {
		t.Error("the edit is gone from the working file")
	}
}

// When origin changes the very file somebody is editing, nothing is pulled and
// nothing is pushed. The commit is local, the update is live, and the reason
// says what is left.
func TestTransaction_RealGitDoesNotPullOverTheFileBeingEdited(t *testing.T) {
	origin, root, _, file := sharedRepoWithOrigin(t)
	git(t, root, "push", "-q", "origin", "HEAD:main")
	pushFromElsewhere(t, origin, "media/media.yml", sharedFile+"# added elsewhere\n")
	edit(t, file, "unless-stopped", "always")

	w := newWorld(t, "services:\n  app:\n    image: x\n")
	resolveFile = func(string) (string, error) { return file, nil }
	w.digest = "sha256:new"
	req := update.Request{Service: "radarr", Image: "example.com/radarr", Tag: "1.0.0", Digest: "sha256:new", From: "sha256:old", To: "sha256:new"}
	step, _ := steps()

	status, reason := transaction(req, step, w.docker(), realGit, true, "radarr: pinned")
	if status != update.Completed || !strings.Contains(reason, "could not rebase before pushing") || !strings.Contains(reason, "media/media.yml") {
		t.Fatalf("status = %q, reason = %q", status, reason)
	}
	if got := git(t, root, "log", "-1", "--format=%s"); got != "radarr: pinned" {
		t.Errorf("the pin should be committed locally: %q", got)
	}
	if got := git(t, origin, "log", "-1", "--format=%s", "main"); got == "radarr: pinned" {
		t.Error("it pushed past a file somebody was editing")
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "always") {
		t.Error("the edit is gone from the working file")
	}
	if got := git(t, root, "status", "--porcelain"); got != "M media/media.yml" {
		t.Errorf("status = %q", got)
	}
}
