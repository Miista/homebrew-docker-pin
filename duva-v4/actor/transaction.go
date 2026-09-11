package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Miista/homebrew-docker-pin/compose"
	"github.com/Miista/homebrew-docker-pin/dockerapi"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/actor"
	"github.com/Miista/homebrew-docker-pin/internal/pin"
)

// Applying is five steps: pull the image, write the digest into the compose
// file, recreate the container, commit, push.
//
// There is no rollback except one case. If the container refuses the new
// image the file is put back, because leaving a claim there would be a lie;
// everything after the container is record-keeping and cannot make the update
// untrue.
//
// The seams -- docker, git -- are struct fields so tests can fake them. What
// they cannot fake is whether the container really ends up running the image
// that was pinned, which is what an integration suite is for.

// Docker is the docker access applying needs.
type Docker struct {
	Pull          func(ref string) error
	GetDigest     func(ref string) (string, error)
	Recreate      func(composeFile, service string) error
	ContainerName func(service string) string
}

// Git is the git access applying needs.
type Git struct {
	Add         func(dir, file string) error
	Commit      func(dir, message string) error
	PullRebase  func(dir string) error
	RebaseAbort func(dir string) error
	Push        func(dir string) error
	IsClean     func(dir string) (bool, error)
}

var realDocker = Docker{
	Pull:          dockerapi.Pull,
	GetDigest:     dockerapi.Digest,
	Recreate:      dockerapi.Recreate,
	ContainerName: dockerapi.ContainerName,
}

var realGit = Git{
	Add: func(dir, file string) error { return runGit(gitCmd(dir, "add", file)) },
	Commit: func(dir, message string) error {
		// Identity per invocation: this image carries no ~/.gitconfig, and a
		// commit that failed for want of a user.email would be a confusing
		// way to learn that.
		return runGit(gitCmd(dir,
			"-c", "user.name=duva",
			"-c", "user.email=duva@localhost",
			"commit", "-m", message))
	},
	PullRebase:  func(dir string) error { return runGit(gitCmd(dir, "pull", "--rebase")) },
	RebaseAbort: func(dir string) error { return runGit(gitCmd(dir, "rebase", "--abort")) },
	Push:        func(dir string) error { return runGit(gitCmd(dir, "push")) },
	IsClean: func(dir string) (bool, error) {
		cmd := gitCmd(dir, "status", "--porcelain")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if isRepoBusy(stderr.String()) {
				return false, errRepoBusy
			}
			return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(string(out)) == "", nil
	},
}

// errRepoBusy is git declining to answer because the index is locked. Not a
// failure: someone is committing, and the next attempt will find it quiet.
var errRepoBusy = errors.New("the repository is busy")

func isRepoBusy(stderr string) bool {
	return strings.Contains(stderr, "index.lock") ||
		strings.Contains(stderr, "Another git process seems to be running")
}

func gitCmd(dir string, args ...string) *exec.Cmd {
	// The compose directory is a mounted volume, so its files belong to
	// whoever owns them on the host rather than to the user this runs as.
	full := append([]string{"-C", dir, "-c", "safe.directory=" + dir}, args...)
	return exec.Command("git", full...)
}

func runGit(cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// transaction carries out one request.
//
// The signature is the actor contract's, not duva's: what comes in says what
// should become true, and what goes out is a status and a reason. Nothing
// about duva's Finding, its policy, or its queue appears here.
func transaction(req actor.Request, step func(string, ...any), d Docker, g Git, push bool) (actor.Status, string) {
	dir := dirOf(req.File)

	// A clean repository is a precondition, not a setting: this commits, and
	// committing on top of someone's half-finished edit is never wanted.
	if g.IsClean != nil {
		clean, err := g.IsClean(dir)
		if errors.Is(err, errRepoBusy) {
			// Not a failure. The decider will be told again and the next
			// attempt finds the repository quiet.
			return actor.Failed, "the repository was busy; will try again"
		}
		if err != nil {
			return actor.Failed, fmt.Sprintf("checking the repository: %v", err)
		}
		if !clean {
			return actor.Failed, "the repository has uncommitted changes; refusing to commit on top of them"
		}
	}

	ref := candidateRef(req)
	container := req.Service
	if d.ContainerName != nil {
		if name := d.ContainerName(req.Service); name != "" {
			container = name
		}
	}

	// Pull first: the cheapest failure is the one before anything is
	// written.
	step("pulling %s for %s", ref, container)
	if err := d.Pull(ref); err != nil {
		return actor.Failed, fmt.Sprintf("pulling %s: %v", ref, err)
	}

	// pin.Compute derives the tag by stripping the base image from the ref it
	// is given, so it is handed the tag form -- `image:tag` -- whatever was
	// pulled. For a digest move that tag is unchanged and the digest is what
	// moved; for a version change the tag is the new one. Either way the
	// digest comes from the image that was just pulled.
	before, out, err := writePin(req, req.Image+":"+req.Tag, d)
	if err != nil {
		return actor.Failed, fmt.Sprintf("writing the pin: %v", err)
	}
	if !out.Changed {
		// The file already says what was asked for. Under a protocol that
		// re-notifies until something is done, this is how being told twice
		// produces one commit rather than two.
		step("%s already pins %s", container, out.NewRaw)
		return actor.Completed, ""
	}
	step("pinned %s to %s", container, out.NewRaw)

	step("recreating %s", container)
	if err := d.Recreate(req.File, req.Service); err != nil {
		step("%s refused the new image, putting the compose file back", container)
		// The container refused it, so the file must not keep claiming it.
		// A failure to restore is worse than the original problem.
		if rerr := restorePin(req, before); rerr != nil {
			return actor.Failed, fmt.Sprintf("%v; and restoring the previous pin failed: %v", err, rerr)
		}
		return actor.Failed, fmt.Sprintf("recreating %s: %v", container, err)
	}

	// From here the update is true whatever else happens: the container is
	// running the new image, and the rest is record-keeping.
	msg := fmt.Sprintf("%s: %s -> %s", req.Service, displayOf(req.From), displayOf(req.To))
	step("committing %q", msg)
	if err := g.Add(dir, req.File); err != nil {
		return actor.Completed, "" // applied; the commit is not the update
	}
	if err := g.Commit(dir, msg); err != nil {
		step("the change is live but could not be committed: %v", err)
		return actor.Completed, ""
	}

	if push {
		step("pushing")
		if err := g.PullRebase(dir); err != nil {
			// A rebase that conflicts is never this process's to resolve.
			if g.RebaseAbort != nil {
				g.RebaseAbort(dir)
			}
			step("could not rebase before pushing: %v", err)
			return actor.Completed, ""
		}
		if err := g.Push(dir); err != nil {
			// A commit that did not reach the remote rides along with the
			// next push. Worth saying, not worth failing over.
			step("committed locally but not pushed: %v", err)
		}
	}
	return actor.Completed, ""
}

// writePin rewrites the image line, returning what it said before.
//
// pin.Compute does the work, so the actor and `docker pin` cannot disagree
// about what a pinned line looks like -- which is the reason that package
// exists at all.
func writePin(req actor.Request, tagRef string, d Docker) (before string, out pin.Outcome, err error) {
	before, err = compose.RawImage(req.File, req.Service)
	if err != nil {
		return "", pin.Outcome{}, err
	}
	// The digest of what was pulled, not of the tag: a digest move pulled a
	// specific digest, and asking about the tag again could answer with
	// whatever it points at now rather than what was decided about.
	getDigest := d.GetDigest
	if req.Digest != "" {
		getDigest = func(string) (string, error) { return req.Digest, nil }
	}
	out, err = pin.Compute(req.File, req.Service, tagRef, pin.Docker{
		GetDigest: getDigest,
		Pull:      d.Pull,
	})
	if err != nil {
		return "", pin.Outcome{}, err
	}
	if out.Built {
		// A locally built image has no registry digest, so pinning it would
		// write a reference no other host can pull.
		return "", pin.Outcome{}, fmt.Errorf("%s is built locally, so there is nothing to pin", req.Service)
	}
	if !out.Changed {
		return before, out, nil
	}
	if err := pin.Apply(req.File, req.Service, out); err != nil {
		return "", pin.Outcome{}, err
	}
	return before, out, nil
}

// restorePin puts the image line back to what it was.
func restorePin(req actor.Request, before string) error {
	return compose.PinImage(req.File, req.Service, before)
}

// candidateRef is what to pull.
//
// A version change pulls the tag; a digest move pulls the digest, because the
// tag has not changed and pulling it again would fetch whatever it points at
// now rather than what was decided about.
func candidateRef(req actor.Request) string {
	if req.Digest != "" {
		return req.Image + "@" + req.Digest
	}
	return req.Image + ":" + req.Tag
}

// displayOf shortens a digest for a commit subject. Seventy-one characters of
// hex tells a reader nothing the first twelve do not.
func displayOf(s string) string {
	if strings.HasPrefix(s, "sha256:") && len(s) > 19 {
		return s[:19]
	}
	return s
}

func dirOf(composeFile string) string { return filepath.Dir(composeFile) }
