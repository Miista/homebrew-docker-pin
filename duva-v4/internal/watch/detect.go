// Package detect finds tags that appeared since a moment in time.
//
// It is deliberately ignorant. It does not know semver, it does not know
// calver, it cannot tell you whether 4.39.25 is newer than 4.39.20, and it
// has no opinion about whether anything it reports should be applied. It
// answers one question: which tags of this image were published after T.
//
// That ignorance is the design. Deciding what a tag means -- how big a change
// it is, whether policy allows it -- belongs to the gate, and a watcher that
// also understood versions would be a second thing that had to agree with the
// gate about version ordering. Two implementations of "is this newer" is one
// too many.
//
// It is also stateless past the timestamp. There is no per-service baseline
// of "what digest did this point at last time", which is the thing that makes
// a watcher expensive to lose: rebuilding one means re-listing every tag of
// every repository, and forgetting one means silently failing to report what
// it had already seen. One timestamp costs a noisy run to lose, not a rebuild.
package watch

import (
	"fmt"
	"regexp"
	"time"
)

// Registry is the registry access detecting needs.
//
// A seam, so the watcher can be tested without a network and so a different
// registry client can be substituted. Nothing here parses a version.
type Registry struct {
	// ListTags returns every tag of an image, with its publish time where
	// the registry gives one cheaply.
	//
	// Cheaply is the operative word and the reason this is not two calls.
	// Docker Hub returns the date with the listing, so the whole answer
	// costs one request per page; elsewhere it does not, and the date comes
	// back zero. A watcher that always asked separately would turn one
	// call into one per tag -- hundreds, for a repository like paperless --
	// which is slow enough to read as a hang and fast enough to earn a 429.
	ListTags func(baseImage string) ([]DatedTag, error)
	// TagCreated is when a tag was published, for the registries that did
	// not say. Called only for tags that survived the include and exclude,
	// so the expensive path is walked as little as possible.
	TagCreated func(baseImage, tag string) (time.Time, error)
	// TagDigest is what a tag points at now. Called only for a service
	// following a moving tag, which is the one case where the answer is the
	// finding rather than a detail of it.
	TagDigest func(baseImage, tag string) (string, error)
}

// DatedTag is a tag, and when it was published if that was known for free.
type DatedTag struct {
	Name string
	// Published is zero when the registry did not say, which means the
	// caller must ask -- see Registry.TagCreated.
	Published time.Time
}

// Service is what to look at: an image, the tag it currently follows, and the
// rules for which tags count.
type Service struct {
	// Name is the compose service, carried through so a finding can be
	// matched back to it.
	Name string
	// Container is what the service's container is called, which is how the
	// gate matches a finding to a service.
	Container string
	// Image is the base reference, without tag or digest.
	Image string
	// Tag is what the compose file follows today.
	Tag string
	// Include and Exclude constrain which tags qualify. Nil means no
	// constraint. These are the service's existing labels -- there is no
	// pattern inferred here, because inferring one would be a version
	// opinion and this package has none.
	Include, Exclude *regexp.Regexp
	// Moving says the tag this service follows is a stream rather than a
	// release: latest, main, edge, dev.
	//
	// It changes which tags are worth looking at, not how they are compared
	// -- this package still knows nothing about versions. A service tracking
	// a stream has already chosen what it follows, so the only tag that can
	// tell it anything is that same tag: the question is whether what it
	// points at has changed. Every other tag in the repository is a different
	// stream, and offering one would be proposing to change the instruction
	// the file records.
	//
	// Decided by the caller, which is where the version knowledge lives.
	Moving bool
	// Digest is what the followed tag resolved to when the file was written,
	// so a moving tag that still points there is not a finding.
	Digest string
}

// Finding is one tag that appeared after the cutoff.
type Finding struct {
	Service   string
	Container string
	Image     string
	// Tag is the tag that appeared.
	Tag string
	// Published is when it appeared.
	Published time.Time
	// Digest is what the tag points at, set only for a moving tag whose
	// digest has changed. Empty for a new tag, where the consumer has a tag
	// to move to and needs nothing resolved.
	Digest string
}

// A failure is returned rather than reported as an absence: a registry that
// cannot be reached is a hole in the answer, and a hole that looks like
// "nothing new" is the failure this whole tool exists to prevent.

// Since reports the tags of one service published after the cutoff, calling
// found for each as it is discovered.
//
// Streamed rather than returned, because checking one service is slow: a
// repository with hundreds of tags costs a request each where the registry
// does not give dates with its listing, and a function that collected
// everything before returning left the caller silent for a minute. Silence
// that long is indistinguishable from a hang -- which cost three
// misdiagnoses before anyone noticed it was merely slow.
//
// Every tag is considered, then filtered by the service's own include and
// exclude, then by time. Order matters: filtering by time first would mean
// asking when every tag was published, which is the expensive half, to
// answer a question the include pattern dismisses for free.
//
// Findings arrive in whatever order the registry listed them. Sorting would
// mean collecting first, which is the thing this signature exists to avoid.
func Since(svc Service, cutoff time.Time, reg Registry, found func(Finding)) error {
	return since(svc, cutoff, false, reg, found)
}

// AtLeastOne is Since, except that a service it has never checked reports its
// newest qualifying tag even when that tag is older than the cutoff.
//
// The window exists so a first run is not buried in every tag ever published.
// But a service whose last release was eight days ago then reports nothing,
// which reads as "up to date" when it is not -- and stays that way until
// someone publishes again, which for a quiet project may be never.
//
// Only where a window can hide something. A moving tag is answered by
// comparing digests and never consults the cutoff at all, so it already
// always reports; this is for version tags alone.
//
// At most one, and only when the window found nothing: a service that is
// genuinely current reports the tag it is already running, which the queue
// recognises and ignores. That comparison lives there, where the version
// knowledge is -- a second copy here would be one that could disagree.
func AtLeastOne(svc Service, cutoff time.Time, reg Registry, found func(Finding)) error {
	return since(svc, cutoff, true, reg, found)
}

func since(svc Service, cutoff time.Time, atLeastOne bool, reg Registry, found func(Finding)) error {
	// A service following a stream has already chosen what it follows. The
	// only question is whether that stream now points somewhere else, which
	// is one request -- and listing the repository's other tags would only
	// produce candidates that change which stream it follows.
	if svc.Moving {
		return sinceMoved(svc, reg, found)
	}

	tags, err := reg.ListTags(svc.Image)
	if err != nil {
		return fmt.Errorf("listing tags for %s: %w", svc.Image, err)
	}

	// The newest qualifying tag, whatever its age, kept in case the cutoff
	// discards everything. Tracked as it goes rather than by sorting
	// afterwards, which would mean collecting first -- the thing this
	// signature exists to avoid.
	var newest Finding
	var newestAt time.Time

	var reported int
	for _, t := range tags {
		if !qualifies(t.Name, svc) {
			continue
		}

		published := t.Published
		if published.IsZero() {
			// The registry did not say, so it has to be asked -- one request
			// for this tag. Reached only after the include and exclude have
			// had their say, which is what keeps a repository with hundreds
			// of tags from costing hundreds of calls.
			var err error
			published, err = reg.TagCreated(svc.Image, t.Name)
			if err != nil {
				// One tag whose date cannot be read is not a reason to give
				// up on the service. It is skipped -- which risks missing
				// it, and is the lesser of two bad options against reporting
				// a tag whose age is unknown as though it were new.
				continue
			}
		}

		f := Finding{
			Service:   svc.Name,
			Container: svc.Container,
			Image:     svc.Image,
			Tag:       t.Name,
			Published: published,
		}
		if published.After(newestAt) {
			newest, newestAt = f, published
		}
		if !published.After(cutoff) {
			continue
		}
		reported++
		found(f)
	}

	// Nothing was new enough, and this service has never been checked. Report
	// the newest there is, so a first run says what exists rather than
	// nothing.
	if atLeastOne && reported == 0 && !newestAt.IsZero() {
		found(newest)
	}
	return nil
}

// sinceMoved reports a moving tag whose digest has changed.
//
// There is no cutoff here, and that is deliberate rather than an omission. A
// digest move has no publish date the registry will tell us cheaply, and it
// needs none: the file records what the tag pointed at, so "has it moved" is
// answered by comparing against that, not against a clock. A move that
// happened before the last check is still a move the file does not reflect.
//
// One request, where the version path costs a listing plus a date per tag.
func sinceMoved(svc Service, reg Registry, found func(Finding)) error {
	if reg.TagDigest == nil {
		// A registry seam that cannot resolve digests cannot answer the only
		// question a moving tag raises. Silence would read as "nothing has
		// changed", which is the failure this tool exists to prevent.
		return fmt.Errorf("cannot check %s: it follows the moving tag %q and this registry cannot resolve digests",
			svc.Name, svc.Tag)
	}
	digest, err := reg.TagDigest(svc.Image, svc.Tag)
	if err != nil {
		return fmt.Errorf("resolving %s:%s: %w", svc.Image, svc.Tag, err)
	}
	// Unpinned, so there is nothing recorded to have moved away from. The
	// gate ignores these anyway -- a service that made no versioning decision
	// has none to watch over -- but reporting one would be inventing a change
	// out of the absence of a record.
	if svc.Digest == "" {
		return nil
	}
	if digest == svc.Digest {
		return nil
	}
	found(Finding{
		Service:   svc.Name,
		Container: svc.Container,
		Image:     svc.Image,
		Tag:       svc.Tag,
		Digest:    digest,
	})
	return nil
}

// qualifies applies the service's own include and exclude.
//
// The tag it already follows is never a finding: the file says it is on that
// one, so reporting it would be telling the operator about where they already
// are.
func qualifies(tag string, svc Service) bool {
	if tag == svc.Tag {
		return false
	}
	if svc.Include != nil && !svc.Include.MatchString(tag) {
		return false
	}
	if svc.Exclude != nil && svc.Exclude.MatchString(tag) {
		return false
	}
	return true
}
