package registry

import (
	"strconv"
	"strings"
)

// Kind is how big a version change is.
type Kind string

const (
	KindPatch   Kind = "patch"
	KindMinor   Kind = "minor"
	KindMajor   Kind = "major"
	KindUnknown Kind = "unknown"
	// KindNone means the two tags are the same version.
	KindNone Kind = "none"
)

// Classify reports how big the step from one tag to another is, by comparing
// their numeric cores position by position: a change in the first segment is
// major, the second minor, the third or beyond patch.
//
// It returns KindUnknown rather than guessing when the pair cannot be
// compared this way — either tag lacking a numeric core, or the two carrying
// different non-numeric suffixes (`1.2.3-alpine` against `1.2.4-slim` is a
// change of image flavour, not a version bump). Callers should treat unknown
// as needing a human: the safe direction is to ask.
//
// Two known misreads, both deliberate:
//
//   - CalVer. `2026.07.2` -> `2026.08.0` is a month bump, which reads here as
//     a minor. Correcting it would mean recognising date-shaped versions,
//     which is guesswork on a tag that looks identical to a semver one.
//   - Two-segment versions. `17.10` -> `17.11` reads as a minor, though
//     upstreams that version this way (postgres, among others) call it a
//     patch. Nothing in the tag distinguishes "17.11 is the eleventh minor of
//     17" from "17.11 is a patch of 17.10".
//
// Both err toward reporting a *larger* change than reality, so the failure
// mode is an unnecessary approval rather than an unattended surprise.
func Classify(from, to string) Kind {
	mf, mt := versionCoreRe.FindStringSubmatch(from), versionCoreRe.FindStringSubmatch(to)
	if mf == nil || mt == nil {
		return KindUnknown
	}

	// A differing suffix means these are not two points on one version line:
	// the flavour changed (-alpine vs -slim), or one is a prerelease of the
	// other. Only an identical suffix (including both empty) is comparable.
	// The linuxserver style, 1.14.7-ls166 -> 1.14.8-ls167, differs in both
	// core and suffix and so is unknown by this rule — correctly, since the
	// build counter carries no version meaning on its own.
	if mf[2] != mt[2] {
		return KindUnknown
	}

	fs, ts := strings.Split(mf[1], "."), strings.Split(mt[1], ".")
	for i := 0; i < len(fs) || i < len(ts); i++ {
		a, b := segment(fs, i), segment(ts, i)
		if a == b {
			continue
		}
		switch i {
		case 0:
			return KindMajor
		case 1:
			return KindMinor
		default:
			return KindPatch
		}
	}
	return KindNone
}

// segment returns the i'th numeric segment, treating a missing one as 0 so
// that 1.2 and 1.2.0 compare equal.
func segment(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return 0
	}
	return n
}
