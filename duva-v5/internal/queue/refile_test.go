package queue

import (
	"strings"
	"testing"
	"time"
)

// A digest move on a followed tag, queued for a person under no policy.
func waitingMove(auto Auto) Entry {
	return Entry{Service: "radarr", Image: "ghcr.io/linuxserver/radarr", Tag: "latest",
		From: "sha256:old", To: "sha256:new", Digest: "sha256:new", Auto: auto,
		Published: "2026-10-21T12:00:00Z"}
}

func following(auto Auto, soak time.Duration) Service {
	return Service{Name: "radarr", Image: "ghcr.io/linuxserver/radarr", Tag: "latest",
		Digest: "sha256:old", Auto: auto, Soak: soak}
}

var refileNow = at("2026-10-22T12:00:00Z")

// duva.auto added where a release already waits: the release is the
// policy's now. With a delay it is scheduled from when it was published.
func TestRefileSchedulesWhatPolicyNowAllows(t *testing.T) {
	e, what := Refile(waitingMove(AutoNone), following(AutoDigest, 3*24*time.Hour), refileNow)
	if e.ReleaseAt != "2026-10-24T12:00:00Z" {
		t.Errorf("release at %q, want three days from publishing", e.ReleaseAt)
	}
	if e.Auto != AutoDigest || !strings.Contains(what, "now unattended") {
		t.Errorf("auto %q, said %q", e.Auto, what)
	}
}

// Old enough already, or no delay: due now, for the release job to apply.
func TestRefileMakesAnOldEnoughReleaseDueNow(t *testing.T) {
	e, _ := Refile(waitingMove(AutoNone), following(AutoDigest, 0), refileNow)
	if e.ReleaseAt != "2026-10-22T12:00:00Z" {
		t.Errorf("release at %q, want now", e.ReleaseAt)
	}
}

// duva.auto taken away from a scheduled release: it goes back to a person,
// rather than applying itself under a policy nobody holds any more.
func TestRefileReturnsAScheduledReleaseToAPerson(t *testing.T) {
	scheduled := waitingMove(AutoDigest)
	scheduled.ReleaseAt = "2026-10-24T12:00:00Z"
	e, what := Refile(scheduled, following(AutoNone, 0), refileNow)
	if e.ReleaseAt != "" || !strings.Contains(what, "back to a person") {
		t.Errorf("release at %q, said %q", e.ReleaseAt, what)
	}
}

// Policy unchanged: nothing moves. An entry policy allowed all along waits
// for a person because its apply failed, and is not re-armed every tick.
func TestRefileLeavesAnUnchangedPolicyAlone(t *testing.T) {
	failed := waitingMove(AutoDigest)
	e, what := Refile(failed, following(AutoDigest, 0), refileNow)
	if e.ReleaseAt != "" || what != "" {
		t.Errorf("release at %q, said %q, want nothing", e.ReleaseAt, what)
	}
}

// A change that does not cross the line -- none to patch, for a minor --
// moves nothing, but is recorded so the next change compares with it.
func TestRefileRecordsAChangeThatMovesNothing(t *testing.T) {
	minor := Entry{Service: "app", Image: "example.com/app", Tag: "1.3.0", From: "1.2.0", To: "1.3.0",
		Kind: "minor", Auto: AutoNone}
	svc := Service{Name: "app", Image: "example.com/app", Tag: "1.2.0", Auto: AutoPatch}
	e, what := Refile(minor, svc, refileNow)
	if e.ReleaseAt != "" || what != "" || e.Auto != AutoPatch {
		t.Errorf("release at %q, said %q, auto %q", e.ReleaseAt, what, e.Auto)
	}
}
