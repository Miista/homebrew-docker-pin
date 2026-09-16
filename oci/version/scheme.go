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
	return true, ""
}
