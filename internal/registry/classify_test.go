package registry

import "testing"

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		want     Kind
	}{
		// Plain semver, the case everything else is measured against.
		{"patch", "1.26.0", "1.26.3", KindPatch},
		{"minor", "1.26.3", "1.27.0", KindMinor},
		{"major", "1.26.3", "2.0.0", KindMajor},
		{"same", "1.26.3", "1.26.3", KindNone},

		// A leading v is decoration, not a version segment.
		{"v prefix", "v1.2.3", "v1.2.4", KindPatch},
		{"v prefix minor", "v1.2.3", "v1.3.0", KindMinor},

		// Identical suffixes are comparable: the flavour did not change.
		{"suffixed patch", "17.10-alpine", "17.10.1-alpine", KindPatch},
		{"suffixed minor", "17.10-alpine", "17.11-alpine", KindMinor},
		{"suffixed major", "17.10-alpine", "18.0-alpine", KindMajor},

		// Missing segments count as zero, so 1.2 and 1.2.0 are the same point.
		{"short vs long, equal", "1.2", "1.2.0", KindNone},
		{"short to patch", "1.2", "1.2.1", KindPatch},

		// Four segments: anything past the second is a patch.
		{"four segments", "5.28.1.4321", "5.28.1.4400", KindPatch},
		{"four segments minor", "5.28.1.4321", "5.29.0.1", KindMinor},

		// Changed suffix: not two points on one line. -alpine to -slim is a
		// different image, and a prerelease is not a version step.
		{"flavour change", "1.2.3-alpine", "1.2.4-slim", KindUnknown},
		{"gains a suffix", "1.2.3", "1.2.4-rc1", KindUnknown},
		{"loses a suffix", "1.2.3-rc1", "1.2.4", KindUnknown},

		// linuxserver's build counter moves with the version, so both core
		// and suffix differ: unknown, which is the honest answer.
		{"linuxserver", "1.14.7-ls166", "1.14.8-ls167", KindUnknown},

		// No numeric core at all.
		{"non-numeric", "stable", "edge", KindUnknown},
		{"one non-numeric", "1.2.3", "latest", KindUnknown},
		{"empty", "", "1.0.0", KindUnknown},
	} {
		if got := Classify(tc.from, tc.to); got != tc.want {
			t.Errorf("%s: Classify(%q, %q) = %q, want %q", tc.name, tc.from, tc.to, got, tc.want)
		}
	}
}

// The two known misreads, asserted so they are a decision on record rather
// than a surprise. Both err toward reporting a bigger change than reality,
// which costs an approval rather than causing an unattended surprise.
func TestClassify_KnownMisreads(t *testing.T) {
	// CalVer: a month bump is not a "minor" in any meaningful sense, but
	// nothing in the tag says it is a date.
	if got := Classify("2026.07.2", "2026.08.0"); got != KindMinor {
		t.Errorf("CalVer month bump = %q, want minor (documented misread)", got)
	}
	// Two-segment versioning: upstream calls this a patch, the shape says
	// minor.
	if got := Classify("17.10", "17.11"); got != KindMinor {
		t.Errorf("two-segment bump = %q, want minor (documented misread)", got)
	}
	// A CalVer year change does at least read as major, which is right.
	if got := Classify("2026.07.2", "2027.01.0"); got != KindMajor {
		t.Errorf("CalVer year bump = %q, want major", got)
	}
}

// Downgrades are classified by size, not direction: the caller decides what to
// do about going backwards. Candidate selection never offers one.
func TestClassify_IgnoresDirection(t *testing.T) {
	if got := Classify("2.0.0", "1.0.0"); got != KindMajor {
		t.Errorf("downgrade = %q, want major", got)
	}
}
