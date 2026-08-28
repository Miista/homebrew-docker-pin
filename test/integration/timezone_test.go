//go:build integration

package integration

import (
	"strings"
	"testing"
)

// duva runs on an image with no /usr/share/zoneinfo, so Go cannot resolve a
// zone name like Europe/Copenhagen from the filesystem: TZ was silently
// ignored and every timestamp stayed UTC while the setting looked applied.
// The binary embeds the database instead (time/tzdata).
//
// This cannot be a unit test. That would load the zone from the developer's
// own machine, which has zoneinfo, and pass whether or not the import exists
// -- failing only once shipped.
func TestTimezoneIsHonoured(t *testing.T) {
	t.Skip("parked: Up stops the service called duva, and this fixture names " +
		"its two duvas by timezone -- so nothing is held back and Start has " +
		"nothing to start. Kept because the behaviour it guards is real and " +
		"was a bug: TZ was silently ignored on an image with no zoneinfo.")

	s := Up(t, "duva-timezones/two-zones")
	s.Start()

	// Two duvas, same instant, zones ten hours apart. Each announces when it
	// will next check, rendered in its own local time.
	copenhagen := s.WaitForLine("duva_copenhagen", "next check at")
	auckland := s.WaitForLine("duva_auckland", "next check at")

	t.Logf("copenhagen: %s", copenhagen)
	t.Logf("auckland:   %s", auckland)

	// The difference is the assertion. One zone alone could match by
	// coincidence, and two agreeing would mean both fell back to UTC --
	// exactly the failure being guarded against.
	if copenhagen == auckland {
		t.Errorf("two timezones produced the same time, so TZ was ignored:\n  %s", copenhagen)
	}

	if !containsAny(copenhagen, "CEST", "CET") {
		t.Errorf("Europe/Copenhagen not named in its output:\n  %s", copenhagen)
	}
	if !containsAny(auckland, "NZST", "NZDT") {
		t.Errorf("Pacific/Auckland not named in its output:\n  %s", auckland)
	}
}

func containsAny(s string, want ...string) bool {
	for _, w := range want {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
