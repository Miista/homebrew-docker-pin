package version

import (
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
	// 2026.08.2, 24.04. Distinguishable from semver only by that magnitude,
	// which is why the range is deliberately narrow.
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
// compared against one. Below it the two are genuinely ambiguous and the
// second segment has to decide.
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
	core, suffix := m[1], m[2]
	// The same qualifier rule IsVersion applies: a suffix that names a flavour
	// or a prerelease still leaves a version, while anything else is a name
	// that happens to start with digits.
	if suffix != "" && !qualifierRe.MatchString(suffix) {
		return SchemeUnknown
	}
	segments := strings.Split(core, ".")
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
	if isCalVer(segments) {
		return SchemeCalVer
	}
	return SchemeSemVer
}

// isCalVer reports whether a segmented core is date-shaped.
//
// Only the leading segment is consulted for the four-digit form: 2026.9.1 is a
// year and a project is not on major version 2026. The two-digit form needs
// corroboration from the second segment, since 24.04 and 24.0 are the same
// shape and only one is a date.
func isCalVer(segments []string) bool {
	// A leading segment of 1000 or more is never a release count: no software
	// is on major version 1000. Whatever it is -- a year, a date, a build id
	// -- it is not a line that a semver tag is a point on, which is the
	// distinction this draws.
	if n, err := strconv.Atoi(segments[0]); err == nil && n >= bigLeadingSegment {
		return true
	}
	if len(segments) < 2 || len(segments[0]) != 2 {
		return false
	}
	year, err := strconv.Atoi(segments[0])
	if err != nil || year < 20 || year > 99 {
		return false
	}
	// The second segment must be a month. Two digits, because that is what a
	// date-shaped tag writes -- `24.04` and `24.10` are both Ubuntu releases,
	// while `24.0` and `24.1` are a major and its minors. The padding alone is
	// not the signal (10 through 12 are not padded); the width is.
	if len(segments[1]) != 2 {
		return false
	}
	month, err := strconv.Atoi(segments[1])
	return err == nil && month >= 1 && month <= 12
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
	return true, ""
}
