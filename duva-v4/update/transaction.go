package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Miista/homebrew-docker-pin/compose"
	"github.com/Miista/homebrew-docker-pin/dockerapi"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/update"
	"github.com/Miista/homebrew-docker-pin/internal/pin"
)

// Applying is five steps: pull the image, write the digest into the compose
// file, recreate the container, commit, push.
//
// There is no rollback. Not as an omission -- as the design.
//
// Once a recreate has been attempted, this process cannot know what state the
// container is in. Recreating stops and removes the old container before
// creating the replacement, so a single error covers four different worlds:
// the old one still running, the old one stopped but present, nothing at all,
// or a new container that exists and would not start. Undoing the file is
// right in the first two and actively wrong in the last two, where it would
// make the file disagree with the container that actually exists.
//
// An earlier version did restore the pin on a failed recreate, under the
// claim "the container refused the new image". That described only the
// last of those four, and was the case the restore suited least.
//
// So the file is left saying what was decided, the repository is left dirty,
// and the reason says so. IsClean then refuses the next apply until a person
// looks -- which is the same handling every failure after the container
// already gets, and the only one that does not assert something unverified.
// Watchtower reaches the same answer from the other side: it has no file to
// put back and still accepts an outage on failure rather than undoing.
//
// The seams -- docker, git -- are struct fields so tests can fake them. What
// they cannot fake is whether the container really ends up running the image
// that was pinned, which is what an integration suite is for.

// Docker is the docker access applying needs.
type Docker struct {
	Pull      func(ref string) error
	GetDigest func(ref string) (string, error)
	Recreate  func(composeFile, service string) error
	// Exists reports whether the service has a container to replace, without
	// touching it. Nil skips the check.
	Exists        func(service string) error
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
	Exists:        dockerapi.Exists,
	ContainerName: dockerapi.ContainerName,
}

var realGit = Git{
	Add: func(dir, file string) error { return runGit(gitCmd(dir, "add", file)) },
	Commit: func(dir, message string) error {
		// No -c identity. The repository being committed to is bind-mounted
		// in, and its .git/config comes with it -- so whatever identity the
		// repository already uses is the one it gets, which is the right
		// answer and not this program's to overrule.
		//
		// A repository with no identity configured anywhere will fail the
		// commit and say so, which is a better outcome than every commit
		// being authored by a name nobody recognises.
		return runGit(gitCmd(dir, "commit", "-m", message))
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

// resolveFile is which compose file declares a service, looked up against this
// stage's own project.
//
// A seam, so a test can point it at a temporary project rather than at the
// mount a container would have. The production one is projectFile plus
// compose.ResolveServiceIn.
var resolveFile = func(service string) (string, error) {
	root, err := projectFile()
	if err != nil {
		return "", fmt.Errorf("finding the compose project: %w", err)
	}
	file, err := compose.ResolveServiceIn(root, service)
	if err != nil {
		return "", fmt.Errorf("finding %s: %w", service, err)
	}
	return file, nil
}

// transaction carries out one request.
//
// The signature is the updater contract's, not duva's: what comes in says what
// should become true, and what goes out is a status and a reason. Nothing
// about duva's Finding, its policy, or its queue appears here.
func transaction(req update.Request, step func(string, ...any), d Docker, g Git, push bool, tmpl string) (update.Status, string) {
	// Which file declares this service is answered here, not taken from the
	// request.
	//
	// The queue sends a path, and that path is rooted where the QUEUE mounted
	// the project -- which is not where this stage mounts it, and cannot be:
	// this one holds the repository, at the path the host has, because it
	// commits and because a relative bind it hands the daemon has to resolve
	// there. A path from the other side of that difference means nothing here.
	//
	// The service name does mean the same thing on both sides, and the project
	// is mounted, so the file is a lookup rather than something to be told.
	// `include:` is followed, which is the only reason the file was ever worth
	// carrying: a service can live in one.
	file, err := resolveFile(req.Service)
	if err != nil {
		return update.Failed, err.Error()
	}

	dir := dirOf(file)

	// A clean repository is a precondition, not a setting: this commits, and
	// committing on top of someone's half-finished edit is never wanted.
	if g.IsClean != nil {
		clean, err := g.IsClean(dir)
		if errors.Is(err, errRepoBusy) {
			// Not a failure. The queue will be told again and the next
			// attempt finds the repository quiet.
			return update.Failed, "the repository was busy; will try again"
		}
		if err != nil {
			return update.Failed, fmt.Sprintf("checking the repository: %v", err)
		}
		if !clean {
			return update.Failed, "the repository has uncommitted changes; refusing to commit on top of them"
		}
	}

	ref := candidateRef(req)
	container := req.Service
	if d.ContainerName != nil {
		if name := d.ContainerName(req.Service); name != "" {
			container = name
		}
	}

	// Everything answerable, answered before anything is done.
	//
	// The principle: do not start what cannot finish. Every one of these used
	// to fail *after* a pull, and some after a pin was written -- leaving a
	// compose file claiming an image nothing runs. None of them can be
	// answered by trying and undoing, because undoing is what this design
	// refuses to do once a container is involved.
	//
	// What cannot be pre-checked is the pull and the recreate: pulling is its
	// own test, and whether a container will accept an image is only knowable
	// by giving it one. A failed pull has written nothing. A failed recreate
	// is the one outcome this cannot clean up after, which is why so much is
	// answered here instead.
	if err := precheck(req, file, d, tmpl); err != nil {
		return update.Failed, err.Error()
	}

	// The steps name the act and nothing else. Every one of them used to
	// carry the container and the full pinned line, and a digest is 71
	// characters -- so the panel was mostly a restatement of the row it sits
	// inside, which already says which service and which version. Failures
	// still carry the detail, because there the detail is the explanation.
	//
	// Pull first: the cheapest failure is the one before anything is
	// written.
	step("pulling new image")
	if err := d.Pull(ref); err != nil {
		return update.Failed, fmt.Sprintf("pulling %s: %v", ref, err)
	}

	// pin.Compute derives the tag by stripping the base image from the ref it
	// is given, so it is handed the tag form -- `image:tag` -- whatever was
	// pulled. For a digest move that tag is unchanged and the digest is what
	// moved; for a version change the tag is the new one. Either way the
	// digest comes from the image that was just pulled.
	out, err := writePin(req, file, req.Image+":"+req.Tag, d)
	if err != nil {
		return update.Failed, fmt.Sprintf("writing the pin: %v", err)
	}
	if !out.Changed {
		// The file already says what was asked for. Under a protocol that
		// re-notifies until something is done, this is how being told twice
		// produces one commit rather than two.
		step("already pinned, nothing to do")
		return update.Completed, ""
	}
	step("pinning image")

	step("recreating")
	if err := d.Recreate(file, req.Service); err != nil {
		// Left as it is, deliberately. See the note at the top of this file:
		// what happened to the container is not knowable from here, so the
		// file is not touched and the state is handed to a person instead.
		reason := fmt.Sprintf("recreating %s: %v"+
			" — the compose file still records %s and the repository is now dirty,"+
			" so no further update can be applied on this host until that is resolved."+
			" Check whether %s is running before deciding which way to resolve it",
			container, err, out.NewRaw, container)
		step("%s", reason)
		return update.Failed, reason
	}

	// From here the update is true whatever else happens: the container is
	// running the new image, and the rest is record-keeping.
	// From and To are tags for a version change and digests for a digest move
	// -- see the contract. Split here so a template can say "1.0 -> 1.1"
	// without the digest fields lying about it, and so a digest move reads as
	// the same tag rather than as a version that did not change.
	fields := commitFields{
		Container:  req.Service,
		Image:      req.Image,
		OldVersion: req.Tag,
		NewVersion: req.Tag,
	}
	if isDigest(req.From) || isDigest(req.To) {
		fields.OldDigest, fields.NewDigest = displayOf(req.From), displayOf(req.To)
	} else {
		fields.OldVersion, fields.NewVersion = req.From, req.To
		fields.NewDigest = displayOf(req.Digest)
	}
	msg, err := commitSubject(tmpl, fields)
	if err != nil {
		// Completed with a reason, never a bare Completed: the container is
		// running the new image, so the update is true -- but the record of it
		// was not written, and that must reach a log rather than only a stream
		// somebody had to be watching.
		return done(step, "the change is live but the commit message could not be built: %v", err)
	}
	step("committing")
	if err := g.Add(dir, file); err != nil {
		return done(step, "the change is live but could not be staged: %v", err)
	}
	if err := g.Commit(dir, msg); err != nil {
		// Not undone, and deliberately not tidied either.
		//
		// The container is running the new image, so a rollback would tear
		// down a healthy service over a commit message. But unstaging does not
		// help either, and an earlier version of this did it: `git restore
		// --staged` turns "M " into " M", which `git status --porcelain` still
		// reports, so IsClean still refuses and the next apply is blocked
		// exactly as before. It moved the problem from the index to the
		// worktree and read like a safeguard.
		//
		// The repository *is* dirty, and correctly so: the compose file records
		// an update nobody committed. The next apply refusing is right. What
		// matters is that this is loud enough for a person to see it, because
		// only a person can resolve it.
		return done(step, "the change is live but could not be committed: %v"+
			" — the compose file records it and the repository is now dirty,"+
			" so no further update can be applied on this host until that is resolved", err)
	}

	if push {
		step("pushing")
		if err := g.PullRebase(dir); err != nil {
			// A rebase that conflicts is never this process's to resolve.
			if g.RebaseAbort != nil {
				g.RebaseAbort(dir)
			}
			return done(step, "committed locally, but could not rebase before pushing: %v", err)
		}
		if err := g.Push(dir); err != nil {
			// A commit that did not reach the remote rides along with the next
			// push. Worth saying, not worth failing over -- but said with a
			// reason, so it reaches the log and not only the stream.
			return done(step, "committed locally but not pushed: %v", err)
		}
	}
	return update.Completed, ""
}

// writePin rewrites the image line.
//
// pin.Compute does the work, so the updater and `docker pin` cannot disagree
// about what a pinned line looks like -- which is the reason that package
// exists at all.
//
// It used to also return the line as it stood before, for the rollback. There
// is no rollback, so there is nothing to hold it for.
func writePin(req update.Request, file, tagRef string, d Docker) (out pin.Outcome, err error) {
	// The digest of what was pulled, not of the tag: a digest move pulled a
	// specific digest, and asking about the tag again could answer with
	// whatever it points at now rather than what was decided about.
	getDigest := d.GetDigest
	if req.Digest != "" {
		getDigest = func(string) (string, error) { return req.Digest, nil }
	}
	out, err = pin.Compute(file, req.Service, tagRef, pin.Docker{
		GetDigest: getDigest,
		Pull:      d.Pull,
	})
	if err != nil {
		return pin.Outcome{}, err
	}
	if out.Built {
		// A locally built image has no registry digest, so pinning it would
		// write a reference no other host can pull.
		return pin.Outcome{}, fmt.Errorf("%s is built locally, so there is nothing to pin", req.Service)
	}
	if !out.Changed {
		return out, nil
	}
	if err := pin.Apply(file, req.Service, out); err != nil {
		return pin.Outcome{}, err
	}
	return out, nil
}

// candidateRef is what to pull.
//
// A version change pulls the tag; a digest move pulls the digest, because the
// tag has not changed and pulling it again would fetch whatever it points at
// now rather than what was decided about.
func candidateRef(req update.Request) string {
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

// isDigest says whether a reference is a digest rather than a tag.
func isDigest(s string) bool { return strings.HasPrefix(s, "sha256:") }

// done reports work that finished with the container running the new image
// but something after it unfinished.
//
// Completed, because the update is true: the host is running what was asked
// for, and no amount of bookkeeping trouble makes that untrue. With a reason,
// because "completed" alone is what let a staged, uncommitted change sit
// unnoticed -- the reason goes into the terminal line, which the queue
// relays and the updater logs.
//
// Said once here rather than at each call site, so a new step after the
// container cannot quietly return a bare Completed.
func done(step func(string, ...any), format string, args ...any) (update.Status, string) {
	reason := fmt.Sprintf(format, args...)
	step("%s", reason)
	return update.Completed, reason
}

// precheck answers everything that can be answered before anything is done.
//
// Ordered cheapest-and-most-likely first, and every failure is reported with
// nothing yet changed -- which is the whole point. A refusal here is a queue
// entry that stays queued and a person who can fix the cause; a failure two
// steps later is a compose file recording an image that nothing runs.
//
// The compose file is checked by computing the pin rather than writing it:
// pin.Compute is the same function the write uses, so a file it cannot parse,
// a service it cannot find, or an image line it cannot rewrite is discovered
// here rather than after a pull.
func precheck(req update.Request, file string, d Docker, tmpl string) error {
	// Is there a container to replace? A service in the compose file with
	// nothing running is the failure that leaves a pin claiming an image that
	// does not run anywhere.
	if d.Exists != nil {
		if err := d.Exists(req.Service); err != nil {
			return fmt.Errorf("%s has no container to replace: %w", req.Service, err)
		}
	}

	// Would the write succeed? Asked by doing the write's own computation,
	// not by reimplementing its checks.
	//
	// pin.Compute is exactly what `docker pin --dry-run` runs: it is the half
	// of the write that decides what the line should become, and pin.Apply is
	// the half that writes it. So everything the real write would refuse --
	// a built image whose digest is local to this daemon, a file it cannot
	// parse, an image line it cannot rewrite, a digest already there that is
	// malformed -- is refused here, with nothing yet done.
	//
	// An earlier version of this hand-rolled IsBuilt and HasUnexpandedVariable
	// beside it, which was a second implementation of a question this already
	// answers, and one that would drift.
	//
	// The digest is the one the queue decided on rather than the daemon's,
	// so this needs no pull: the point is to answer before pulling. For a
	// version change the queue has no digest yet, and the only honest answer
	// is a placeholder -- everything except the digest itself is still checked,
	// which is everything that can fail before the pull.

	// An unexpanded variable is the one question the engine does not answer.
	// Every caller checks it around Compute rather than inside it -- `docker
	// pin`, duva, the queue and the watcher all do -- because `${TAG}` is a
	// compose-file question, not a pinning one. Left to Compute it would be
	// pulled as a literal and fail at the registry, describing a tag nobody
	// wrote. Checked first because it is the cheapest and reads the same file.
	if raw, err := compose.RawImage(file, req.Service); err == nil &&
		compose.HasUnexpandedVariable(raw) {
		return fmt.Errorf(
			"%s in %s still has an unexpanded variable (%s), so there is nothing to pin",
			req.Service, file, raw)
	}

	probeDigest := req.Digest
	if probeDigest == "" {
		probeDigest = "sha256:" + strings.Repeat("0", 64)
	}
	out, err := pin.Compute(file, req.Service, req.Image+":"+req.Tag, pin.Docker{
		GetDigest: func(string) (string, error) { return probeDigest, nil },
		Pull:      func(string) error { return nil }, // not reached: the digest is given
	})
	if err != nil {
		return fmt.Errorf("%s cannot be pinned in %s: %w", req.Service, file, err)
	}
	// Built is not an error from Compute -- it is a successful outcome saying
	// there is nothing to pin, which the caller is expected to act on. Checked
	// here rather than only in writePin, where it would be found after a pull.
	//
	// It does reach here: the watcher and the queue both skip unexpanded
	// variables, but neither looks at `build:`, so a service carrying both a
	// `build:` and a pinned `image:` arrives with nothing having filtered it.
	// Its digest is local to this daemon, so pinning one would write a
	// reference no other host could pull, into a file that is committed.
	if out.Built {
		return fmt.Errorf("%s is built locally (build:), so its digest is local to this "+
			"daemon and no other host could pull what would be written", req.Service)
	}

	// Would the commit be accepted? The same question readiness asks, asked
	// again with this service's real values -- readiness uses a representative
	// message, and a template that renders differently for a real service
	// would otherwise be found out after the container was replaced.
	if err := checkMessageAccepted(dirOf(file), tmpl); err != nil {
		return err
	}
	return nil
}
