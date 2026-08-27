package main

import (
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
	// RequireCleanRepo refuses to act when the compose file's repository has
	// uncommitted changes, so duva's commit cannot sweep up someone's
	// half-finished edit.
	RequireCleanRepo bool
}

// apply runs the transaction for one finding.
func apply(f watch.Finding, d Docker, g Git, opts applyOptions) Result {
	res := Result{Service: f.Service}

	if opts.RequireCleanRepo && g.IsClean != nil {
		clean, err := g.IsClean(dirOf(f.File))
		if err != nil {
			return fail(res, StepWrite, fmt.Errorf("checking the repository: %w", err))
		}
		if !clean {
			return fail(res, StepWrite, fmt.Errorf(
				"the repository has uncommitted changes; duva will not commit on top of them"))
		}
	}

	ref := candidateRef(f)

	// Pull first: the cheapest failure is the one before anything is written.
	if err := pullImage(ref, d); err != nil {
		return fail(res, StepPull, err)
	}

	before, out, err := writePin(f.File, f.Service, ref, d)
	if err != nil {
		return fail(res, StepWrite, err)
	}
	res.Outcome = out
	if !out.Changed {
		// The registry offered something the file already pins. Nothing to
		// do, and nothing to report as a failure.
		return res
	}

	if err := recreate(f.File, f.Service, d); err != nil {
		res = fail(res, StepRecreate, err)
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

	msg := commitMessage(opts.Host, f.Service, pin.TagOf(before), out.Tag)
	if err := commit(f.File, msg, g); err != nil {
		res.FailedAt, res.Err = StepCommit, err
		return res
	}

	if opts.Push {
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
