package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/compose"
	"github.com/Miista/homebrew-docker-pin/internal/fixture"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

// The transaction is where failures acquire meaning, so these are mostly
// about what state the world is left in when a step does not work. docker and
// git are faked because they leave the machine; the compose file is real,
// parsed and rewritten by the real code, because that is ours.

var errBoundary = errors.New("boundary failure")

// recorder is a working set of fakes plus a log of what was called, so a test
// can assert that a step was skipped as well as that one failed.
type recorder struct {
	docker Docker
	git    Git
	calls  []string
	// digest is what a pull is deemed to have produced.
	digest string
}

func newRecorder() *recorder {
	r := &recorder{digest: "sha256:" + strings.Repeat("a", 64)}
	r.docker = Docker{
		Pull: func(string) error { r.calls = append(r.calls, "pull"); return nil },
		GetDigest: func(string) (string, error) {
			r.calls = append(r.calls, "digest")
			return r.digest, nil
		},
		ComposeUp: func(string, string) error { r.calls = append(r.calls, "up"); return nil },
	}
	r.git = Git{
		Add:         func(string, string) error { r.calls = append(r.calls, "add"); return nil },
		Commit:      func(string, string) error { r.calls = append(r.calls, "commit"); return nil },
		PullRebase:  func(string) error { r.calls = append(r.calls, "rebase"); return nil },
		RebaseAbort: func(string) error { r.calls = append(r.calls, "abort"); return nil },
		Push:        func(string) error { r.calls = append(r.calls, "push"); return nil },
		IsClean:     func(string) (bool, error) { return true, nil },
	}
	return r
}

func (r *recorder) did(step string) bool {
	for _, c := range r.calls {
		if c == step {
			return true
		}
	}
	return false
}

// pending builds a project with one service that has an update available, and
// the finding describing it -- the state duva is in when it decides to act.
func pending(t *testing.T, f *fixture.Fixture) (watch.Finding, string) {
	t.Helper()
	svc := f.WithUpdate(f.PinnedService(), fixture.BumpPatch)
	file := f.Project(svc)
	return watch.Finding{
		Service:    svc.Name,
		File:       file,
		Image:      svc.Image,
		CurrentTag: svc.Tag,
		Kind:       watch.KindTag,
		Candidate:  svc.AvailableTags[len(svc.AvailableTags)-1],
		Status:     watch.StatusAvailable,
	}, file
}

func imageLine(t *testing.T, file, service string) string {
	t.Helper()
	raw, err := compose.RawImage(file, service)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The baseline every other test is measured against: with nothing broken, the
// update reaches the container and the change is recorded.
func TestApply_Success(t *testing.T) {
	f := fixture.New(t)
	finding, file := pending(t, f)
	r := newRecorder()

	res := apply(finding, r.docker, r.git, applyOptions{Host: "testhost", Push: true})

	if res.Err != nil {
		t.Fatalf("unexpected failure at %s: %v", res.FailedAt, res.Err)
	}
	if !res.Applied {
		t.Error("the update should be applied")
	}
	for _, step := range []string{"pull", "up", "add", "commit", "rebase", "push"} {
		if !r.did(step) {
			t.Errorf("step %q was skipped: %v", step, r.calls)
		}
	}
	if got := imageLine(t, file, finding.Service); !strings.Contains(got, "@"+r.digest) {
		t.Errorf("compose file not updated: %s", got)
	}
	if !strings.Contains(got(res), finding.Candidate) {
		t.Errorf("the outcome should name the new tag: %+v", res.Outcome)
	}
}

func got(r Result) string { return r.Outcome.NewRaw }

// Pull first, so the cheapest failure happens before anything is written.
func TestApply_PullFailure_WritesNothing(t *testing.T) {
	f := fixture.New(t)
	finding, file := pending(t, f)
	before := imageLine(t, file, finding.Service)

	r := newRecorder()
	r.docker.Pull = func(string) error { return errBoundary }

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h"})

	if res.FailedAt != StepPull || res.Err == nil {
		t.Fatalf("expected a pull failure, got %+v", res)
	}
	if res.Applied {
		t.Error("nothing should have been applied")
	}
	if imageLine(t, file, finding.Service) != before {
		t.Error("the compose file must be untouched when the pull failed")
	}
	if r.did("up") || r.did("commit") {
		t.Errorf("later steps should not have run: %v", r.calls)
	}
}

// The one case duva does undo: the container refused the new image, so the
// file must not keep claiming it.
func TestApply_RecreateFailure_RevertsTheFile(t *testing.T) {
	f := fixture.New(t)
	finding, file := pending(t, f)
	before := imageLine(t, file, finding.Service)

	r := newRecorder()
	r.docker.ComposeUp = func(string, string) error { return errBoundary }

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h"})

	if res.FailedAt != StepRecreate {
		t.Fatalf("expected a recreate failure, got %+v", res)
	}
	if !res.Reverted {
		t.Error("the file should have been put back")
	}
	if after := imageLine(t, file, finding.Service); after != before {
		t.Errorf("file not restored:\n before %s\n after  %s", before, after)
	}
	if r.did("commit") {
		t.Error("a failed update must not be committed")
	}
}

// A failure to restore is worse than the original problem, and must say so
// rather than reporting only that compose up failed.
func TestApply_RestoreFailureIsReported(t *testing.T) {
	f := fixture.New(t)
	finding, file := pending(t, f)

	r := newRecorder()
	// The write must succeed and only the restore fail, so the file becomes
	// unwritable at the moment compose up refuses the new image.
	r.docker.ComposeUp = func(string, string) error {
		if err := os.Chmod(file, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(file, 0o644) })
		return errBoundary
	}

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h"})
	if res.Reverted {
		t.Error("the restore did not succeed, so Reverted must be false")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "restoring") {
		t.Errorf("the error should mention the failed restore: %v", res.Err)
	}
}

// The container is running the new image by the time a commit is attempted,
// so a commit failure does not make the update untrue.
func TestApply_CommitFailure_LeavesTheUpdateApplied(t *testing.T) {
	f := fixture.New(t)
	finding, file := pending(t, f)

	r := newRecorder()
	r.git.Commit = func(string, string) error { return errBoundary }

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h"})

	if !res.Applied {
		t.Error("the container is running the new image; the update is applied")
	}
	if res.FailedAt != StepCommit {
		t.Errorf("expected a commit failure, got %+v", res)
	}
	if res.Reverted {
		t.Error("a commit failure must not undo a running container")
	}
	// The pin stands as an uncommitted change, which `git status` will show.
	if got := imageLine(t, file, finding.Service); !strings.Contains(got, "@"+r.digest) {
		t.Errorf("the new pin should remain in the file: %s", got)
	}
}

// A commit that did not reach the remote rides along with the next push.
func TestApply_PushFailure_IsANoteNotAFailure(t *testing.T) {
	f := fixture.New(t)
	finding, _ := pending(t, f)

	r := newRecorder()
	r.git.Push = func(string) error { return errBoundary }

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h", Push: true})

	if !res.Applied || res.Err != nil {
		t.Errorf("a failed push must not fail the transaction: %+v", res)
	}
	if res.Note == "" {
		t.Error("a failed push should be noted")
	}
}

// duva has no business resolving a conflict, so it aborts and reports.
func TestApply_RebaseConflict_IsAborted(t *testing.T) {
	f := fixture.New(t)
	finding, _ := pending(t, f)

	r := newRecorder()
	r.git.PullRebase = func(string) error { return errBoundary }

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h", Push: true})

	if !r.did("abort") {
		t.Error("a failed rebase must be aborted, not left in progress")
	}
	if r.did("push") {
		t.Error("nothing should be pushed on top of a conflicted rebase")
	}
	if res.Note == "" {
		t.Error("the conflict should be reported")
	}
}

// Push is off by default: it needs a key in a container that already holds the
// docker socket, for value a human's next push delivers anyway.
func TestApply_PushIsOptional(t *testing.T) {
	f := fixture.New(t)
	finding, _ := pending(t, f)

	r := newRecorder()
	apply(finding, r.docker, r.git, applyOptions{Host: "h"}) // Push not set

	if r.did("push") || r.did("rebase") {
		t.Errorf("push must be opt-in: %v", r.calls)
	}
	if !r.did("commit") {
		t.Error("the change should still be committed locally")
	}
}

// duva's commit must not sweep up a half-finished edit someone left behind.
func TestApply_RefusesADirtyRepository(t *testing.T) {
	f := fixture.New(t)
	finding, file := pending(t, f)
	before := imageLine(t, file, finding.Service)

	r := newRecorder()
	r.git.IsClean = func(string) (bool, error) { return false, nil }

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h"})

	if res.Err == nil {
		t.Fatal("expected a refusal")
	}
	if r.did("pull") || r.did("up") {
		t.Errorf("nothing should have happened: %v", r.calls)
	}
	if imageLine(t, file, finding.Service) != before {
		t.Error("the compose file must be untouched")
	}
}

// The registry can offer something the file already pins -- nothing to do,
// and not a failure.
func TestApply_NoChangeIsNotAFailure(t *testing.T) {
	f := fixture.New(t)
	finding, file := pending(t, f)

	// The pull yields the digest the file already carries.
	current := imageLine(t, file, finding.Service)
	r := newRecorder()
	r.digest = current[strings.Index(current, "@")+1:]

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h"})

	if res.Err != nil {
		t.Fatalf("unexpected failure: %v", res.Err)
	}
	if res.Outcome.Changed {
		t.Error("nothing changed, so the outcome should say so")
	}
	if r.did("up") || r.did("commit") {
		t.Errorf("nothing should have been recreated or committed: %v", r.calls)
	}
}

// A moving tag's reference does not change -- only what it points at does --
// so the pull must use the current tag rather than the candidate digest.
func TestApply_MovingTagPullsTheTagNotTheDigest(t *testing.T) {
	f := fixture.New(t)
	svc := f.MovingTagService(true)
	file := f.Project(svc)

	finding := watch.Finding{
		Service:    svc.Name,
		File:       file,
		Image:      svc.Image,
		CurrentTag: svc.Tag,
		Kind:       watch.KindDigest,
		Candidate:  svc.AvailableDigest,
		Status:     watch.StatusAvailable,
	}

	var pulled string
	r := newRecorder()
	r.docker.Pull = func(ref string) error { pulled = ref; return nil }

	apply(finding, r.docker, r.git, applyOptions{Host: "h"})

	want := svc.Image + ":" + svc.Tag
	if pulled != want {
		t.Errorf("pulled %q, want %q -- a digest is not a pullable reference here", pulled, want)
	}
}

// A locked index means something else is committing right now. duva's next
// run will find the repository quiet, so this is a note rather than a
// failure -- reporting it as an error would cry wolf every time someone
// edits the stack while duva happens to wake.
func TestApply_BusyRepositoryIsDeferred(t *testing.T) {
	f := fixture.New(t)
	finding, file := pending(t, f)
	before := imageLine(t, file, finding.Service)

	r := newRecorder()
	r.git.IsClean = func(string) (bool, error) { return false, ErrRepoBusy }

	res := apply(finding, r.docker, r.git, applyOptions{Host: "h"})

	if res.Err != nil {
		t.Errorf("a busy repository is not a failure: %v", res.Err)
	}
	if res.Applied {
		t.Error("nothing should have been applied")
	}
	if res.Note == "" {
		t.Error("the deferral should be noted")
	}
	if r.did("pull") || r.did("up") || r.did("commit") {
		t.Errorf("nothing should have happened: %v", r.calls)
	}
	if imageLine(t, file, finding.Service) != before {
		t.Error("the compose file must be untouched")
	}
}
