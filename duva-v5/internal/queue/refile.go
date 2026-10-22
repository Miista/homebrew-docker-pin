package queue

import (
	"fmt"
	"time"
)

// Refile fits a waiting entry to its service's policy as it stands now: a
// change made outside duva -- a duva.auto label added, raised or removed -- is
// a change to what should happen to what is already waiting, not only to
// what is found next.
//
//   - Policy now allows what it did not: the entry is scheduled, its release
//     time counted from when the release was published as for any delayed
//     release, or now if it is old enough. The release job applies it.
//   - Policy no longer allows what it did: a scheduled entry goes back to a
//     person.
//
// Only on a change of policy. The policy an entry was last decided under is
// on it (Auto), and compared with now: an entry policy allowed all along, but
// whose apply failed, waits for a person and is not re-armed every tick.
//
// Refile never applies anything. It sets or clears the release time, and the
// release job, which runs after reconcile, is the one path an unattended
// apply takes. Pure, like RecheckEntry: the caller reads the policy.
//
// What changed is said in one clause, empty when nothing did.
func Refile(e Entry, svc Service, now time.Time) (Entry, string) {
	if svc.Auto == e.Auto {
		return e, ""
	}
	// The release as a notice would have carried it: a version change names
	// its new tag, a digest move the tag it moved on.
	tag := e.To
	if e.Kind == "" {
		tag = e.Tag
	}
	notice := Notice{Image: e.Image + ":" + tag, Digest: e.Digest}
	before := svc
	before.Auto = e.Auto
	if before.Auto == "" {
		before.Auto = AutoNone
	}
	was := Decide(notice, before, true).Outcome == Apply
	v := Decide(notice, svc, true)
	is := v.Outcome == Apply

	out := e
	out.Auto = svc.Auto
	switch {
	case is && !was && e.ReleaseAt == "":
		published, _ := time.Parse(time.RFC3339, e.Published)
		at := releaseAt(svc.Soak, published, now)
		if at.IsZero() {
			at = now
		}
		out.ReleaseAt = at.UTC().Format(time.RFC3339)
		out.Why = v.Why
		if svc.Soak > 0 {
			out.Why = fmt.Sprintf("%s; delayed until %s", v.Why, at.Local().Format("2006-01-02 15:04 MST"))
		}
		return out, fmt.Sprintf("now unattended: duva.auto is %s", svc.Auto)
	case !is && was && e.ReleaseAt != "":
		out.ReleaseAt = ""
		out.Why = v.Why
		return out, fmt.Sprintf("back to a person: duva.auto is %s", svc.Auto)
	}
	// The policy changed, but not which side of the line this entry is on.
	// Recorded, so the next change is compared with this one.
	return out, ""
}
