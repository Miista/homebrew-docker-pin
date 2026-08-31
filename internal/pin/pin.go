// Package pin is the shared write engine: it decides what a compose service's
// image line should become and rewrites it.
//
// It is deliberately silent — no printing, no notifications, no policy. It
// answers "what would this become, and did it change?" and performs the write.
// Everything about presentation belongs to the caller, because there are two
// with very different needs: `docker pin`, a CLI reporting to a human, and
// duva, a daemon deciding whether it is allowed to act at all.
//
// This is the half that pin/unpin and duva share. They do not share a *read*
// path — pin/unpin read `image:` and `build:`, duva also reads labels — so
// only the write side lives here.
package pin

import (
	"fmt"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"regexp"
	"strconv"
	"time"
)

// Docker is the subset of docker operations the engine needs, as a seam so
// callers (and tests) can supply their own.
type Docker struct {
	// GetDigest returns the repo digest of a locally present image.
	GetDigest func(ref string) (string, error)
	// Pull fetches an image reference.
	Pull func(ref string) error
}

// Outcome describes what an upgrade did, or in a dry run would do.
type Outcome struct {
	// OldRaw and NewRaw are the image references as written in the compose
	// file, before and after. They are equal when nothing changed.
	OldRaw, NewRaw string
	// Tag and Digest are what the service is (or would be) pinned to.
	Tag, Digest string
	// Changed is true when the pin moved, or would move in a dry run.
	Changed bool
	// Built is true when the service is built locally (`build:`) and was
	// therefore skipped: a local image's digest is not served by any
	// registry, so pinning it yields a reference no other host can pull.
	Built bool
}

// Compute works out what a service's image line would become, given a pullRef
// that has already been pulled. It performs no writes.
//
// The tag written is the tag of pullRef — the tag is an instruction (which
// stream to follow), not a record of what runs, so it is never replaced by
// some other tag discovered from the digest. See "The tag is the tag to
// follow" in the README.
func Compute(composeFile, service, pullRef string, d Docker) (Outcome, error) {
	built, err := compose.IsBuilt(composeFile, service)
	if err != nil {
		return Outcome{}, err
	}
	if built {
		raw, err := compose.RawImage(composeFile, service)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{OldRaw: raw, NewRaw: raw, Built: true}, nil
	}

	baseImage, currentTag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return Outcome{}, err
	}
	oldRaw, err := compose.RawImage(composeFile, service)
	if err != nil {
		return Outcome{}, err
	}

	out := Outcome{OldRaw: oldRaw, NewRaw: oldRaw, Tag: currentTag, Digest: DigestOf(oldRaw)}

	digest, err := d.GetDigest(pullRef)
	if err != nil {
		return out, err
	}
	if oldDigest := DigestOf(oldRaw); oldDigest != "" && oldDigest == digest {
		return out, nil // already at this digest
	}

	pullTag := strings.TrimPrefix(pullRef, baseImage+":")
	out.NewRaw = fmt.Sprintf("%s:%s@%s", baseImage, pullTag, digest)
	out.Tag = pullTag
	out.Digest = digest
	out.Changed = true
	return out, nil
}

// Apply writes the computed image line to the compose file. Separate from
// Compute so callers can compute concurrently and serialise the writes.
func Apply(composeFile, service string, out Outcome) error {
	return compose.PinImage(composeFile, service, out.NewRaw)
}

// DigestOf extracts "sha256:..." from an "image@sha256:..." reference, or ""
// if the reference carries no digest.
func DigestOf(image string) string {
	if i := strings.Index(image, "@"); i != -1 {
		return image[i+1:]
	}
	return ""
}

// TagOf extracts the tag from "base:tag" or "base:tag@sha256:...".
func TagOf(image string) string {
	if i := strings.Index(image, "@"); i != -1 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i != -1 {
		// A colon before the last slash is a registry port, not a tag.
		if !strings.Contains(image[i:], "/") {
			return image[i+1:]
		}
	}
	return ""
}

// ShortDigest abbreviates "sha256:<64 hex>" to its first 12 hex characters.
func ShortDigest(digest string) string {
	const prefix = "sha256:"
	if strings.HasPrefix(digest, prefix) && len(digest) > len(prefix)+12 {
		return digest[:len(prefix)+12]
	}
	return digest
}

// duva.delay is how long a release must have been out before duva will take
// it. It lived in internal/schedule while `docker pin schedule` owned the
// concept of waiting; the label is duva's, so the parser is too.

var delayRe = regexp.MustCompile(`^(\d+)(h|d|w)$`)

// ParseDelay parses a release-age delay: "48h", "7d" or "2w".
//
// Days and weeks rather than only hours because that is how the wait is
// actually chosen -- "a week" is a decision, "168h" is arithmetic.
func ParseDelay(s string) (time.Duration, error) {
	m := delayRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid delay %q (use e.g. \"48h\", \"7d\", \"2w\")", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, err
	}
	unit := time.Hour
	switch m[2] {
	case "d":
		unit = 24 * time.Hour
	case "w":
		unit = 7 * 24 * time.Hour
	}
	return time.Duration(n) * unit, nil
}
