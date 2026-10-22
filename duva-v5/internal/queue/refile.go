package queue

import (
	"fmt"
	"time"
)

// Refile fits a waiting entry to its service's policy as it stands now: a
// change made outside duva -- duva.auto or duva.delay added, changed or
// removed -- is a change to what should happen to what already waits, not
// only to what is found next.
//
// duva.auto says what is applied unattended; duva.delay says when the next
// action happens, whatever it is. So:
//
//   - A release in the queue that is younger than a delay now on its service
//     is held until the delay is over (counted from when it was published,
//     else found): Scheduled if its policy covers it, Upcoming if not.
//   - A held release follows its delay: a changed delay moves its time, and
//     one removed or already over makes it due now. What happens then is the
//     release job's -- applied, or to a person -- by the policy at that time.
//   - A release in the queue that its policy did not cover and now does is
//     due now, for the release job to apply. Only on that change of policy:
//     one covered all along, whose apply failed, waits for a person and is
//     not re-armed every tick.
//
// applies says whether anything here applies. Without it a covered release
// is never made due for applying: nothing would apply it.
//
// Refile never applies anything. It sets or clears the release time, and the
// release job is the one unattended path. Pure, like RecheckEntry: the
// caller reads the policy. What changed is said in one clause, empty when
// nothing did.
func Refile(e Entry, svc Service, now time.Time, applies bool) (Entry, string) {
	out := e
	out.Auto = svc.Auto
	since := parseTime(e.Published)
	if since.IsZero() {
		since = parseTime(e.FirstSeen)
	}
	hold := releaseAt(svc.Soak, since, now)
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	if e.ReleaseAt != "" {
		held := parseTime(e.ReleaseAt)
		switch {
		case !hold.IsZero() && stamp(hold) != e.ReleaseAt:
			out.ReleaseAt = stamp(hold)
			return out, fmt.Sprintf("held until %s: duva.delay is %s", out.ReleaseAt, formatSoak(svc.Soak))
		case hold.IsZero() && held.After(now):
			out.ReleaseAt = stamp(now)
			if svc.Soak <= 0 {
				return out, "due now: duva.delay was removed"
			}
			return out, fmt.Sprintf("due now: duva.delay is %s, and it is older than that", formatSoak(svc.Soak))
		}
		return out, ""
	}

	if !hold.IsZero() {
		out.ReleaseAt = stamp(hold)
		return out, fmt.Sprintf("held until %s: duva.delay is %s", out.ReleaseAt, formatSoak(svc.Soak))
	}
	previous := e.Auto
	if previous == "" {
		previous = AutoNone
	}
	if applies && svc.Auto != previous && svc.Auto.Covers(e) && !previous.Covers(e) {
		out.ReleaseAt = stamp(now)
		return out, fmt.Sprintf("now unattended: duva.auto is %s", svc.Auto)
	}
	return out, ""
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// formatSoak is a delay as a label would say it: 3d, or 36h.
func formatSoak(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}
