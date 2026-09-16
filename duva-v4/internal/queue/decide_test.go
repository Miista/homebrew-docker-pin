package queue

import (
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// svc is a pinned service, which is the interesting starting point: pin
// status is the opt-in, so an unpinned one is barely a case.
func svc(tag, digest string, auto Auto) Service {
	return Service{
		Name:   "app",
		File:   "/compose/docker-compose.yml",
		Image:  "example.com/app",
		Tag:    tag,
		Digest: digest,
		Auto:   auto,
	}
}

func notice(image, digest string) Notice {
	return Notice{Container: "app", Image: image, Digest: digest}
}

// --- what is not ours -------------------------------------------------------

// A notice naming a container no service declares must be rejected loudly, not
// dropped. Either the watcher watches something the compose file does not
// describe, or a service lost its container_name and went invisible -- and a
// silently ignored notice looks exactly like nothing having happened.
func TestUnknownContainerIsRejected(t *testing.T) {
	v := Decide(Notice{Container: "ghost"}, Service{}, false)
	if v.Outcome != Reject {
		t.Errorf("outcome = %q, want reject", v.Outcome)
	}
	if !strings.Contains(v.Why, "ghost") {
		t.Errorf("the reason should name the container, got %q", v.Why)
	}
}

// Rejected is not the same as ignored, and the distinction has to survive:
// one means something is wrong, the other means nothing is needed.
func TestRejectIsNotIgnore(t *testing.T) {
	if Reject == Ignore {
		t.Fatal("reject and ignore must be distinguishable")
	}
}

// --- the opt-in -------------------------------------------------------------

func TestUnpinnedServiceIsIgnored(t *testing.T) {
	s := svc("1.0.0", "", AutoNone) // no digest: not pinned
	v := Decide(notice("example.com/app:1.1.0", "sha256:new"), s, true)
	if v.Outcome != Ignore {
		t.Errorf("outcome = %q, want ignore", v.Outcome)
	}
	if !strings.Contains(v.Why, "not pinned") {
		t.Errorf("reason = %q, want it to say why", v.Why)
	}
}

// And the positive: the same notice against a pinned service is acted on.
func TestPinnedServiceIsConsidered(t *testing.T) {
	s := svc("1.0.0", "sha256:old", AutoNone)
	v := Decide(notice("example.com/app:1.1.0", "sha256:new"), s, true)
	if v.Outcome == Ignore {
		t.Errorf("a pinned service was ignored: %q", v.Why)
	}
}

// --- nothing to do ----------------------------------------------------------

// The re-notify case. The protocol re-tells the queue about work until the
// compose file shows the target, so "already on it" is the common answer and
// must not produce a second apply.
func TestSameTagSameDigestIsIgnored(t *testing.T) {
	s := svc("latest", "sha256:abc", AutoMajor)
	v := Decide(notice("example.com/app:latest", "sha256:abc"), s, true)
	if v.Outcome != Ignore {
		t.Errorf("outcome = %q, want ignore", v.Outcome)
	}
	if !strings.Contains(v.Why, "already") {
		t.Errorf("reason = %q", v.Why)
	}
}

// A watcher reporting an older tag is not an update. Going backwards on the
// strength of a notice would be the worst kind of bug this package can have.
func TestOlderTagIsIgnored(t *testing.T) {
	s := svc("2.0.0", "sha256:old", AutoMajor)
	v := Decide(notice("example.com/app:1.9.0", "sha256:other"), s, true)
	if v.Outcome != Ignore {
		t.Errorf("outcome = %q, want ignore -- 1.9.0 is behind 2.0.0", v.Outcome)
	}
	if !strings.Contains(v.Why, "not newer") {
		t.Errorf("reason = %q", v.Why)
	}
}

func TestSameTagNumberIsIgnored(t *testing.T) {
	s := svc("1.2.3", "sha256:old", AutoMajor)
	// Same version, arriving as a notice again.
	v := Decide(notice("example.com/app:1.2.3", "sha256:old"), s, true)
	if v.Outcome != Ignore {
		t.Errorf("outcome = %q, want ignore", v.Outcome)
	}
}

// --- digest moves -----------------------------------------------------------

// Choosing a moving tag is itself the decision to ride the edge, so a digest
// move applies unless the service opted out.
func TestDigestMoveAppliesWhenAutoAllows(t *testing.T) {
	s := svc("latest", "sha256:old", AutoPatch)
	v := Decide(notice("example.com/app:latest", "sha256:new"), s, true)
	if v.Outcome != Apply {
		t.Errorf("outcome = %q, want apply", v.Outcome)
	}
	if v.From != "sha256:old" || v.To != "sha256:new" {
		t.Errorf("from/to = %q/%q, want the digests", v.From, v.To)
	}
	// No version pair, so nothing to classify -- and that is different from
	// KindUnknown, which means there IS a pair and it could not be read.
	if v.Kind != "" {
		t.Errorf("kind = %q, want empty for a digest move", v.Kind)
	}
}

func TestDigestMoveQueuesWhenAutoIsNone(t *testing.T) {
	s := svc("latest", "sha256:old", AutoNone)
	v := Decide(notice("example.com/app:latest", "sha256:new"), s, true)
	if v.Outcome != Queue {
		t.Errorf("outcome = %q, want queue", v.Outcome)
	}
	if !strings.Contains(v.Why, "none") {
		t.Errorf("reason = %q, want it to name the policy", v.Why)
	}
}

// Even AutoPatch takes a digest move: there is no bump to threshold on, so
// any non-none setting means the service asked to follow the tag.
func TestDigestMoveIgnoresTheLadder(t *testing.T) {
	for _, auto := range []Auto{AutoPatch, AutoMinor, AutoMajor} {
		s := svc("dev", "sha256:old", auto)
		v := Decide(notice("example.com/app:dev", "sha256:new"), s, true)
		if v.Outcome != Apply {
			t.Errorf("auto=%s: outcome = %q, want apply", auto, v.Outcome)
		}
	}
}

// --- the ladder -------------------------------------------------------------

func TestPatchWithinAutoPatchApplies(t *testing.T) {
	s := svc("1.0.0", "sha256:old", AutoPatch)
	v := Decide(notice("example.com/app:1.0.1", "sha256:new"), s, true)
	if v.Outcome != Apply {
		t.Errorf("outcome = %q, want apply (%s)", v.Outcome, v.Why)
	}
	if v.Kind != version.KindPatch {
		t.Errorf("kind = %q, want patch", v.Kind)
	}
	if v.From != "1.0.0" || v.To != "1.0.1" {
		t.Errorf("from/to = %q/%q, want the tags", v.From, v.To)
	}
}

func TestMinorExceedsAutoPatchAndQueues(t *testing.T) {
	s := svc("1.0.0", "sha256:old", AutoPatch)
	v := Decide(notice("example.com/app:1.1.0", "sha256:new"), s, true)
	if v.Outcome != Queue {
		t.Errorf("outcome = %q, want queue", v.Outcome)
	}
	if v.Kind != version.KindMinor {
		t.Errorf("kind = %q, want minor", v.Kind)
	}
	if !strings.Contains(v.Why, "exceeds") {
		t.Errorf("reason = %q, want it to say it exceeded the threshold", v.Why)
	}
}

func TestMinorWithinAutoMinorApplies(t *testing.T) {
	s := svc("1.0.0", "sha256:old", AutoMinor)
	v := Decide(notice("example.com/app:1.1.0", "sha256:new"), s, true)
	if v.Outcome != Apply {
		t.Errorf("outcome = %q, want apply (%s)", v.Outcome, v.Why)
	}
}

func TestMajorExceedsAutoMinorAndQueues(t *testing.T) {
	s := svc("1.0.0", "sha256:old", AutoMinor)
	v := Decide(notice("example.com/app:2.0.0", "sha256:new"), s, true)
	if v.Outcome != Queue {
		t.Errorf("outcome = %q, want queue", v.Outcome)
	}
	if v.Kind != version.KindMajor {
		t.Errorf("kind = %q, want major", v.Kind)
	}
}

func TestMajorWithinAutoMajorApplies(t *testing.T) {
	s := svc("1.0.0", "sha256:old", AutoMajor)
	v := Decide(notice("example.com/app:2.0.0", "sha256:new"), s, true)
	if v.Outcome != Apply {
		t.Errorf("outcome = %q, want apply (%s)", v.Outcome, v.Why)
	}
}

// duva.auto: none is the default and every service on both hosts has it
// today, so this is the path that actually runs.
func TestAutoNoneQueuesEverySize(t *testing.T) {
	for _, to := range []string{"1.0.1", "1.1.0", "2.0.0"} {
		s := svc("1.0.0", "sha256:old", AutoNone)
		v := Decide(notice("example.com/app:"+to, "sha256:new"), s, true)
		if v.Outcome != Queue {
			t.Errorf("%s: outcome = %q, want queue", to, v.Outcome)
		}
		if !strings.Contains(v.Why, "none") {
			t.Errorf("%s: reason = %q", to, v.Why)
		}
	}
}

// --- what cannot be measured ------------------------------------------------

// A flavour switch is not an upgrade at any policy.
//
// -alpine and -slim are different lines: different base image, different libc.
// 1.1.0-slim is not a newer 1.0.0-alpine, it is a different artifact whose
// core happens to compare higher, so it is refused rather than sized.
//
// This used to reach the queue as "unclassifiable" and, at duva.auto: major,
// apply -- silently swapping the base image out from under a service that
// asked for alpine. duva.auto says how big a step may be taken unattended; it
// does not say which line to follow, and the tag already said that.
func TestAFlavourSwitchIsNeverACandidate(t *testing.T) {
	for _, auto := range []Auto{AutoNone, AutoPatch, AutoMinor, AutoMajor} {
		t.Run(string(auto), func(t *testing.T) {
			s := svc("1.0.0-alpine", "sha256:old", auto)
			v := Decide(notice("example.com/app:1.1.0-slim", "sha256:new"), s, true)
			if v.Outcome != Ignore {
				t.Errorf("outcome = %q, want ignore (%s)", v.Outcome, v.Why)
			}
			if !strings.Contains(v.Why, "slim") || !strings.Contains(v.Why, "alpine") {
				t.Errorf("reason = %q, want it to name both flavours", v.Why)
			}
		})
	}
}

// Within one flavour the sizing still applies, which is what keeps the rule
// from simply switching updates off for every service on -alpine.
func TestWithinAFlavourTheStepIsStillSized(t *testing.T) {
	s := svc("1.0.0-alpine", "sha256:old", AutoMinor)
	v := Decide(notice("example.com/app:1.1.0-alpine", "sha256:new"), s, true)
	if v.Outcome != Apply {
		t.Errorf("outcome = %q, want apply (%s)", v.Outcome, v.Why)
	}
	if v.Kind != version.KindMinor {
		t.Errorf("kind = %q, want minor", v.Kind)
	}
}

// --- reading a reference ----------------------------------------------------

func TestTagOf(t *testing.T) {
	for ref, want := range map[string]string{
		"app":                             "latest",
		"app:1.2.3":                       "1.2.3",
		"example.com/app:1.2.3":           "1.2.3",
		"example.com/app":                 "latest",
		"app:1.2.3@sha256:abc":            "1.2.3",
		"localhost:5000/app":              "latest",
		"localhost:5000/app:v2":           "v2",
		"localhost:5000/app:v2@sha256:ab": "v2",
		"ghcr.io/owner/name:1.0-alpine":   "1.0-alpine",
	} {
		if got := tagOf(ref); got != want {
			t.Errorf("tagOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

// A registry port is not a tag. Reading localhost:5000/app as tag "5000/app"
// would make every locally-registered image look like a tag change.
func TestTagOfDoesNotMistakeAPortForATag(t *testing.T) {
	if got := tagOf("localhost:5000/app"); got != "latest" {
		t.Errorf("tagOf = %q, want latest -- 5000 is a port", got)
	}
}

// A watcher that only watches which tags exist has no reason to resolve
// digests, and must not be punished for it: reporting the tag already pinned
// with no digest says nothing, and treating it as a move would queue an
// update to nowhere -- which the updater would then try to pull.
func TestTheCurrentTagWithNoDigestIsIgnored(t *testing.T) {
	s := svc("latest", "sha256:old", AutoMajor)
	v := Decide(Notice{Container: "app", Image: "example.com/app:latest"}, s, true)
	if v.Outcome != Ignore {
		t.Errorf("outcome = %q, want ignore (%s)", v.Outcome, v.Why)
	}
	if v.To != "" {
		t.Errorf("it produced a candidate %q out of nothing", v.To)
	}
}

// And the positive: a watcher that does resolve one still gets a move.
func TestTheCurrentTagWithANewDigestIsAMove(t *testing.T) {
	s := svc("latest", "sha256:old", AutoMajor)
	v := Decide(Notice{Container: "app", Image: "example.com/app:latest", Digest: "sha256:new"}, s, true)
	if v.Outcome != Apply {
		t.Errorf("outcome = %q, want apply", v.Outcome)
	}
}

// --- versioning schemes -----------------------------------------------------

// A new version is never of a different scheme than the one in use. The scheme
// is read off the tag the file already declares, so the ordinary case needs no
// label at all.
//
// This is ofelia, which put `0.3.22 -> a573727` on the page: mcuadros/ofelia
// publishes releases and git SHAs to the same repository, and a SHA is not a
// newer version of 0.3.22 -- it is not a version.
func TestACandidateOfADifferentSchemeIsIgnored(t *testing.T) {
	for _, tc := range []struct{ name, current, candidate string }{
		{"a git sha", "0.3.22", "a573727"},
		{"calver against semver", "0.3.22", "2026.9.1"},
		{"semver against calver", "2026.8.2", "0.3.22"},
		{"a build number", "0.3.22", "12345678"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Decide(
				notice("example.com/app:"+tc.candidate, ""),
				svc(tc.current, "sha256:old", AutoNone),
				true,
			)
			if v.Outcome != Ignore {
				t.Errorf("%s -> %s: outcome %s, want %s (%s)",
					tc.current, tc.candidate, v.Outcome, Ignore, v.Why)
			}
			// The reason is what a person reads when asking why an update they
			// expected never appeared, so it must name the tag it refused.
			if !strings.Contains(v.Why, tc.candidate) {
				t.Errorf("the reason does not name %s: %q", tc.candidate, v.Why)
			}
		})
	}
}

// The rule must not cost the ordinary upgrade it exists to protect.
func TestSameSchemeStillUpgrades(t *testing.T) {
	for _, tc := range []struct{ name, current, candidate string }{
		{"semver patch", "0.3.22", "0.3.23"},
		{"calver", "2026.8.2", "2026.9.1"},
		{"single segment", "8", "9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Decide(
				notice("example.com/app:"+tc.candidate, ""),
				svc(tc.current, "sha256:old", AutoNone),
				true,
			)
			if v.Outcome != Queue {
				t.Errorf("%s -> %s: outcome %s, want %s (%s)",
					tc.current, tc.candidate, v.Outcome, Queue, v.Why)
			}
		})
	}
}

// An unrecognisable tag in the compose file means nothing can be said about
// what supersedes it -- so this says that, rather than comparing anyway.
func TestAnUnknownCurrentSchemeSaysSo(t *testing.T) {
	v := Decide(
		notice("example.com/app:1.0.0", ""),
		svc("a573727", "sha256:old", AutoNone),
		true,
	)
	if v.Outcome != Ignore {
		t.Fatalf("outcome = %s, want %s", v.Outcome, Ignore)
	}
	if !strings.Contains(v.Why, "a573727") {
		t.Errorf("the reason does not name the tag in use: %q", v.Why)
	}
}
