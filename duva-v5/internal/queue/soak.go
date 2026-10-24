package queue

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseSoak reads a duva.defer label value: how old a release
// must be before policy applies it unattended. Empty means the label is
// absent, which is no soak.
//
// A soak exists so that a release announced as bad can be stopped before it
// reaches this host. v4 had none, so a service under duva.auto applied a
// release on the run that found it -- before anyone could have heard it was
// broken.
//
// Go's durations, plus whole days ("3d"), because a soak is measured in days
// and "72h" is a conversion nobody should have to make in a label. Anything
// else is an error rather than a fallback, for the same reason as duva.auto:
// `duva.defer: 3 days` silently meaning "no delay" is the permissive direction.
func ParseSoak(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("duva.defer: %q is not a number of days", value)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("duva.defer: %q is not a duration (want e.g. 3d, 72h or 0)", value)
	}
	return d, nil
}

// releaseAt is when a release found now may be applied unattended, given the
// service's soak.
//
// Counted from when the release was published, not from when it was found:
// a soak is about the release's age, and a watcher checking daily would
// otherwise add up to a day to every one. A notice that does not say when
// is counted from now, which errs towards waiting longer.
//
// Zero means no wait: no soak, or a release already old enough.
func releaseAt(soak time.Duration, published, now time.Time) time.Time {
	if soak <= 0 {
		return time.Time{}
	}
	from := published
	if from.IsZero() || from.After(now) {
		// A publish time in the future is a clock that cannot be trusted,
		// and trusting it would only ever make the wait longer than asked
		// for -- so it is not trusted in either direction.
		from = now
	}
	at := from.Add(soak)
	if !at.After(now) {
		return time.Time{}
	}
	return at
}
