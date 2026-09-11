package watch

import (
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

func TestParseAuto(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Auto
		wantErr bool
	}{
		{"", AutoNone, false}, // label absent
		{"none", AutoNone, false},
		{"patch", AutoPatch, false},
		{"minor", AutoMinor, false},
		{"major", AutoMajor, false},

		// Compose labels are hand-written; be forgiving about shape.
		{"  patch  ", AutoPatch, false},
		{"PATCH", AutoPatch, false},
		{"Minor", AutoMinor, false},

		// A typo must not quietly mean "none". Silently doing nothing is
		// discovered months later, when an update that should have applied
		// itself never did.
		{"pathc", AutoNone, true},
		{"all", AutoNone, true},
		{"true", AutoNone, true},
		{"yes", AutoNone, true},
	} {
		got, err := ParseAuto(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseAuto(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAuto(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The whole ladder, every bump against every threshold.
func TestDecide_TagCandidates(t *testing.T) {
	for _, tc := range []struct {
		bump version.Kind
		auto Auto
		want Decision
	}{
		// none: nothing is ever applied unattended.
		{version.KindPatch, AutoNone, DecideApprove},
		{version.KindMinor, AutoNone, DecideApprove},
		{version.KindMajor, AutoNone, DecideApprove},
		{version.KindUnknown, AutoNone, DecideApprove},

		// patch: only patches.
		{version.KindPatch, AutoPatch, DecideAuto},
		{version.KindMinor, AutoPatch, DecideApprove},
		{version.KindMajor, AutoPatch, DecideApprove},
		{version.KindUnknown, AutoPatch, DecideApprove},

		// minor: patches and minors.
		{version.KindPatch, AutoMinor, DecideAuto},
		{version.KindMinor, AutoMinor, DecideAuto},
		{version.KindMajor, AutoMinor, DecideApprove},
		{version.KindUnknown, AutoMinor, DecideApprove},

		// major: everything, including what could not be classified.
		{version.KindPatch, AutoMajor, DecideAuto},
		{version.KindMinor, AutoMajor, DecideAuto},
		{version.KindMajor, AutoMajor, DecideAuto},
		{version.KindUnknown, AutoMajor, DecideAuto},
	} {
		f := Finding{Status: StatusAvailable, Kind: KindTag, Bump: tc.bump}
		got, why := Decide(f, tc.auto)
		if got != tc.want {
			t.Errorf("bump=%s auto=%s: %s, want %s (why: %s)", tc.bump, tc.auto, got, tc.want, why)
		}
		if why == "" {
			t.Errorf("bump=%s auto=%s: decision came with no explanation", tc.bump, tc.auto)
		}
	}
}

// An unclassifiable change is treated as a major: if duva cannot tell how big
// a change is, it is not one to make unattended. The explanation should say
// so rather than claiming it exceeded a threshold.
func TestDecide_UnknownExplainsItself(t *testing.T) {
	f := Finding{Status: StatusAvailable, Kind: KindTag, Bump: version.KindUnknown}
	_, why := Decide(f, AutoMinor)
	// What matters is that it does not claim a threshold was exceeded, which
	// would be a different and wrong diagnosis. The phrasing is free.
	if strings.Contains(why, "exceeds") {
		t.Errorf("why = %q: an unclassifiable change did not exceed a threshold, "+
			"it could not be measured against one", why)
	}
	if why == "" {
		t.Error("an unclassifiable change should still explain itself")
	}
}

// Choosing a moving tag is itself the decision to ride the edge, so a digest
// move applies unless the service explicitly opted out.
func TestDecide_MovingTag(t *testing.T) {
	f := Finding{Status: StatusAvailable, Kind: KindDigest}
	for _, tc := range []struct {
		auto Auto
		want Decision
	}{
		{AutoNone, DecideApprove},
		{AutoPatch, DecideAuto},
		{AutoMinor, DecideAuto},
		{AutoMajor, DecideAuto},
	} {
		if got, why := Decide(f, tc.auto); got != tc.want {
			t.Errorf("digest with auto=%s: %s, want %s (why: %s)", tc.auto, got, tc.want, why)
		}
	}
}
