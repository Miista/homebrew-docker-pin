package version

import "testing"

func TestSchemeOf(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want Scheme
	}{
		// SemVer, in the shapes images actually publish.
		{"1.2.3", SchemeSemVer},
		{"0.3.22", SchemeSemVer},
		{"v1.2.3", SchemeSemVer},
		{"1.47.0-alpine", SchemeSemVer},
		{"8", SchemeSemVer},
		{"8-alpine", SchemeSemVer},
		{"17.10", SchemeSemVer},
		{"1.2.3-rc1", SchemeSemVer},

		// CalVer. The four-digit year is unambiguous; a project is not on
		// major version 2026.
		{"2026.9.1", SchemeCalVer},
		{"2026.08.2", SchemeCalVer},
		{"2024.1.0", SchemeCalVer},
		// Ubuntu's 24.04 is a year and a month, and reads as semver here on
		// purpose -- see TestTwoDigitYearsAreNotRecognised.
		{"24.04", SchemeSemVer},
		{"22.04", SchemeSemVer},

		// Moving tags: a stream, not a version.
		{"latest", SchemeMoving},
		{"stable", SchemeMoving},
		{"edge", SchemeMoving},
		{"main", SchemeMoving},
		{"LATEST", SchemeMoving},

		// Unknown: nothing can be inferred.
		{"a573727", SchemeUnknown}, // ofelia's git SHAs
		{"b6d03ed", SchemeUnknown},
		{"sha-abc1234", SchemeUnknown},
		{"12345678", SchemeUnknown}, // a build number, not a version
		{"20260916", SchemeUnknown}, // a date stamp, same problem
		{"1.2.3-agent-hub", SchemeUnknown},
		{"", SchemeUnknown},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			if got := SchemeOf(tc.tag); got != tc.want {
				t.Errorf("SchemeOf(%q) = %s, want %s", tc.tag, got, tc.want)
			}
		})
	}
}

// No software is on major version 1000, so a leading segment at or above it is
// not a release count -- it is a year, a date stamp or a build id. Each is a
// different thing from a version, and the point of saying so is that none of
// them may be compared against one.
func TestALeadingSegmentOfAThousandOrMoreIsNotAVersion(t *testing.T) {
	for _, tag := range []string{"2026.9.1", "1000.1", "12345.0", "99999.2.1"} {
		if got := SchemeOf(tag); got == SchemeSemVer {
			t.Errorf("SchemeOf(%q) = %s: a leading segment >= 1000 is not a release count", tag, got)
		}
	}
	// Just below it, and ordinary. A project on major version 999 is unusual
	// and not impossible, and nothing else about the tag suggests a date.
	if got := SchemeOf("999.1.2"); got != SchemeSemVer {
		t.Errorf("SchemeOf(\"999.1.2\") = %s, want %s", got, SchemeSemVer)
	}
}

// A bare number above the threshold is unknown rather than calver: with no
// second segment there is nothing to corroborate a year against, and calling
// it calver would license comparing it against 2026.9.1.
func TestABareBigNumberIsUnknown(t *testing.T) {
	for _, tag := range []string{"1234", "20260916", "12345678"} {
		if got := SchemeOf(tag); got != SchemeUnknown {
			t.Errorf("SchemeOf(%q) = %s, want %s", tag, got, SchemeUnknown)
		}
	}
	// Below it, a bare number is the ordinary single-segment version that
	// postgres and node publish.
	for _, tag := range []string{"8", "22", "999"} {
		if got := SchemeOf(tag); got != SchemeSemVer {
			t.Errorf("SchemeOf(%q) = %s, want %s", tag, got, SchemeSemVer)
		}
	}
}

// A two-digit leading segment is never read as a year, even when the rest of
// the tag is shaped like a date.
//
// 24.04 is Ubuntu's year and month; it is also an ordinary major.minor, and
// nothing in the tag says which. Recognising it would mean encoding one
// vendor's convention as the norm, and charging every project on major version
// 24 for it. Ubuntu is the one that departed from the convention.
//
// Safe in the direction it fails: such a tag reads as semver and is compared
// against other semver tags from the same repository, which share whatever
// convention it uses. The comparison this rule exists to prevent -- 0.3.22
// against 2026.9.1 -- needs a four-digit year to arise at all.
func TestTwoDigitYearsAreNotRecognised(t *testing.T) {
	for _, tag := range []string{"24.04", "22.04", "24.10", "24.0", "24.1", "99.9"} {
		if got := SchemeOf(tag); got != SchemeSemVer {
			t.Errorf("SchemeOf(%q) = %s, want %s", tag, got, SchemeSemVer)
		}
	}
	// And such tags still compare against each other, which is what keeps an
	// Ubuntu-versioned image upgradable.
	if ok, why := SameScheme("24.04", "24.10"); !ok {
		t.Errorf("SameScheme(24.04, 24.10) = false (%s), want true", why)
	}
}

func TestSameScheme(t *testing.T) {
	for _, tc := range []struct {
		name, current, candidate string
		want                     bool
	}{
		{"semver to semver", "0.3.22", "0.3.23", true},
		{"semver across flavours", "1.47.0-alpine", "1.48.0", true},
		{"calver to calver", "2026.8.2", "2026.9.1", true},
		{"ubuntu to ubuntu", "24.04", "24.10", true},

		// The rule: a new version can never be a different scheme.
		{"semver to calver", "0.3.22", "2026.9.1", false},
		{"calver to semver", "2026.8.2", "0.3.22", false},
		{"semver to moving", "0.3.22", "latest", false},
		{"moving to semver", "latest", "1.2.3", false},

		// ofelia: the case this exists for.
		{"semver to git sha", "0.3.22", "a573727", false},
		{"semver to build number", "0.3.22", "12345678", false},

		// An unknown current tag means nothing can be said either way.
		{"unknown current", "a573727", "0.3.22", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := SameScheme(tc.current, tc.candidate)
			if got != tc.want {
				t.Errorf("SameScheme(%q, %q) = %v (%s), want %v",
					tc.current, tc.candidate, got, why, tc.want)
			}
			// A refusal must say why: it becomes the reason on the page.
			if !got && why == "" {
				t.Error("refused without a reason")
			}
			if got && why != "" {
				t.Errorf("accepted but gave a reason: %q", why)
			}
		})
	}
}

// The reason is read by a person deciding whether the rule was right, so it
// must name both tags and both schemes.
func TestRefusalNamesWhatItCompared(t *testing.T) {
	_, why := SameScheme("0.3.22", "2026.9.1")
	for _, want := range []string{"0.3.22", "2026.9.1", "semver", "calver"} {
		if !contains(why, want) {
			t.Errorf("the reason %q does not mention %q", why, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
