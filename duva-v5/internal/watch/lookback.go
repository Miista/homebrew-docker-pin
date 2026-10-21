package watch

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxLookBack is the furthest a check may be asked to look back. Each
// service costs a tag listing per page of tags, an image with many tags took
// a minute for a week on its own, and Docker Hub rate-limits anonymous use: a
// month across every service is already the kind of burst that finds the
// limit.
const MaxLookBack = 90 * 24 * time.Hour

// ParseLookBack reads how far back a person asked a check to look: whole days
// ("7d"), a Go duration ("72h", "90m"), or empty or "0" for "since each
// service's last check". Anything else is an error rather than a quiet "since
// the last check": a typo asking for a month must not answer for a day.
func ParseLookBack(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number of days", raw)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(raw); err != nil {
			return 0, fmt.Errorf("%q is not a duration (want e.g. 7d, 72h or 0)", raw)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is before now", raw)
	}
	if d > MaxLookBack {
		return 0, fmt.Errorf("%q is further back than %dd", raw, int(MaxLookBack/(24*time.Hour)))
	}
	return d, nil
}

// FormatLookBack is how a look-back reads in the history and the log: "7d",
// "36h", or "since the last check" for zero.
func FormatLookBack(d time.Duration) string {
	switch {
	case d == 0:
		return "since the last check"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	default:
		return d.String()
	}
}
