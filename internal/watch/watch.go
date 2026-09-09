// Package watch is duva's detection half: given a compose project, work out
// which pinned services have something newer waiting.
//
// It returns findings rather than printing them. duva's original runOnce wrote
// straight to an io.Writer, which was fine when notifying was all it did; now
// the same answers feed a notifier, a web UI and (later) the actor, so they
// have to be data.
//
// Detection is deliberately read-only: nothing here pulls an image, rewrites a
// compose file or touches a container.
package watch

import (
	// Registers SHA-256 with go-digest. Without it every digest validates as
	// "unsupported algorithm", which would reject every pinned service.
	_ "crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/pin"
	"github.com/Miista/homebrew-docker-pin/internal/registry"
)

// Registry is the registry access detection needs, seamed for tests.
type Registry struct {
	ListMatchingTags func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error)
	TagCreated       func(baseImage, tag string) (time.Time, error)
	// RemoteDigest returns the manifest digest a tag currently points at,
	// without pulling.
	RemoteDigest func(baseImage, tag string) (string, error)
}

// Status is why a service is in the state it is.
type Status string

const (
	// StatusUpToDate: nothing newer qualifies.
	StatusUpToDate Status = "up-to-date"
	// StatusAvailable: something newer is waiting.
	StatusAvailable Status = "available"
	// StatusSkipped: not watched — unpinned, or built locally.
	StatusSkipped Status = "skipped"
	// StatusError: the check failed for this service. One service failing
	// must never stop the others.
	StatusError Status = "error"
)

// Kind distinguishes what sort of candidate was found, which decides what can
// be said about it. A moving-tag service has no old/new tag pair, so it can
// never be classified by semver — that is different from a version pair that
// simply could not be parsed.
type Kind string

const (
	// KindTag: a constrained service (duva.include_tags) with a newer matching tag.
	KindTag Kind = "tag"
	// KindDigest: a moving tag (latest, dev) whose digest has moved.
	KindDigest Kind = "digest"
)

// Finding is what detection learned about one service.
type Finding struct {
	Service string
	// File is the compose file the service is declared in, which is not
	// necessarily the project root: include: is resolved.
	File string
	// Image is the base image, CurrentTag and CurrentDigest what the compose
	// file pins today.
	Image, CurrentTag, CurrentDigest string

	Status Status
	// Reason explains a Skipped or UpToDate finding in one clause, and
	// carries the message for an Error.
	Reason string

	// Set when Status is Available:
	Kind Kind
	// Candidate is the newer tag (KindTag) or the new digest (KindDigest).
	Candidate string
	// Bump is how big the change is, for a tag candidate. A digest candidate
	// has no version pair to compare, so it is left empty -- that is a
	// different thing from registry.KindUnknown, which means there IS a pair
	// and it could not be read.
	Bump registry.Kind
	// Decision is whether policy allows duva to apply this itself, and Why
	// says so in one clause fit for a notification or a table cell.
	Decision Decision
	// Auto is the service's duva.auto threshold, carried so the queue can
	// show what the rule IS and not only that a candidate exceeded it. A
	// service is easiest to misconfigure in the direction of applying more
	// than intended, which is invisible if the policy is only ever mentioned
	// in the explanation of what it rejected.
	Auto Auto
	Why  string

	// Soaking is a candidate held back by duva.delay: newer than what is
	// pinned, matching the include, but not yet as old as the service asks
	// for. Empty unless a delay is set and something is waiting on it.
	//
	// Carried because a soak that says nothing looks like nothing found. The
	// wait is a default, not a lock -- an operator who knows a release is
	// fine can take it early, which they cannot do if duva never mentions it.
	Soaking *SoakingTag
}

// SoakingTag is a candidate waiting out its delay.
type SoakingTag struct {
	// Tag is the version being held back, and Age how long since it was
	// published. Delay is what the service asks for, so a reader can see how
	// much longer it has to wait.
	Tag   string
	Age   time.Duration
	Delay time.Duration
}

// NeedsApproval reports whether this finding is waiting on a human.
func (f Finding) NeedsApproval() bool {
	return f.Status == StatusAvailable && f.Decision == DecideApprove
}

// AutoApplies reports whether policy allows duva to apply this itself.
func (f Finding) AutoApplies() bool {
	return f.Status == StatusAvailable && f.Decision == DecideAuto
}

// Available reports whether this finding is something a human could act on.
func (f Finding) Available() bool { return f.Status == StatusAvailable }

// Rules are a service's per-service settings, read from its compose labels.
type Rules struct {
	Include string
	Exclude string
	Delay   string
	Auto    Auto
}

// knownLabels is every duva.* label a service may carry. Anything else under
// the duva. prefix is a typo, and saying so beats ignoring it: a misspelled
// duva.include_tags silently means "follow the moving tag instead", which
// looks like duva working rather than duva misconfigured.
//
// Named to match diun's own duva.include_tags/duva.exclude_tags, which most
// services already carry for that tool -- so pointing duva at the same
// stream is one label to add per service, not a different name to learn.
var knownLabels = map[string]bool{
	"duva.include_tags": true,
	"duva.exclude_tags": true,
	"duva.delay":        true,
	"duva.auto":         true,
}

// Labels reads and validates a service's duva.* rules.
func Labels(composeFile, service string) (Rules, error) {
	l, err := compose.Labels(composeFile, service)
	if err != nil {
		return Rules{}, err
	}
	for k := range l {
		if strings.HasPrefix(k, "duva.") && !knownLabels[k] {
			return Rules{}, fmt.Errorf("unknown label %q", k)
		}
	}

	auto, err := ParseAuto(l["duva.auto"])
	if err != nil {
		return Rules{}, err
	}

	r := Rules{
		Include: l["duva.include_tags"],
		Exclude: l["duva.exclude_tags"],
		Delay:   l["duva.delay"],
		Auto:    auto,
	}

	// A delay bounds which candidate tags qualify, so it is meaningless
	// without a set of candidates to choose from.
	if r.Delay != "" && r.Include == "" {
		return Rules{}, fmt.Errorf("duva.delay requires duva.include_tags")
	}
	// Same for exclude: nothing to exclude from.
	if r.Exclude != "" && r.Include == "" {
		return Rules{}, fmt.Errorf("duva.exclude_tags requires duva.include_tags")
	}
	return r, nil
}

// Baseline records the last digest seen from the registry for each
// moving-tag service, purely to suppress a false "update available" on the
// first check of a newly watched service. It does not decide up-to-date on
// its own, and reporting the same digest twice is not this package's problem
// to solve: whether an operator has already been told about a given
// candidate is duva.State's Notified map, one layer up.
//
// The compose file's own pin is always the truth for "up to date" -- an
// operator (or `docker pin`) can rewrite it at any time outside of duva, and
// baseline advancing on every check, not just on an apply duva itself made,
// is what lets that be noticed rather than masked by a stale memory of a
// digest already seen once. Constrained services need no baseline at all:
// the compose file's own tag is the reference.
type Baseline map[string]string

// Project scans every service reachable from rootFile and reports a finding
// for each. Services are returned in the order compose lists them; callers
// that care about order should sort.
//
// baseline is read and updated in place: a moving-tag service seen for the
// first time records its current digest and reports up-to-date, rather than
// firing on day one for something that has not actually changed.
func Project(rootFile string, reg Registry, baseline Baseline) ([]Finding, error) {
	names, err := compose.ListServices(rootFile)
	if err != nil {
		return nil, err
	}

	findings := make([]Finding, 0, len(names))
	for _, name := range names {
		findings = append(findings, service(rootFile, name, reg, baseline))
	}
	return findings, nil
}

// service checks one service, converting any failure into an Error finding:
// one unreachable registry must not abort the whole run.
func service(rootFile, name string, reg Registry, baseline Baseline) Finding {
	f := Finding{Service: name}

	serviceFile, err := compose.ResolveServiceIn(rootFile, name)
	if err != nil {
		return errorf(f, err)
	}
	f.File = serviceFile

	built, err := compose.IsBuilt(serviceFile, name)
	if err != nil {
		return errorf(f, err)
	}
	if built {
		f.Status, f.Reason = StatusSkipped, "built locally (build:)"
		return f
	}

	raw, err := compose.RawImage(serviceFile, name)
	if err != nil {
		return errorf(f, err)
	}
	// Pin status is the opt-in: an unpinned service has made no versioning
	// decision to watch over.
	if !strings.Contains(raw, "@sha256:") {
		// The image line as written, so the report can show what it looked at
		// rather than leaving the reader to guess which service it meant.
		f.Image = raw
		f.Status, f.Reason = StatusSkipped, "not pinned"
		return f
	}

	// Looking pinned is not the same as being pinned. "@sha256:" is a text
	// match, so `image: x/y:1.0.0@sha256:abc` passes it -- and then the
	// malformed digest is recorded as the baseline and the service reported
	// up-to-date, when the daemon rejects that reference outright ("invalid
	// reference format") and it cannot be pulled at all.
	//
	// Reported as an error rather than skipped: an unpinned service has opted
	// out, which is a choice, but this one meant to opt in and got it wrong.
	// Saying nothing would leave someone believing a service is watched when
	// it is not.
	if d := pin.DigestOf(raw); digest.Digest(d).Validate() != nil {
		f.Image = raw
		return errorf(f, fmt.Errorf(
			"%s is not a valid digest, so this reference cannot be pulled", d))
	}

	// A digest is enough to look pinned, but `image: nginx:${TAG}@sha256:...`
	// is not something duva can reason about: it reads the file as written, so
	// the tag it would ask a registry about is the literal "${TAG}". Skipped
	// rather than errored -- the service is configured in a way this tool does
	// not handle, which is the operator's to know about, not a fault to retry
	// every run.
	if compose.HasUnexpandedVariable(raw) {
		f.Image = raw
		f.Status, f.Reason = StatusSkipped, "image has an unexpanded variable"
		return f
	}

	image, tag, err := compose.ParseImage(serviceFile, name)
	if err != nil {
		return errorf(f, err)
	}
	f.Image, f.CurrentTag, f.CurrentDigest = image, tag, pin.DigestOf(raw)

	rules, err := Labels(serviceFile, name)
	if err != nil {
		return errorf(f, err)
	}

	if rules.Include == "" {
		f = movingTag(f, reg, baseline)
	} else {
		f = constrained(f, rules, reg)
	}
	f.Auto = rules.Auto
	if f.Available() {
		f.Decision, f.Why = Decide(f, rules.Auto)
	}
	return f
}

// constrained handles a service with a duva.include_tags regex: the newest matching
// tag newer than the one pinned, subject to the delay soak.
func constrained(f Finding, rules Rules, reg Registry) Finding {
	c, err := pin.SelectCandidate(f.File, f.Service, pin.Rules{
		Include: rules.Include,
		Exclude: rules.Exclude,
		Delay:   rules.Delay,
	}, pin.Registry{
		ListMatchingTags: reg.ListMatchingTags,
		TagCreated:       reg.TagCreated,
	})
	if err != nil {
		return errorf(f, err)
	}
	// The newest candidate held back by the soak, if any. Reported whether or
	// not something older did qualify: a service can be offered 1.2.0 while
	// 1.3.0 is still soaking, and both are worth seeing.
	if len(c.TooFresh) > 0 {
		if delay, err := pin.ParseDelay(rules.Delay); err == nil {
			f.Soaking = &SoakingTag{
				Tag:   c.TooFresh[0].Tag,
				Age:   c.TooFresh[0].Age,
				Delay: delay,
			}
		}
	}

	if c.Tag == "" {
		f.Status, f.Reason = StatusUpToDate, c.Hold
		return f
	}
	f.Status, f.Kind, f.Candidate = StatusAvailable, KindTag, c.Tag
	f.Bump = registry.Classify(f.CurrentTag, c.Tag)
	return f
}

// movingTag handles a service following a moving tag: compare the tag's
// current manifest digest against the recorded baseline. No pull.
func movingTag(f Finding, reg Registry, baseline Baseline) Finding {
	digest, err := reg.RemoteDigest(f.Image, f.CurrentTag)
	if err != nil {
		return errorf(f, fmt.Errorf("fetching remote digest for %s:%s: %w", f.Image, f.CurrentTag, err))
	}

	// The file's own pin, not just the baseline, is what "up to date" means:
	// the registry serving exactly what is already written is up to date
	// regardless of what baseline last recorded -- covering a pin edited by
	// hand back to a digest duva has already seen (and so would otherwise
	// still be sitting in baseline, masking the fact that the file no longer
	// agrees with it).
	if digest == f.CurrentDigest {
		baseline[f.Service] = digest
		f.Status, f.Reason = StatusUpToDate, "tag still points at the pinned digest"
		return f
	}

	// First sight of this service: record where the tag is now and say
	// nothing, or every newly watched service would report an "update" on
	// day one regardless of whether anything moved.
	if baseline[f.Service] == "" {
		baseline[f.Service] = digest
		f.Status, f.Reason = StatusUpToDate, "baseline recorded"
		return f
	}
	// Past this point the file and the registry disagree, so there is
	// something to report -- once. baseline's only job is that "once": a
	// repeat check that finds the exact same available digest must not
	// re-queue it (the operator already has it in front of them), but the
	// file always wins the actual up-to-date question above, so baseline can
	// never mask a pin that no longer matches what it remembers.
	f.Status, f.Kind, f.Candidate = StatusAvailable, KindDigest, digest
	baseline[f.Service] = digest
	return f
}

func errorf(f Finding, err error) Finding {
	f.Status, f.Reason = StatusError, err.Error()
	return f
}
