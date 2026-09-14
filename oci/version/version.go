package version

import (
	"regexp"
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

// versionCoreRe splits a tag into its numeric dotted core and the rest,
// tolerating a leading "v".
var versionCoreRe = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)(.*)$`)

// CompareVersions orders two version-ish tags: the dotted numeric cores are
// compared numerically segment by segment (missing segments count as 0), and
// on equal cores a tag with a suffix ranks BELOW the bare tag, so a build or
// prerelease like 2026.6.1-g8487590 sorts before 2026.6.1. Differing suffixes
// compare lexically. Tags without a numeric core compare lexically as a whole.
// Returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	ma, mb := versionCoreRe.FindStringSubmatch(a), versionCoreRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return strings.Compare(a, b)
	}
	sa, sb := strings.Split(ma[1], "."), strings.Split(mb[1], ".")
	for i := 0; i < len(sa) || i < len(sb); i++ {
		var na, nb int
		if i < len(sa) {
			na, _ = strconv.Atoi(sa[i])
		}
		if i < len(sb) {
			nb, _ = strconv.Atoi(sb[i])
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	switch {
	case ma[2] == mb[2]:
		return 0
	case ma[2] == "":
		return 1 // bare release beats any suffixed build of the same core
	case mb[2] == "":
		return -1
	default:
		return compareSuffixes(ma[2], mb[2])
	}
}

var suffixRunRe = regexp.MustCompile(`\d+|\D+`)

// compareSuffixes orders version suffixes like rpm/dpkg do: split into
// alternating numeric and non-numeric runs, comparing numeric runs
// numerically — so -ls100 > -ls99 and -r10 > -r2, where a plain lexical
// compare would invert them.

func compareSuffixes(a, b string) int {
	ra, rb := suffixRunRe.FindAllString(a, -1), suffixRunRe.FindAllString(b, -1)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		na, ea := strconv.Atoi(ra[i])
		nb, eb := strconv.Atoi(rb[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		default:
			if c := strings.Compare(ra[i], rb[i]); c != 0 {
				return c
			}
		}
	}
	// Equal prefix runs: the longer suffix ranks higher (more specific build).
	switch {
	case len(ra) == len(rb):
		return 0
	case len(ra) < len(rb):
		return -1
	default:
		return 1
	}
}

// NewestMatching returns the highest tag (per CompareVersions) that matches
// include and is strictly newer than current, or "" when no tag qualifies.

// IsVersion reports whether a tag names a specific release, as opposed to a
// stream that moves.
//
// The distinction decides what an update to the service even means. A tag
// like 1.29.0 is a point: a newer release is a different tag, and updating
// means changing the tag. A tag like latest, main, edge or dev is a stream:
// it is already pointing at whatever is newest, so there is no newer tag to
// move to -- the update is that the same tag now resolves to a different
// digest.
//
// Conflating the two produces nonsense candidates. A service on `latest` was
// offered `latest -> edge` and `latest -> pr-185`, which is not an upgrade
// but a change of which stream the service follows -- a decision the file
// already recorded and nothing here should overrule. The tag is the
// instruction; only a person changes an instruction.
//
// The test is a numeric core, the same one Classify compares on: `1`, `1.29`,
// `v1.29.0`, `2026.08.1` and `1.2.3-alpine` are versions; `latest`, `main`,
// `edge`, `stable`, `nightly` and `pr-185` are not.
// A multi-segment core is decisive on its own: nothing names a stream
// `1.29.0`. A single-segment core is not -- `v3-agent-hub` and `v2-logo` are
// branch builds, not releases of 3 and 2 -- so a bare-word suffix there is
// read as a name rather than a qualifier. The cost of being wrong is a
// digest-based update where a version comparison was possible, which is the
// safe direction: it changes no tag.
func IsVersion(tag string) bool {
	m := versionCoreRe.FindStringSubmatch(tag)
	if m == nil {
		return false
	}
	core, suffix := m[1], m[2]
	if strings.Contains(core, ".") {
		return true
	}
	// Single-segment core. A qualifier suffix (-alpine, -rc1, or none) still
	// reads as a version; anything else is a name that happens to start with
	// a digit.
	return suffix == "" || qualifierRe.MatchString(suffix)
}

// qualifierRe is a suffix that qualifies a release rather than naming a
// stream: a flavour (-alpine, -slim), a prerelease (-rc1, -beta2), or a build
// counter (-ls166). Deliberately narrow -- it must not match a branch name
// like -agent-hub, which is the thing it exists to reject.
var qualifierRe = regexp.MustCompile(`^-(alpine|slim|bookworm|bullseye|ubuntu|debian|distroless|rc\d*|beta\d*|alpha\d*|ls\d+)$`)
