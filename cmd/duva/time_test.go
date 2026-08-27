package main

import (
	"testing"
	"time"
)

// An age answers the question a reader actually has -- was this recent --
// which a timestamp makes them work out.
func TestSince(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"seconds", 20 * time.Second, "just now"},
		{"a minute", 90 * time.Second, "1 minute ago"},
		{"minutes", 25 * time.Minute, "25 minutes ago"},
		{"an hour", 75 * time.Minute, "1 hour ago"},
		{"hours", 5 * time.Hour, "5 hours ago"},
		{"yesterday", 30 * time.Hour, "yesterday"},
		{"days", 5 * 24 * time.Hour, "5 days ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := since(now.Add(-tc.ago), now); got != tc.want {
				t.Errorf("since(%v ago) = %q, want %q", tc.ago, got, tc.want)
			}
		})
	}
}

// A state file written by a host whose clock runs ahead would otherwise read
// as a negative age -- "-3 minutes ago".
func TestSince_FutureReadsAsJustNow(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if got := since(now.Add(time.Hour), now); got != "just now" {
		t.Errorf("a timestamp in the future should read as %q, got %q", "just now", got)
	}
}
