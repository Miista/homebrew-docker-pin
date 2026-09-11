// Package decide is the gate.
//
// It answers one question: a detector says a version of some image exists —
// given what this host declares it is running, is that an update, how big is
// it, and may it be applied without asking anyone?
//
// Everything here is a pure function. Nothing reads a file, opens a socket,
// or talks to a registry: the caller gathers the world and passes it in, and
// gets a decision back. That is not tidiness for its own sake. The decider is
// the only component that says whether a container may be replaced, and a
// function whose answer depends only on its arguments is one you can enumerate
// the behaviour of — which is what the tests in this package do.
//
// The I/O — parsing compose files, receiving webhooks, holding the queue —
// lives in the caller. See the duva-v4 design note.
package decide

import (
	"fmt"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/registry"
)

// Notice is what a detector reported: this container's image has something
// newer behind it.
//
// Deliberately not diun's payload type. A detector is a replaceable part, and
// the decider should not know which one it has — so the caller translates
// whatever arrived into this, and only the translation changes if the detector
// does.
type Notice struct {
	// Container is the container's name, which is how a notice is matched to
	// a service: an image reference is not unique (two services on one host
	// can share an image), and a container name is.
	Container string
	// Image is the full reference the detector saw, tag included.
	Image string
	// Digest is what that reference resolves to now.
	Digest string
}

// Service is what the compose file declares for one service: the tag it
// follows, the digest it is pinned to, and the policy on it.
type Service struct {
	// Name is the compose service name.
	Name string
	// File is the compose file it is declared in — not necessarily the
	// project root, since include: is resolved.
	File string
	// Image is the base reference without tag or digest.
	Image string
	// Tag is the tag the file follows. "The tag is the tag to follow": it is
	// an instruction, not a description of what is pinned.
	Tag string
	// Digest is what the file pins, empty if the service is not pinned.
	Digest string
	// Auto is how large a change may be applied without asking.
	Auto Auto
}

// Pinned reports whether the service has opted in to being watched.
//
// Pin status is the opt-in, as it is in duva today: an unpinned service has
// made no versioning decision, so there is nothing to have an opinion about.
func (s Service) Pinned() bool { return s.Digest != "" }

// Verdict is what the decider concluded about one notice.
type Verdict struct {
	// Outcome says what should happen.
	Outcome Outcome
	// Kind is how big the change is, for a tag change. Empty for a digest
	// move, which has no version pair to compare — which is a different thing
	// from registry.KindUnknown, meaning there IS a pair and it could not be
	// read.
	Kind registry.Kind
	// From and To are what it would change, for the queue to render and the
	// actor to apply. For a tag change these are tags; for a digest move they
	// are digests.
	From, To string
	// Why says the reason in one clause, fit for a queue row or a log line.
	Why string
}

// Outcome is the decision itself.
type Outcome string

const (
	// Apply: policy allows this unattended. The actor is told.
	Apply Outcome = "apply"
	// Queue: it needs a human. It waits where someone can see it.
	Queue Outcome = "queue"
	// Ignore: nothing to do — already on it, not watched, not comparable as
	// an update.
	Ignore Outcome = "ignore"
	// Reject: the notice does not describe anything this host has. Distinct
	// from Ignore because it means something is wrong rather than nothing
	// being needed.
	Reject Outcome = "reject"
)

// Decide is the whole gate, as one function.
//
// The order matters and is worth reading as a sequence of questions:
//
//  1. Do we know this container at all?          -> Reject
//  2. Has it opted in by being pinned?           -> Ignore
//  3. Is what arrived actually different?        -> Ignore
//  4. How big a change is it?
//  5. Does policy allow it unattended?           -> Apply or Queue
func Decide(n Notice, svc Service, found bool) Verdict {
	// A notice naming a container this host does not declare is not a
	// non-event. Either the detector is watching something the compose file
	// does not describe, or a service lost its container_name and became
	// invisible. Both are worth saying out loud rather than dropping: a
	// silently ignored notice looks exactly like nothing having happened.
	if !found {
		return Verdict{
			Outcome: Reject,
			Why:     fmt.Sprintf("no service declares a container named %q", n.Container),
		}
	}

	if !svc.Pinned() {
		return Verdict{
			Outcome: Ignore,
			Why:     "not pinned, so it made no versioning decision to watch over",
		}
	}

	newTag := tagOf(n.Image)

	// Same tag, same digest: the detector is telling us what we already have.
	// This is the common case on a re-notify, and it is how "already handled"
	// is answered without anyone keeping a flag.
	if newTag == svc.Tag && n.Digest == svc.Digest {
		return Verdict{Outcome: Ignore, Why: "already on it"}
	}

	// Same tag, different digest: a moving tag moved. There is no version
	// pair, so there is nothing to classify -- choosing `latest` is itself
	// the decision to ride the edge.
	if newTag == svc.Tag {
		return digestMove(n, svc)
	}

	// Different tag: a version change, which can be sized.
	return tagChange(n, svc, newTag)
}

// digestMove decides a moving tag whose digest has moved.
//
// There is no bump to threshold on, so the only question is whether the
// service opted out entirely. Anything else applies: following a moving tag
// is the decision to take what it points at, and asking for approval on top
// would be pretending to a control already declined.
func digestMove(n Notice, svc Service) Verdict {
	v := Verdict{From: svc.Digest, To: n.Digest}
	if svc.Auto == AutoNone {
		v.Outcome, v.Why = Queue, "digest moved; duva.auto is none"
		return v
	}
	v.Outcome, v.Why = Apply, "digest moved on a followed tag"
	return v
}

// tagChange decides a change from one version tag to another.
func tagChange(n Notice, svc Service, newTag string) Verdict {
	kind := registry.Classify(svc.Tag, newTag)
	v := Verdict{Kind: kind, From: svc.Tag, To: newTag}

	// Not every tag change is an upgrade. A detector reporting an older tag,
	// or one on a different flavour line, is not something to apply -- and
	// Classify says so by refusing to compare them.
	if kind != registry.KindUnknown && registry.CompareVersions(newTag, svc.Tag) <= 0 {
		v.Outcome = Ignore
		v.Why = fmt.Sprintf("%s is not newer than %s", newTag, svc.Tag)
		return v
	}

	if svc.Auto == AutoNone {
		v.Outcome, v.Why = Queue, string(kind)+"; duva.auto is none"
		return v
	}
	if rank(kind) <= svc.Auto.rank() {
		v.Outcome = Apply
		v.Why = fmt.Sprintf("%s, within duva.auto: %s", kind, svc.Auto)
		return v
	}
	if kind == registry.KindUnknown {
		v.Outcome = Queue
		v.Why = fmt.Sprintf("version change could not be classified; needs duva.auto: %s", AutoMajor)
		return v
	}
	v.Outcome = Queue
	v.Why = fmt.Sprintf("%s exceeds duva.auto: %s", kind, svc.Auto)
	return v
}

// tagOf returns the tag from a full image reference, defaulting to latest.
//
// Here rather than in a shared helper because the decider's idea of a tag is
// narrow: it does not resolve, normalise or validate a reference, it reads the
// one field it compares on.
func tagOf(ref string) string {
	// Strip any digest first: a reference can carry both, and the tag is what
	// precedes the "@".
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	// The last colon after the last slash is the tag separator. A colon
	// before a slash is a registry port (localhost:5000/app), not a tag.
	slash := strings.LastIndexByte(ref, '/')
	if i := strings.LastIndexByte(ref, ':'); i > slash {
		return ref[i+1:]
	}
	return "latest"
}
