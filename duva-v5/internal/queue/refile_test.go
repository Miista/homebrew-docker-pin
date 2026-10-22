package queue

import (
	"strings"
	"testing"
	"time"
)

// A digest move on a followed tag, published a day before refileNow.
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

const days = 24 * time.Hour

// duva.delay says when the next action happens, whatever it is: a delay added
// holds a release in the queue that is younger than it, covered or not.
func TestRefileHoldsAReleaseYoungerThanANewDelay(t *testing.T) {
	for _, auto := range []Auto{AutoNone, AutoDigest} {
		e, what := Refile(waitingMove(auto), following(auto, 3*days), refileNow, true)
		if e.ReleaseAt != "2026-10-24T12:00:00Z" || !strings.Contains(what, "held until") {
			t.Errorf("auto %s: release at %q, said %q, want held three days from publishing", auto, e.ReleaseAt, what)
		}
	}
}

// Older than the delay already: nothing to hold.
func TestRefileLeavesAReleaseOlderThanTheDelay(t *testing.T) {
	e, what := Refile(waitingMove(AutoNone), following(AutoNone, 12*time.Hour), refileNow, true)
	if e.ReleaseAt != "" || what != "" {
		t.Errorf("release at %q, said %q, want nothing", e.ReleaseAt, what)
	}
}

// duva.auto added, no delay: what waits for a person is due now, for the
// release job to apply. Not where nothing applies.
func TestRefileMakesWhatPolicyNowCoversDue(t *testing.T) {
	e, what := Refile(waitingMove(AutoNone), following(AutoDigest, 0), refileNow, true)
	if e.ReleaseAt != "2026-10-22T12:00:00Z" || !strings.Contains(what, "now unattended") {
		t.Errorf("release at %q, said %q, want due now", e.ReleaseAt, what)
	}
	if e, _ := Refile(waitingMove(AutoNone), following(AutoDigest, 0), refileNow, false); e.ReleaseAt != "" {
		t.Errorf("with nothing to apply it, release at %q", e.ReleaseAt)
	}
}

// duva.auto taken away from a held release: it stays held, and comes to a
// person when the delay is over. Scheduled becomes Upcoming; its time stands.
func TestRefileKeepsAHeldReleaseWhoseCoverIsGone(t *testing.T) {
	held := waitingMove(AutoDigest)
	held.ReleaseAt = "2026-10-24T12:00:00Z"
	e, what := Refile(held, following(AutoNone, 3*days), refileNow, true)
	if e.ReleaseAt != held.ReleaseAt || what != "" || e.Auto.Covers(e) {
		t.Errorf("release at %q, said %q, auto %q", e.ReleaseAt, what, e.Auto)
	}
}

// A held release follows its delay: changed, it moves; removed, it is due.
func TestRefileMovesAHeldReleaseWithItsDelay(t *testing.T) {
	held := waitingMove(AutoNone)
	held.ReleaseAt = "2026-10-24T12:00:00Z"
	if e, _ := Refile(held, following(AutoNone, 5*days), refileNow, true); e.ReleaseAt != "2026-10-26T12:00:00Z" {
		t.Errorf("delay raised: release at %q", e.ReleaseAt)
	}
	if e, what := Refile(held, following(AutoNone, 0), refileNow, true); e.ReleaseAt != "2026-10-22T12:00:00Z" || !strings.Contains(what, "removed") {
		t.Errorf("delay removed: release at %q, said %q", e.ReleaseAt, what)
	}
}

// Policy unchanged, no delay: nothing moves. A release covered all along
// waits for a person because its apply failed, and is not re-armed.
func TestRefileLeavesAFailedApplyAlone(t *testing.T) {
	e, what := Refile(waitingMove(AutoDigest), following(AutoDigest, 0), refileNow, true)
	if e.ReleaseAt != "" || what != "" {
		t.Errorf("release at %q, said %q, want nothing", e.ReleaseAt, what)
	}
}

// Without a publish time the delay counts from when it was found.
func TestRefileCountsFromFoundWithoutAPublishTime(t *testing.T) {
	e := waitingMove(AutoNone)
	e.Published, e.FirstSeen = "", "2026-10-22T06:00:00Z"
	if got, _ := Refile(e, following(AutoNone, 2*days), refileNow, true); got.ReleaseAt != "2026-10-24T06:00:00Z" {
		t.Errorf("release at %q, want two days from finding", got.ReleaseAt)
	}
}
