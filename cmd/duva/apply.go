package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/pin"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

// Applying an update is five steps: pull the image, write the digest into the
// compose file, recreate the container, commit, push. Each is a function that
// does one thing and reports whether it worked; none of them knows what came
// before or what should happen if it fails. The transaction decides that --
// a step that understood rollback would have to understand the whole sequence,
// which is exactly what makes such code untestable.
//
// The steps that leave this machine -- docker, git -- are seams, so unit tests
// can fake them and the integration suite can use the real ones.

// Docker is the docker access applying an update needs.
type Docker struct {
	// Pull fetches an image reference.
	Pull func(ref string) error
	// GetDigest returns the repo digest of a locally present image.
	GetDigest func(ref string) (string, error)
	// ComposeUp recreates one service from its compose file.
	ComposeUp func(composeFile, service string) error
}

// Git is the git access applying an update needs. duva owns this, not
// internal/pin: pinning an image line is a thing two tools share, but
// committing it to a repository is duva's business alone.
type Git struct {
	// Add stages one file, relative to the repository at dir.
	Add func(dir, file string) error
	// Commit records the staged change. Author identity is passed per call
	// rather than configured, so duva needs no ~/.gitconfig in its image.
	Commit func(dir, message string) error
	// PullRebase rebases onto the upstream branch before pushing, so a
	// concurrent push from another host is absorbed rather than rejected.
	PullRebase func(dir string) error
	// RebaseAbort leaves a conflicted rebase, which duva must never try to
	// resolve.
	RebaseAbort func(dir string) error
	// Push publishes the branch.
	Push func(dir string) error
	// IsClean reports whether the repository has no uncommitted changes.
	IsClean func(dir string) (bool, error)
}

var realDocker = Docker{
	Pull: func(ref string) error {
		return run(exec.Command("docker", "pull", "-q", ref))
	},
	GetDigest: func(ref string) (string, error) {
		out, err := exec.Command("docker", "image", "inspect", ref,
			"--format", "{{index .RepoDigests 0}}").Output()
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", ref, err)
		}
		raw := strings.TrimSpace(string(out))
		if i := strings.Index(raw, "@"); i != -1 {
			return raw[i+1:], nil
		}
		return "", fmt.Errorf("no repo digest for %s", ref)
	},
	// The project is named explicitly rather than derived. duva sees every
	// compose file at the same mount point, so compose would name every stack
	// after that mount and fail to find the container it was asked to replace
	// -- starting a second one beside it. See project.go.
	ComposeUp: func(composeFile, service string) error {
		name, err := project(realSelf)
		if err != nil {
			return err
		}
		return run(exec.Command("docker", "compose",
			"--project-name", name, "-f", composeFile, "up", "-d", service))
	},
}

var realGit = Git{
	Add: func(dir, file string) error {
		return run(gitCmd(dir, "add", file))
	},
	Commit: func(dir, message string) error {
		// Identity per invocation: duva's image carries no ~/.gitconfig, and
		// a commit that fails for want of a user.email would be a confusing
		// way to learn that. No --no-verify: a repository's commit-msg hook
		// rejecting a malformed subject is correct, not an obstacle.
		return run(gitCmd(dir,
			"-c", "user.name=duva",
			"-c", "user.email=duva@localhost",
			"commit", "-m", message))
	},
	PullRebase:  func(dir string) error { return run(gitCmd(dir, "pull", "--rebase")) },
	RebaseAbort: func(dir string) error { return run(gitCmd(dir, "rebase", "--abort")) },
	Push:        func(dir string) error { return run(gitCmd(dir, "push")) },
	IsClean: func(dir string) (bool, error) {
		out, err := gitCmd(dir, "status", "--porcelain").Output()
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(string(out)) == "", nil
	},
}

func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}

func run(cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", cmd.Args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- the steps ----------------------------------------------------------

// pullImage fetches the candidate. Doing this first means the cheapest
// failure -- an image that cannot be fetched -- happens before anything has
// been written.
func pullImage(ref string, d Docker) error {
	if err := d.Pull(ref); err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	return nil
}

// writePin records the pulled image's digest in the compose file, under the
// tag it was pulled as. Returns what was there before, so the transaction can
// put it back.
func writePin(composeFile, service, ref string, d Docker) (before string, out pin.Outcome, err error) {
	before, err = compose.RawImage(composeFile, service)
	if err != nil {
		return "", pin.Outcome{}, err
	}
	out, err = pin.Compute(composeFile, service, ref, pin.Docker{
		GetDigest: d.GetDigest,
		Pull:      d.Pull,
	})
	if err != nil {
		return before, out, fmt.Errorf("computing the new pin for %s: %w", service, err)
	}
	if !out.Changed {
		return before, out, nil
	}
	if err := pin.Apply(composeFile, service, out); err != nil {
		return before, out, fmt.Errorf("writing the new pin for %s: %w", service, err)
	}
	return before, out, nil
}

// restorePin puts an image line back as it was, for when the container
// refused the new one.
func restorePin(composeFile, service, before string) error {
	return compose.PinImage(composeFile, service, before)
}

// recreate brings the service up on the newly pinned image.
func recreate(composeFile, service string, d Docker) error {
	if err := d.ComposeUp(composeFile, service); err != nil {
		return fmt.Errorf("recreating %s: %w", service, err)
	}
	return nil
}

// commit records the compose change. The repository is the directory the
// compose file lives in, or an ancestor of it -- git resolves that itself.
func commit(composeFile, message string, g Git) error {
	dir := filepath.Dir(composeFile)
	if err := g.Add(dir, filepath.Base(composeFile)); err != nil {
		return fmt.Errorf("staging: %w", err)
	}
	if err := g.Commit(dir, message); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

// push publishes the commit, rebasing first so a concurrent push from another
// host is absorbed rather than rejected. A conflict is left alone: duva has no
// business resolving one, so it aborts the rebase and reports.
func push(composeFile string, g Git) error {
	dir := filepath.Dir(composeFile)
	if err := g.PullRebase(dir); err != nil {
		if abortErr := g.RebaseAbort(dir); abortErr != nil {
			return fmt.Errorf("rebase failed (%v) and could not be aborted: %w", err, abortErr)
		}
		return fmt.Errorf("rebasing before push: %w", err)
	}
	if err := g.Push(dir); err != nil {
		return fmt.Errorf("pushing: %w", err)
	}
	return nil
}

// commitMessage follows the fleet convention, <host>/<service>: <subject>.
func commitMessage(host, service, from, to string) string {
	return fmt.Sprintf("%s/%s: %s -> %s", host, service, from, to)
}

// --- the finding a step operates on ------------------------------------

// candidateRef is the image reference to pull for a finding: the newer tag
// for a constrained service, or the same moving tag for one following a
// digest. A moving tag's reference does not change -- only what it points at
// does -- which is why this cannot simply use the candidate.
func candidateRef(f watch.Finding) string {
	if f.Kind == watch.KindDigest {
		return f.Image + ":" + f.CurrentTag
	}
	return f.Image + ":" + f.Candidate
}

// dirOf is the directory a compose file lives in, which is where git commands
// for it run.
func dirOf(composeFile string) string {
	return filepath.Dir(composeFile)
}
