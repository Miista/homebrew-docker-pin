package registry

import (
	"regexp"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.10.0", "1.9.9", 1},
		{"2026.6.1", "2026.6", 1},
		{"v1.2.3", "1.2.3", 0},
		{"2026.6.1-g8487590", "2026.6.1", -1}, // suffixed build ranks below bare release
		{"2026.6.1", "2026.6.1-g8487590", 1},
		{"17.5-alpine", "17.4-alpine", 1},
		{"18.0-alpine", "17.9-alpine", 1},
		{"alpine", "alpine", 0}, // no numeric core: lexical
	}
	for _, tt := range tests {
		if got := CompareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNewestMatching(t *testing.T) {
	tags := []string{"latest", "18.1-alpine", "17.4-alpine", "17.5-alpine", "17.5", "dev"}
	alpine17 := regexp.MustCompile(`^17\.\d+-alpine$`)

	if got := NewestMatching(tags, alpine17, "17.4-alpine"); got != "17.5-alpine" {
		t.Errorf("got %q, want 17.5-alpine", got)
	}
	// Already newest: nothing to do.
	if got := NewestMatching(tags, alpine17, "17.5-alpine"); got != "" {
		t.Errorf("got %q, want empty (up to date)", got)
	}
	// Constraint never escapes to 18.x even though it exists.
	if got := NewestMatching(tags, alpine17, "17.0-alpine"); got != "17.5-alpine" {
		t.Errorf("got %q, want 17.5-alpine", got)
	}
	// No matching tags at all.
	none := regexp.MustCompile(`^99\.`)
	if got := NewestMatching(tags, none, "17.4-alpine"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
