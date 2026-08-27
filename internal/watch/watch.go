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
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/pin"
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
	// KindTag: a constrained service (duva.include) with a newer matching tag.
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
}

// Available reports whether this finding is something a human could act on.
func (f Finding) Available() bool { return f.Status == StatusAvailable }

// Rules are a service's per-service settings, read from its compose labels.
type Rules struct {
	Include string
	Exclude string
	Delay   string
}

// Labels reads a service's duva.* rules.
func Labels(composeFile, service string) (Rules, error) {
	l, err := compose.Labels(composeFile, service)
	if err != nil {
		return Rules{}, err
	}
	return Rules{
		Include: l["duva.include"],
		Exclude: l["duva.exclude"],
		Delay:   l["duva.delay"],
	}, nil
}

// Baseline records the last digest seen for each moving-tag service, so a
// digest move is reported once rather than every run. Constrained services
// need no baseline: the compose file's own tag is the reference.
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
		f.Status, f.Reason = StatusSkipped, "not pinned"
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
		return movingTag(f, reg, baseline)
	}
	return constrained(f, rules, reg)
}

// constrained handles a service with a duva.include regex: the newest matching
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
	if c.Tag == "" {
		f.Status, f.Reason = StatusUpToDate, c.Hold
		return f
	}
	f.Status, f.Kind, f.Candidate = StatusAvailable, KindTag, c.Tag
	return f
}

// movingTag handles a service following a moving tag: compare the tag's
// current manifest digest against the recorded baseline. No pull.
func movingTag(f Finding, reg Registry, baseline Baseline) Finding {
	digest, err := reg.RemoteDigest(f.Image, f.CurrentTag)
	if err != nil {
		return errorf(f, fmt.Errorf("fetching remote digest for %s:%s: %w", f.Image, f.CurrentTag, err))
	}

	// First sight of this service: record where the tag is now and say
	// nothing, or every newly watched service would report an "update" on
	// day one regardless of whether anything moved.
	if baseline[f.Service] == "" {
		baseline[f.Service] = digest
		f.Status, f.Reason = StatusUpToDate, "baseline recorded"
		return f
	}
	if digest == baseline[f.Service] {
		f.Status, f.Reason = StatusUpToDate, "tag still points at the pinned digest"
		return f
	}
	f.Status, f.Kind, f.Candidate = StatusAvailable, KindDigest, digest
	return f
}

func errorf(f Finding, err error) Finding {
	f.Status, f.Reason = StatusError, err.Error()
	return f
}
