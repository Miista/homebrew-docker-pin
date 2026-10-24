package version

import (
	"regexp"
	"strconv"
	"strings"
)

// Scheme is the versioning convention a tag follows.
//
// Detected from the tag a service already runs, so a candidate can be held to
// the same convention: an upstream that publishes both `0.3.22` and a git SHA,
// or both `1.47.0` and `2026.9.1`, offers tags that are not steps along the
// same line. Comparing across conventions produces an answer -- 2026 is
// greater than 0 -- and the answer is meaningless.
type Scheme string

const (
	// SchemeSemVer is a dotted numeric version: 1.2.3, 0.3.22, 8, 17.10.
	// Not strictly semver, which requires three segments; anything counted in
	// releases rather than dates reads the same way.
	SchemeSemVer Scheme = "semver"
	// SchemeCalVer is a version whose leading segment is a year: 2026.9.1,
	// 2026.08.2. Distinguishable from semver only by that magnitude, so only
	// the four-digit form is recognised -- see isCalVer.
	SchemeCalVer Scheme = "calver"
	// SchemeMoving is a tag that is not a version at all but a stream that
	// gets repointed: latest, stable, edge, main. Following one is a decision
	// to take whatever it points at, so there is no version step to size.
	SchemeMoving Scheme = "moving"
	// SchemeUnknown is a tag whose convention cannot be determined: a git SHA,
	// a build id, a branch name. Not an error -- plenty of images publish
	// them -- but nothing can be inferred about whether one supersedes
	// another, so the honest answer is to say so.
	SchemeUnknown Scheme = "unknown"
)

// movingTags are the names conventionally used for a tag that is repointed
// rather than published once.
//
// A fixed list rather than a pattern: "anything not version-shaped" would
// sweep in git SHAs and branch names, which are the opposite case -- they name
// one immutable build, not a stream. Getting that backwards would treat a SHA
// as a moving tag and follow it forever.
var movingTags = map[string]bool{
	"latest": true, "stable": true, "edge": true, "main": true,
	"master": true, "nightly": true, "develop": true, "dev": true,
	"release": true, "rolling": true, "current": true,
}

// bigLeadingSegment is the threshold above which a leading segment is not a
// version number.
//
// No software is on major version 1000. A leading segment at or above it is a
// year (2026.9.1), or a date stamp, or a build counter -- and every one of
// those is a different thing from a release count, so none of them should be
// compared against one. Below it a leading segment is read as a release count,
// including a two-digit year: see isCalVer for why that is deliberate.
const bigLeadingSegment = 1000

// SchemeOf reports which convention a tag follows.
func SchemeOf(tag string) Scheme {
	t := strings.TrimSpace(tag)
	if t == "" {
		return SchemeUnknown
	}
	if movingTags[strings.ToLower(t)] {
		return SchemeMoving
	}
	m := versionCoreRe.FindStringSubmatch(t)
	if m == nil {
		return SchemeUnknown
	}
	// The scheme is read off the numeric core alone; the suffix is enforced by
	// SameScheme instead.
	if suffix := m[2]; suffix != "" && !strings.HasPrefix(suffix, "-") {
		return SchemeUnknown
	}
	//
	// The alternative was a list of suffixes known to qualify a release --
	// -alpine, -rc1, -ls166 -- which is a list that is always missing one.
	// cloudflared publishes 2026.8.2-g803899b and mariadb 10.11.11-ubu2604;
	// both read as unrecognisable until someone notices the service has gone
	// quiet and adds another alternation. The core is what carries the
	// version, and it is the same core in every one of those cases.
	//
	// A suffix must be separated by `-`, which is what distinguishes a
	// qualified version from a string that merely starts with digits.
	// `1.2.3-rc1` is 1.2.3 qualified as rc1; `4bf4de2` is a git SHA whose
	// first character happens to be a 4, and reading it as version 4 would put
	// a commit on the page as an upgrade. That is ofelia's failure with a
	// different tag shape.
	//
	// Discarding it is only safe because SameScheme then requires the two
	// suffixes to be IDENTICAL: a candidate must be numerically greater AND
	// carry the same suffix. So `1.2.3-agent-hub` classifies as semver here
	// and is still refused against `1.2.4`, because a branch build and a
	// release are not the same line even when their cores compare.
	segments := strings.Split(m[1], ".")
	if len(segments) == 1 {
		// A single segment with no dot: `8` (postgres), `22` (node). A version
		// by convention.
		//
		// Above the same threshold it is not. `1234`, `20260916`, `12345678`
		// -- a build counter or a date stamp, and nothing in a bare number
		// says which. Unknown rather than calver: with no second segment there
		// is nothing to corroborate a year against, and calling it calver
		// would license comparing it against 2026.9.1.
		if n, err := strconv.Atoi(segments[0]); err == nil && n >= bigLeadingSegment {
			return SchemeUnknown
		}
		return SchemeSemVer
	}
	if isCalVer(segments[0]) {
		return SchemeCalVer
	}
	return SchemeSemVer
}

// isCalVer reports whether a version core's leading segment is date-shaped.
//
// The leading segment alone decides, and only by magnitude: 2026.9.1 is a year
// because no project is on major version 2026.
//
// Two-digit years are deliberately NOT recognised. Ubuntu tags 24.04 and 24.10,
// which are a year and a month -- but 24.04 is also an entirely ordinary
// major.minor, and nothing in the tag distinguishes them. Telling them apart
// would mean encoding one vendor's convention as though it were the norm, and
// then every project on major version 24 pays for it. Ubuntu departed from the
// convention; the cost of that belongs with the tag, not with this rule.
//
// The consequence is bounded and in the safe direction: a two-digit calver tag
// reads as semver, so it is compared against other semver tags. Both sides of
// such a comparison come from the same repository, so they share whatever
// convention it uses and the ordering still holds. What the rule protects
// against -- comparing 0.3.22 against 2026.9.1 -- needs the four-digit form to
// arise at all.
func isCalVer(leading string) bool {
	n, err := strconv.Atoi(leading)
	return err == nil && n >= bigLeadingSegment
}

// SameScheme reports whether a candidate follows the same convention as the
// tag in use, and why not when it does not.
//
// The rule this exists for: a new version can never be of a different scheme
// than the current one. If the file says 0.3.22, then 2026.9.1 is not a newer
// version of it -- it is a different numbering of the same software, or a
// different artifact entirely, and either way it is not a step to take
// unattended.
//
// An unknown scheme on either side is not a match. That is the honest answer
// rather than the permissive one: nothing can be inferred about whether a git
// SHA supersedes 0.3.22, so nothing is.
func SameScheme(current, candidate string) (bool, string) {
	c, n := SchemeOf(current), SchemeOf(candidate)
	if c == SchemeUnknown {
		return false, "the tag in use, " + current + ", follows no recognised versioning scheme, so nothing can be said about what supersedes it"
	}
	if n == SchemeUnknown {
		return false, candidate + " follows no recognised versioning scheme"
	}
	if c != n {
		return false, candidate + " is " + string(n) + " but " + current + " is " + string(c)
	}
	// Same scheme is not enough: the suffix must match too.
	//
	// A suffix names which artifact of a release this is -- the flavour
	// (-alpine, -slim), the branch it was built from (-agent-hub), the
	// distribution it targets (-ubu2604). Two tags differing in it are not two
	// points on one line however their cores compare: `1.47.0-alpine` to
	// `1.48.0` drops the flavour, and applying it would swap the image out
	// from under a service that asked for alpine.
	//
	// This is what makes discarding the suffix in SchemeOf safe. The core says
	// which scheme; the suffix says which line within it; and a candidate has
	// to match on both.
	cs, ns := suffixOf(current), suffixOf(candidate)
	if !sameSuffixShape(cs, ns) {
		switch {
		case cs == "":
			return false, candidate + " carries " + ns + ", which " + current + " does not"
		case ns == "":
			return false, candidate + " drops the " + cs + " that " + current + " carries"
		default:
			return false, candidate + " carries " + ns + " but " + current + " carries " + cs
		}
	}
	return true, ""
}

// sameSuffixShape reports whether two suffixes name the same line.
//
// Usually identity: -alpine follows -alpine, and a flavour that changes is a
// different artifact. Counters are compared by shape rather than value, since
// that is the whole point of a counter -- -ls43 to -ls44 is a step along
// linuxserver's line, and requiring identity would freeze every service on it.
//
// A commit hash is NOT treated that way, though it looks similar. A tag like
// 2026.8.2-g803899b names one commit and will never be republished, so there
// is nothing for it to move to: it is the tag equivalent of a digest pin.
// Measured, not assumed -- 2026.9.1 and 2026.9.1-ge0efe57 resolve to the SAME
// digest on ghcr.io, so the suffixed form is an alias for the release rather
// than a separate line. Matching -g803899b to -ge0efe57 would invent a
// progression between two names for two different commits.
//
// The consequence for such a service is that nothing is ever offered, which is
// correct and worth saying plainly: whoever pinned to a commit asked for that
// commit. Following releases again means pinning to the bare tag.
func sameSuffixShape(a, b string) bool {
	if a == b {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	return suffixShape(a) == suffixShape(b)
}

// digitRunRe is any run of digits, replaced to compare two suffixes by shape.
var digitRunRe = regexp.MustCompile(`\d+`)

// suffixShape reduces a suffix to what identifies its LINE rather than its
// build: -ls43 and -ls44 are both -ls#, two points on linuxserver's counter.
//
// Digits only, deliberately. A commit hash is NOT reduced, because a tag
// naming a commit is not a line at all -- see SameScheme.
func suffixShape(s string) string {
	return digitRunRe.ReplaceAllString(s, "#")
}

// suffixOf is what follows a tag's numeric core, or "" when it has none.
//
// Read through the same expression SchemeOf and Classify use, so the three
// cannot disagree about where a core ends and a suffix begins.
func suffixOf(tag string) string {
	m := versionCoreRe.FindStringSubmatch(strings.TrimSpace(tag))
	if m == nil {
		return ""
	}
	return m[2]
}
