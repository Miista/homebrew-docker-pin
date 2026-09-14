// Package actor is the contract between a decider and whatever applies its
// decisions.
//
// It holds no implementation. What lives here is the shape of the request, the
// shape of the stream, and the terminal statuses — the whole of what the two
// sides must agree on. The reference actor that pulls an image, rewrites a
// pin, replaces a container and commits is one implementation of it; an actor
// that opens a pull request, or hands the work to Ansible, is another, and the
// decider cannot tell which it has.
//
// That constraint is the point, so it is worth stating what the decider is
// therefore not allowed to know:
//
//   - That applying involves a registry, a container, or git. An actor that
//     opens a pull request touches none of them.
//   - That the compose file ends up saying anything in particular. Completion
//     is the actor having done its job, not the host running a new image —
//     a pull request actor completes when the pull request exists.
//   - Anything about failure beyond that it happened and a reason to show.
//
// Completion is reported through the same stream the UI watches, rather than
// a second endpoint: there is one thing happening, and one place to read about
// it. The decider watches for the terminal line and relays everything else
// verbatim.
//
// # Versioning
//
// Because this is the only thing the two sides share, it is also the only
// thing with a compatibility obligation — which is what lets the decider and
// the actor be released independently rather than in lockstep.
//
// The `/v1` in the endpoint paths is therefore load-bearing rather than
// decorative. Adding a field here is backwards compatible: an older actor
// ignores what it does not recognise, and an older decider ignores what it is
// sent. Renaming or removing one is not, and means a `/v2` served alongside
// `/v1` until both sides have moved.
//
// So: add freely, never rename in place.
package actor

import (
	"fmt"
	"strings"
)

// Request is what a decider asks an actor to do.
//
// Deliberately says *what should become true*, not how. Service and File name
// the thing; From and To are what the decider decided. An actor reads these
// and does whatever applying means to it.
type Request struct {
	// Service is the compose service name.
	Service string `json:"service"`
	// File is the compose file it is declared in — not necessarily the
	// project root, since include: is resolved. Carried because the actor
	// should not have to re-derive it, and because the decider already knows.
	File string `json:"file"`
	// Image is the base reference, without tag or digest.
	Image string `json:"image"`
	// From and To are the change. For a version change these are tags; for a
	// moving tag whose digest moved they are digests.
	From string `json:"from"`
	To   string `json:"to"`
	// Tag is the tag to follow once this is applied. The tag is an
	// instruction, not a description of what is pinned, so it is carried
	// separately from To: a digest move leaves it alone, a version change
	// sets it.
	Tag string `json:"tag"`
	// Digest is the digest to end up on, when the decider knows it. Empty for
	// a version change, where the actor resolves it by pulling the tag.
	Digest string `json:"digest,omitempty"`
}

// Accepted is what an actor returns when it has taken the work.
type Accepted struct {
	// Stream is where to read progress from, absolute or relative to the
	// actor's base. The decider follows it and does not construct it.
	Stream string `json:"stream"`
}

// Status is how a piece of work ended.
type Status string

const (
	// InProgress: the actor is working. Not a terminal status, and never
	// appears on the wire — it is the absence of a terminal line.
	InProgress Status = "in-progress"
	// Completed: the actor did its job. Not "the host is running the new
	// image": a pull request actor completes when the pull request exists.
	Completed Status = "completed"
	// Failed: it did not, and the reason follows.
	Failed Status = "failed"
)

// TerminalPrefix marks the last line of a stream.
//
// A line prefix rather than a typed event or a separate result endpoint,
// because the stream is read by two things with different needs: the decider,
// which must recognise the end, and a person watching the UI, who should see
// something they can read. A prefix satisfies both and costs nothing —
// everything before it is relayed verbatim and the decider does not parse it.
const TerminalPrefix = "status: "

// Terminal renders the last line of a stream.
//
//	status: completed
//	status: failed: the container refused the image
func Terminal(s Status, reason string) string {
	if reason == "" {
		return TerminalPrefix + string(s)
	}
	return fmt.Sprintf("%s%s: %s", TerminalPrefix, s, reason)
}

// ParseTerminal reads a line, reporting whether it ends the stream.
//
// Anything that is not a terminal line is progress, and progress is none of
// the decider's business — so this answers only "is this the end, and how did
// it go".
func ParseTerminal(line string) (s Status, reason string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	rest, found := strings.CutPrefix(line, TerminalPrefix)
	if !found {
		return "", "", false
	}
	word, reason, _ := strings.Cut(rest, ": ")
	switch Status(word) {
	case Completed, Failed:
		return Status(word), reason, true
	default:
		// A status nobody defined is not a terminal line. Treating an
		// unrecognised one as the end would let a typo in an actor look like
		// success; leaving it as progress means the decider waits, and its
		// timeout catches it.
		return "", "", false
	}
}

// Readiness is whether an actor would accept work right now, and why not.
//
// Part of the contract because a UI that offers a button for work the actor
// would refuse is worse than one that greys it: the person has already
// decided by the time they find out. The same reasoning as reporting whether
// a decider has an actor at all.
//
// Deliberately not "is the repository clean". That is *this* actor's reason,
// and the contract must not learn it -- an actor that opens a pull request
// has no working tree to be dirty, and one that writes to a config service
// has neither. What every actor can answer is whether it would take work, and
// a sentence a person can read when it would not.
type Readiness struct {
	// Ready is whether work offered now would be attempted.
	Ready bool `json:"ready"`
	// Reason is why not, in words meant for a person. Empty when ready.
	Reason string `json:"reason,omitempty"`
}
