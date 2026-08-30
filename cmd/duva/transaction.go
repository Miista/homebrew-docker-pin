package main

import (
	"errors"
	"fmt"

	"github.com/Miista/homebrew-docker-pin/internal/pin"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

// Applying an update is a transaction: the steps do one thing each, and this
// decides what their failures mean.
//
// There is no rollback beyond one narrow case. If a container starts and later
// misbehaves, the compose file and git both say what was intended, and
// recovery is a human pinning the previous digest back -- which the commit
// records. Rehearsing an upgrade on a cloned stack before promoting it is a
// better answer than an after-the-fact revert, and belongs in a separate tool.
//
// The exception is a compose file describing something that has not run: if
// `compose up` refuses the new image, duva knows for certain the container
// never took it, so the file is put back. That is not rollback; it is refusing
// to leave a lie in the file.

// Step names what part of the transaction was reached, for reporting.
type Step string

const (
	StepPull     Step = "pull"
	StepWrite    Step = "write"
	StepRecreate Step = "recreate"
	StepCommit   Step = "commit"
	StepPush     Step = "push"
)

// Result is what a transaction did.
type Result struct {
	Service string
	// Applied is true when the container is running the new image. Later
	// steps can fail without changing that -- a commit that did not happen
	// does not un-run a container.
	Applied bool
	// Outcome describes the pin, when one was written.
	Outcome pin.Outcome
	// FailedAt names the step that failed, empty on success.
	FailedAt Step
	// Err is that step's error.
	Err error
	// Reverted is true when the compose file was put back because the
	// container refused the new image.
	Reverted bool
	// Note carries a warning that did not fail the transaction, such as a
	// commit that could not be pushed.
	Note string
}

// Failed reports whether the update did not reach the container.
func (r Result) Failed() bool { return r.Err != nil && !r.Applied }

// applyOptions is what a transaction needs beyond the finding.
type applyOptions struct {
	Host string
	// Push publishes the commit. Off by default: pushing needs a key in a
	// container that already holds the docker socket, for value a human's
	// next push delivers anyway.
	Push bool
	// CommitTemplate is the subject a change is committed under. Empty means
	// the default; see commit.go.
	CommitTemplate string
	// Log records each step as it happens. Every one of them changes
	// something on the host -- an image pulled, a file rewritten, a container
	// replaced, a commit made -- and an update that reported only its outcome
	// left no way to tell how far it got when a later step failed.
	Log func(format string, args ...any)
}

// step records a step, when the caller wanted a record.
func (o applyOptions) step(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// apply runs the transaction for one finding.
func apply(f watch.Finding, d Docker, g Git, opts applyOptions) Result {
	res := Result{Service: f.Service}

	// A clean repository is a precondition, not a setting: duva commits, and
	// committing on top of someone's half-finished edit is never wanted.
	if g.IsClean != nil {
		clean, err := g.IsClean(dirOf(f.File))
		// A locked index is not a failure: something else is committing right
		// now, and the next run will find the repository quiet. Reporting it
		// as an error would cry wolf every time someone edits the stack while
		// duva happens to wake.
		if errors.Is(err, ErrRepoBusy) {
			res.Note = "the repository was busy; will try again on the next check"
			return res
		}
		if err != nil {
			return fail(res, StepWrite, fmt.Errorf("checking the repository: %w", err))
		}
		if !clean {
			return fail(res, StepWrite, fmt.Errorf(
				"the repository has uncommitted changes; duva will not commit on top of them"))
		}
	}

	ref := candidateRef(f)

	// What the container is called, so every line says which one it changed.
	// The compose service name alone does not: a host runs several stacks,
	// and a service is often named for what it does rather than what it is,
	// so "soaking: pulling ..." reads as a fragment.
	container := f.Service
	if d.ContainerName != nil {
		container = d.ContainerName(f.Service)
	}

	// Pull first: the cheapest failure is the one before anything is written.
	opts.step("pulling %s for %s", ref, container)
	if err := pullImage(ref, d); err != nil {
		return fail(res, StepPull, err)
	}

	before, out, err := writePin(f.File, f.Service, ref, d)
	if err != nil {
		return fail(res, StepWrite, err)
	}
	res.Outcome = out
	if out.Changed {
		opts.step("pinned %s to %s", container, out.NewRaw)
	}
	if !out.Changed {
		// The registry offered something the file already pins. Nothing to
		// do, and nothing to report as a failure.
		return res
	}

	opts.step("recreating %s", container)
	if err := recreate(f.File, f.Service, d); err != nil {
		res = fail(res, StepRecreate, err)
		opts.step("%s refused the new image, putting the compose file back", container)
		// The container refused the new image, so the file must not keep
		// claiming it. A failure to restore is worse than the original
		// problem and is reported as such.
		if rerr := restorePin(f.File, f.Service, before); rerr != nil {
			res.Err = fmt.Errorf("%w; and restoring the previous pin failed: %v", err, rerr)
			return res
		}
		res.Reverted = true
		return res
	}

	// The container is running the new image. Everything from here is
	// record-keeping: it can fail without making the update untrue.
	res.Applied = true

	msg, err := commitSubject(opts.CommitTemplate, commitFields{
		Host:       opts.Host,
		Container:  f.Service,
		Image:      f.Image,
		OldVersion: pin.TagOf(before),
		NewVersion: out.Tag,
		OldDigest:  pin.DigestOf(before),
		NewDigest:  out.Digest,
	})
	if err != nil {
		// The container is already running the new image, so this is
		// record-keeping that failed rather than an update that did.
		res.FailedAt, res.Err = StepCommit, err
		return res
	}
	opts.step("committing %q", msg)
	if err := commit(f.File, msg, g); err != nil {
		res.FailedAt, res.Err = StepCommit, err
		return res
	}

	if opts.Push {
		opts.step("pushing the commit")
		if err := push(f.File, g); err != nil {
			// A commit that did not reach the remote rides along with the
			// next push. Worth saying, not worth failing over.
			res.Note = fmt.Sprintf("committed locally but not pushed: %v", err)
		}
	}
	return res
}

func fail(res Result, step Step, err error) Result {
	res.FailedAt, res.Err = step, err
	return res
}
