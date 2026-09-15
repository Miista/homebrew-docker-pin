package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/compose"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/actor"
)

// world is a compose project plus fakes for docker and git, so a transaction
// can be run without a daemon or a repository. What it cannot show is whether
// the container really ends up running what was pinned; that needs an
// integration suite, and duva's already has one.
type world struct {
	file string

	pulled    []string
	recreated []string
	committed []string
	pushed    int
	added     []string

	pullErr     error
	recreateErr error
	clean       bool
	cleanErr    error
	digest      string
}

func newWorld(t *testing.T, composeContent string) *world {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(composeContent), 0o644); err != nil {
		t.Fatal(err)
	}
	return &world{file: file, clean: true, digest: "sha256:newdigest"}
}

func (w *world) docker() Docker {
	return Docker{
		Pull: func(ref string) error {
			w.pulled = append(w.pulled, ref)
			return w.pullErr
		},
		GetDigest: func(ref string) (string, error) { return w.digest, nil },
		Recreate: func(file, service string) error {
			w.recreated = append(w.recreated, service)
			return w.recreateErr
		},
		ContainerName: func(service string) string { return "the-" + service },
	}
}

func (w *world) git() Git {
	return Git{
		Add:         func(dir, file string) error { w.added = append(w.added, file); return nil },
		Commit:      func(dir, msg string) error { w.committed = append(w.committed, msg); return nil },
		PullRebase:  func(dir string) error { return nil },
		RebaseAbort: func(dir string) error { return nil },
		Push:        func(dir string) error { w.pushed++; return nil },
		IsClean:     func(dir string) (bool, error) { return w.clean, w.cleanErr },
	}
}

func (w *world) image(t *testing.T) string {
	t.Helper()
	raw, err := compose.RawImage(w.file, "app")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

const versionPin = `
services:
  app:
    image: example.com/app:1.0.0@sha256:olddigest
    container_name: the-app
`

func versionRequest() actor.Request {
	return actor.Request{
		Service: "app",
		File:    "", // filled in per test
		Image:   "example.com/app",
		From:    "1.0.0",
		To:      "1.1.0",
		Tag:     "1.1.0",
	}
}

// steps collects what a transaction reported, so a test can assert on what
// an operator would have seen rather than only on the outcome.
func steps() (func(string, ...any), *[]string) {
	var lines []string
	return func(format string, args ...any) {
		if len(args) > 0 {
			lines = append(lines, fmt.Sprintf(format, args...))
			return
		}
		lines = append(lines, format)
	}, &lines
}

// --- the happy path ----------------------------------------------------------

func TestVersionChangeIsAppliedAndCommitted(t *testing.T) {
	w := newWorld(t, versionPin)
	req := versionRequest()
	req.File = w.file
	step, lines := steps()

	status, reason := transaction(req, step, w.docker(), w.git(), false, "")
	if status != actor.Completed {
		t.Fatalf("status = %q (%s)\n%v", status, reason, *lines)
	}

	if len(w.pulled) != 1 || w.pulled[0] != "example.com/app:1.1.0" {
		t.Errorf("pulled %v, want the new tag", w.pulled)
	}
	if got := w.image(t); !strings.Contains(got, ":1.1.0@sha256:newdigest") {
		t.Errorf("the compose file was not repinned: %s", got)
	}
	if len(w.recreated) != 1 {
		t.Errorf("recreated %v, want one", w.recreated)
	}
	if len(w.committed) != 1 || !strings.Contains(w.committed[0], "1.0.0 -> 1.1.0") {
		t.Errorf("committed %v, want a readable subject", w.committed)
	}
}

// A digest move pulls the digest, not the tag: the tag has not changed, and
// pulling it again would fetch whatever it points at now rather than what was
// decided about.
func TestDigestMovePullsTheDigest(t *testing.T) {
	w := newWorld(t, `
services:
  app:
    image: example.com/app:latest@sha256:olddigest
    container_name: the-app
`)
	req := actor.Request{
		Service: "app", File: w.file, Image: "example.com/app",
		From: "sha256:olddigest", To: "sha256:newdigest",
		Tag: "latest", Digest: "sha256:newdigest",
	}
	step, _ := steps()

	status, reason := transaction(req, step, w.docker(), w.git(), false, "")
	if status != actor.Completed {
		t.Fatalf("status = %q (%s)", status, reason)
	}
	if len(w.pulled) != 1 || w.pulled[0] != "example.com/app@sha256:newdigest" {
		t.Errorf("pulled %v, want the digest", w.pulled)
	}
	if got := w.image(t); !strings.Contains(got, "latest@sha256:newdigest") {
		t.Errorf("image = %s, want the tag kept and the digest moved", got)
	}
}

// --- being told twice --------------------------------------------------------

// The protocol re-notifies until something is done, so being told twice must
// produce one commit rather than two.
func TestBeingToldTwiceIsIdempotent(t *testing.T) {
	w := newWorld(t, versionPin)
	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	transaction(req, step, w.docker(), w.git(), false, "")
	firstCommits := len(w.committed)

	status, _ := transaction(req, step, w.docker(), w.git(), false, "")
	if status != actor.Completed {
		t.Errorf("the second attempt reported %q, want completed", status)
	}
	if len(w.committed) != firstCommits {
		t.Errorf("committed again: %v", w.committed)
	}
	if len(w.recreated) != 1 {
		t.Errorf("recreated twice: %v", w.recreated)
	}
}

// --- failures ----------------------------------------------------------------

// The cheapest failure is the one before anything is written.
func TestAFailedPullChangesNothing(t *testing.T) {
	w := newWorld(t, versionPin)
	w.pullErr = errors.New("no such image")
	req := versionRequest()
	req.File = w.file
	before := w.image(t)
	step, _ := steps()

	status, reason := transaction(req, step, w.docker(), w.git(), false, "")
	if status != actor.Failed {
		t.Errorf("status = %q, want failed", status)
	}
	if !strings.Contains(reason, "no such image") {
		t.Errorf("reason = %q", reason)
	}
	if w.image(t) != before {
		t.Error("the compose file was written despite the pull failing")
	}
	if len(w.recreated) != 0 {
		t.Error("it recreated a container after a failed pull")
	}
}

// The one rollback: if the container refuses the image, the file must not
// keep claiming it.
func TestARefusedImagePutsTheFileBack(t *testing.T) {
	w := newWorld(t, versionPin)
	w.recreateErr = errors.New("the container would not start")
	req := versionRequest()
	req.File = w.file
	before := w.image(t)
	step, lines := steps()

	status, reason := transaction(req, step, w.docker(), w.git(), false, "")
	if status != actor.Failed {
		t.Errorf("status = %q, want failed", status)
	}
	if !strings.Contains(reason, "would not start") {
		t.Errorf("reason = %q", reason)
	}
	if got := w.image(t); got != before {
		t.Errorf("the file was left claiming an image that never ran:\n  got  %s\n  want %s", got, before)
	}
	if len(w.committed) != 0 {
		t.Errorf("it committed a failed update: %v", w.committed)
	}
	if !containsLine(*lines, "putting the compose file back") {
		t.Errorf("the revert was not reported:\n%v", *lines)
	}
}

// Committing on top of someone's half-finished edit is never wanted.
func TestADirtyRepositoryRefusesBeforeAnythingHappens(t *testing.T) {
	w := newWorld(t, versionPin)
	w.clean = false
	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	status, reason := transaction(req, step, w.docker(), w.git(), false, "")
	if status != actor.Failed {
		t.Errorf("status = %q, want failed", status)
	}
	if !strings.Contains(reason, "uncommitted") {
		t.Errorf("reason = %q", reason)
	}
	if len(w.pulled) != 0 || len(w.recreated) != 0 {
		t.Error("it started work on a dirty repository")
	}
}

// A locked index is someone else committing, not a failure to report as one.
func TestABusyRepositoryIsToldToTryAgain(t *testing.T) {
	w := newWorld(t, versionPin)
	w.cleanErr = errRepoBusy
	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	status, reason := transaction(req, step, w.docker(), w.git(), false, "")
	if status != actor.Failed {
		t.Errorf("status = %q", status)
	}
	if !strings.Contains(reason, "busy") || !strings.Contains(reason, "again") {
		t.Errorf("reason = %q, want it to say it will be retried", reason)
	}
}

// Everything after the container is record-keeping: a commit that fails does
// not make the update untrue, so the status stays completed.
func TestAFailedCommitStillCompletes(t *testing.T) {
	w := newWorld(t, versionPin)
	g := w.git()
	g.Commit = func(dir, msg string) error { return errors.New("no identity") }
	req := versionRequest()
	req.File = w.file
	step, lines := steps()

	status, _ := transaction(req, step, w.docker(), g, false, "")
	if status != actor.Completed {
		t.Errorf("status = %q, want completed -- the container is running the new image", status)
	}
	if !containsLine(*lines, "could not be committed") {
		t.Errorf("the failure was not reported:\n%v", *lines)
	}
}

// --- pushing -----------------------------------------------------------------

func TestPushIsOffByDefault(t *testing.T) {
	w := newWorld(t, versionPin)
	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	transaction(req, step, w.docker(), w.git(), false, "")
	if w.pushed != 0 {
		t.Errorf("pushed %d times with push off", w.pushed)
	}
}

func TestPushRebasesFirst(t *testing.T) {
	w := newWorld(t, versionPin)
	var order []string
	g := w.git()
	g.PullRebase = func(dir string) error { order = append(order, "rebase"); return nil }
	g.Push = func(dir string) error { order = append(order, "push"); return nil }

	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	transaction(req, step, w.docker(), g, true, "")
	if len(order) != 2 || order[0] != "rebase" || order[1] != "push" {
		t.Errorf("order = %v, want rebase then push", order)
	}
}

// A conflicted rebase is never this process's to resolve.
func TestAConflictedRebaseIsAbortedAndNotPushed(t *testing.T) {
	w := newWorld(t, versionPin)
	aborted := false
	g := w.git()
	g.PullRebase = func(dir string) error { return errors.New("conflict") }
	g.RebaseAbort = func(dir string) error { aborted = true; return nil }

	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	status, _ := transaction(req, step, w.docker(), g, true, "")
	if status != actor.Completed {
		t.Errorf("status = %q, want completed -- the update happened", status)
	}
	if !aborted {
		t.Error("the rebase was left conflicted")
	}
	if w.pushed != 0 {
		t.Error("it pushed on top of a conflicted rebase")
	}
}

// A commit that did not reach the remote rides along with the next push.
func TestAFailedPushStillCompletes(t *testing.T) {
	w := newWorld(t, versionPin)
	g := w.git()
	g.Push = func(dir string) error { return errors.New("no remote") }

	req := versionRequest()
	req.File = w.file
	step, lines := steps()

	status, _ := transaction(req, step, w.docker(), g, true, "")
	if status != actor.Completed {
		t.Errorf("status = %q, want completed", status)
	}
	if !containsLine(*lines, "not pushed") {
		t.Errorf("the failure was not reported:\n%v", *lines)
	}
}

// --- references --------------------------------------------------------------

func TestCandidateRef(t *testing.T) {
	tag := actor.Request{Image: "example.com/app", Tag: "1.2.3"}
	if got := candidateRef(tag); got != "example.com/app:1.2.3" {
		t.Errorf("got %q", got)
	}
	digest := actor.Request{Image: "example.com/app", Tag: "latest", Digest: "sha256:abc"}
	if got := candidateRef(digest); got != "example.com/app@sha256:abc" {
		t.Errorf("got %q", got)
	}
}

func TestDisplayOfShortensADigest(t *testing.T) {
	long := "sha256:0123456789abcdef0123456789abcdef"
	if got := displayOf(long); len(got) != 19 {
		t.Errorf("displayOf(%q) = %q", long, got)
	}
	if got := displayOf("1.2.3"); got != "1.2.3" {
		t.Errorf("a version should be left alone, got %q", got)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// --- nothing after the container is silent -----------------------------------
//
// The container is running the new image, so the status is Completed -- but a
// bare Completed is what let bazarr's first real apply leave a staged,
// uncommitted change with no trace anywhere. Every one of these must carry a
// reason, because the reason is what reaches the log.

func TestGitFailuresAreNeverSilent(t *testing.T) {
	boom := errors.New("the hook said no")

	for _, tc := range []struct {
		name   string
		break_ func(g *Git)
		want   string
	}{
		{"staging fails", func(g *Git) {
			g.Add = func(string, string) error { return boom }
		}, "could not be staged"},
		{"committing fails", func(g *Git) {
			g.Commit = func(string, string) error { return boom }
		}, "could not be committed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, versionPin)
			req := versionRequest()
			req.File = w.file
			step, lines := steps()

			g := w.git()
			tc.break_(&g)
			status, reason := transaction(req, step, w.docker(), g, false, "")

			// The update is true: the container was replaced.
			if status != actor.Completed {
				t.Errorf("status = %q, want completed -- the container was replaced", status)
			}
			if len(w.recreated) != 1 {
				t.Errorf("recreated %v, want the one service", w.recreated)
			}
			// But it must say what did not finish.
			if reason == "" {
				t.Fatal("completed with no reason: this is the silence that hid a half-done apply")
			}
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.want)
			}
			// And whoever was watching saw the same thing.
			if joined := strings.Join(*lines, "\n"); !strings.Contains(joined, tc.want) {
				t.Errorf("the stream never mentioned it:\n%s", joined)
			}
		})
	}
}

// The positive: a clean run says nothing, so a reason always means something
// went wrong rather than being noise on every apply.
func TestACleanApplyCarriesNoReason(t *testing.T) {
	w := newWorld(t, versionPin)
	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	status, reason := transaction(req, step, w.docker(), w.git(), false, "")
	if status != actor.Completed || reason != "" {
		t.Errorf("a clean apply reported %q / %q, want completed with nothing to say", status, reason)
	}
}

// A refused commit says plainly that the host is now blocked.
//
// Not unstaged: `git restore --staged` turns "M " into " M", which
// git status --porcelain still reports, so IsClean still refuses. An earlier
// version did that and it read like a safeguard while fixing nothing.
func TestARefusedCommitSaysTheHostIsBlocked(t *testing.T) {
	w := newWorld(t, versionPin)
	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	g := w.git()
	g.Commit = func(string, string) error { return errors.New("the hook said no") }

	status, reason := transaction(req, step, w.docker(), g, false, "")

	if status != actor.Completed {
		t.Errorf("status = %q, want completed -- the container was replaced", status)
	}
	if !strings.Contains(reason, "until that is resolved") {
		t.Errorf("reason = %q, want it to say the host is blocked until someone resolves it", reason)
	}
	// The file still records the update: nothing about this is a rollback.
	if raw, _ := os.ReadFile(w.file); !strings.Contains(string(raw), w.digest) {
		t.Error("the file was reverted; a failed commit must not undo the update")
	}
}

// --- do not start what cannot finish -----------------------------------------

// A service with no container must be refused before anything happens. It
// used to fail inside Recreate -- after a pull and after the pin was written
// -- leaving the compose file claiming an image that nothing runs.
func TestAMissingContainerIsRefusedBeforeAnythingHappens(t *testing.T) {
	w := newWorld(t, versionPin)
	req := versionRequest()
	req.File = w.file
	step, _ := steps()

	before, _ := os.ReadFile(w.file)

	d := w.docker()
	d.Exists = func(service string) error { return errors.New("no such container") }

	status, reason := transaction(req, step, d, w.git(), false, "")

	if status != actor.Failed {
		t.Errorf("status = %q, want failed", status)
	}
	if !strings.Contains(reason, "no container to replace") {
		t.Errorf("reason = %q, want it to say there is nothing to replace", reason)
	}
	// Nothing was done: no pull, no write, no recreate.
	if len(w.pulled) != 0 {
		t.Errorf("pulled %v; a refusal must come before the pull", w.pulled)
	}
	if len(w.recreated) != 0 {
		t.Errorf("recreated %v; nothing should have been touched", w.recreated)
	}
	after, _ := os.ReadFile(w.file)
	if string(before) != string(after) {
		t.Error("the compose file was changed by a run that should not have started")
	}
}

// Same for a commit message the repository's hook would reject: refused with
// nothing done, rather than discovered after the container was replaced.
func TestARejectableMessageIsRefusedBeforeAnythingHappens(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(versionPin), 0o644); err != nil {
		t.Fatal(err)
	}
	// A repository whose hook demands a prefix the default template lacks.
	exec.Command("git", "-C", dir, "init", "-q").Run()
	hooks := filepath.Join(dir, ".git", "hooks")
	os.MkdirAll(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, "commit-msg"),
		[]byte("#!/bin/sh\ngrep -q '^[a-z]*/' \"$1\" || exit 1\n"), 0o755)

	w := &world{file: file, clean: true, digest: "sha256:newdigest"}
	req := versionRequest()
	req.File = file
	step, _ := steps()

	status, reason := transaction(req, step, w.docker(), w.git(), false, "")

	if status != actor.Failed {
		t.Errorf("status = %q, want failed", status)
	}
	if !strings.Contains(reason, "commit-msg") {
		t.Errorf("reason = %q, want it to name the hook", reason)
	}
	if len(w.pulled) != 0 || len(w.recreated) != 0 {
		t.Errorf("work started anyway: pulled=%v recreated=%v", w.pulled, w.recreated)
	}
}
