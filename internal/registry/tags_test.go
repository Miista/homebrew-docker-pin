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

func TestIsDockerHub(t *testing.T) {
	hub := []string{"caddy", "library/postgres", "docker.io/library/redis", "miista/foo"}
	notHub := []string{"localhost:5000/app", "localhost/app", "myregistry:5000/app", "ghcr.io/x/y", "registry.example.com/app"}
	for _, img := range hub {
		if !isDockerHub(img) {
			t.Errorf("isDockerHub(%q) = false, want true", img)
		}
	}
	for _, img := range notHub {
		if isDockerHub(img) {
			t.Errorf("isDockerHub(%q) = true, want false", img)
		}
	}
}

func TestCompareVersions_NumericSuffixRuns(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.32.8-ls100", "1.32.8-ls99", 1},  // lexical compare would say -1
		{"3.18.4-r10", "3.18.4-r2", 1},
		{"16-3.10", "16-3.9", 1},
		{"1.0.0-rc10", "1.0.0-rc9", 1},
		{"1.0.0-rc2", "1.0.0-rc10", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
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

func TestMatchingCandidates(t *testing.T) {
	tags := []string{"2.9.0", "2.9.0-beta.2", "2.8.4", "2.7.6", "latest", "3.0.0"}
	include := regexp.MustCompile(`^2\.\d+\.\d+`)
	exclude := regexp.MustCompile(`beta`)

	got := MatchingCandidates(tags, include, exclude, "2.7.6")
	want := []string{"2.9.0", "2.8.4"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}

	// Without exclude the beta is a candidate, ranked below the release.
	got = MatchingCandidates(tags, include, nil, "2.8.4")
	want = []string{"2.9.0", "2.9.0-beta.2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}
