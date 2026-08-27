package pin

import (
	"fmt"
	"regexp"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/registry"
	"github.com/Miista/homebrew-docker-pin/internal/schedule"
)

// MaxDelayChecks bounds how many candidate publish dates one service may
// query per run when walking past too-fresh releases. Registries rate-limit,
// and a service whose ten newest tags are all too fresh is not going to be
// upgraded this run anyway.
const MaxDelayChecks = 10

// Registry is the subset of registry access candidate selection needs, seamed
// so tests need no network.
type Registry struct {
	// ListMatchingTags lists tags for baseImage matching include (and not
	// exclude) that are newer than current.
	ListMatchingTags func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error)
	// TagCreated reports when a tag was published.
	TagCreated func(baseImage, tag string) (time.Time, error)
}

// Rules constrain which registry tags a service may upgrade to. They come
// from compose labels (duva) or a pin.yaml entry (docker pin schedule); this
// package does not care which.
type Rules struct {
	// Include is a regex a candidate tag must match. Required — an empty
	// Include means the service follows a moving tag instead, which is not
	// candidate selection at all.
	Include string
	// Exclude drops otherwise-matching candidates.
	Exclude string
	// Delay is a release-age soak ("48h", "7d", "2w"): only adopt a tag
	// published at least this long ago.
	Delay string
}

// Candidate is the chosen upgrade target for a constrained service.
type Candidate struct {
	// Tag is the tag to upgrade to, or "" when nothing qualifies.
	Tag string
	// Hold explains, in one human-readable clause, why nothing qualified.
	// Empty when Tag is set.
	Hold string
	// TooFresh lists candidates skipped for not having soaked long enough,
	// newest first, so a caller can report what it is waiting on.
	TooFresh []FreshTag
}

// FreshTag is a candidate rejected by the delay soak.
type FreshTag struct {
	Tag string
	Age time.Duration
}

// SelectCandidate picks the upgrade target for a constrained service: the
// newest tag matching Include (and not Exclude) that is newer than the tag
// currently pinned, subject to the Delay soak.
//
// It never falls back to a moving tag, so the constraint cannot be silently
// escaped — a service pinned to `^17\.\d+-alpine$` will not jump to 18
// because nothing matched.
func SelectCandidate(composeFile, service string, rules Rules, reg Registry) (Candidate, error) {
	baseImage, currentTag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return Candidate{}, err
	}

	include, exclude, err := compileFilters(rules)
	if err != nil {
		return Candidate{}, err
	}

	tags, err := reg.ListMatchingTags(baseImage, include, exclude, currentTag)
	if err != nil {
		return Candidate{}, fmt.Errorf("listing tags for %s: %w", baseImage, err)
	}
	candidates := registry.MatchingCandidates(tags, include, exclude, currentTag)
	if len(candidates) == 0 {
		return Candidate{Hold: fmt.Sprintf("no tag newer than %s matches %s", currentTag, rules.Include)}, nil
	}
	if rules.Delay == "" {
		return Candidate{Tag: candidates[0]}, nil
	}

	delay, err := schedule.ParseDelay(rules.Delay)
	if err != nil {
		return Candidate{}, err
	}
	if len(candidates) > MaxDelayChecks {
		candidates = candidates[:MaxDelayChecks]
	}

	var tooFresh []FreshTag
	for _, tag := range candidates {
		created, err := reg.TagCreated(baseImage, tag)
		if err != nil {
			return Candidate{}, fmt.Errorf("publish date for %s:%s: %w", baseImage, tag, err)
		}
		age := time.Since(created)
		if age >= delay {
			return Candidate{Tag: tag, TooFresh: tooFresh}, nil
		}
		tooFresh = append(tooFresh, FreshTag{Tag: tag, Age: age})
	}
	return Candidate{
		Hold:     fmt.Sprintf("%d newer tag(s) match %s but none is %s old yet", len(candidates), rules.Include, rules.Delay),
		TooFresh: tooFresh,
	}, nil
}

func compileFilters(rules Rules) (include, exclude *regexp.Regexp, err error) {
	if include, err = regexp.Compile(rules.Include); err != nil {
		return nil, nil, err
	}
	if rules.Exclude != "" {
		if exclude, err = regexp.Compile(rules.Exclude); err != nil {
			return nil, nil, err
		}
	}
	return include, exclude, nil
}
