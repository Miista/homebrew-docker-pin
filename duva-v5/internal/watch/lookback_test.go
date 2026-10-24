package watch

import (
	"testing"
	"time"
)

func TestParseLookBack(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":    0,
		"0":   0,
		"7d":  7 * 24 * time.Hour,
		"72h": 72 * time.Hour,
		"90d": MaxLookBack,
		" 1d": 24 * time.Hour,
	} {
		if got, err := ParseLookBack(raw); err != nil || got != want {
			t.Errorf("ParseLookBack(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
}

// Refused rather than read as "since the last check": a typo asking for a
// month must not quietly answer for a day.
func TestParseLookBackRefuses(t *testing.T) {
	for _, raw := range []string{"week", "7 days", "-1d", "-2h", "91d", "1w"} {
		if _, err := ParseLookBack(raw); err == nil {
			t.Errorf("ParseLookBack(%q) was accepted", raw)
		}
	}
}

func TestFormatLookBack(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                  "since the last check",
		7 * 24 * time.Hour: "7d",
		36 * time.Hour:     "36h0m0s",
	} {
		if got := FormatLookBack(d); got != want {
			t.Errorf("FormatLookBack(%v) = %q, want %q", d, got, want)
		}
	}
}
