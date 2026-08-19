package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
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

// hubPagedTestServer serves pageTags across successive pages (one per
// element), each linking to the next via "next" until the last, tracking how
// many pages were actually requested.
func hubPagedTestServer(t *testing.T, pageTags [][]string) (url string, pagesFetched *int) {
	t.Helper()
	fetched := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		fetched++
		results := make([]map[string]any, len(pageTags[page]))
		for i, name := range pageTags[page] {
			results[i] = map[string]any{"name": name, "digest": "sha256:x"}
		}
		next := ""
		if page+1 < len(pageTags) {
			next = fmt.Sprintf("%s?page=%d", srv.URL, page+1)
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results, "next": next})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "?page=0", &fetched
}

func TestListDockerHubTagsMatching_StopsAfterFirstMatchingPage(t *testing.T) {
	// Page 0 has a match (2.9.0, newer than current 2.7.6); pages 1-2 exist
	// but must never be fetched.
	url, fetched := hubPagedTestServer(t, [][]string{
		{"latest", "2.9.0"},
		{"2.8.4"},
		{"2.7.6"},
	})
	include := regexp.MustCompile(`^2\.\d+\.\d+$`)

	tags, err := listDockerHubTagsMatching(url, include, nil, "2.7.6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *fetched != 1 {
		t.Errorf("fetched %d pages, want 1 (should stop once a page has a match)", *fetched)
	}
	if len(tags) != 2 {
		t.Errorf("tags = %v, want the 2 tags from the one page fetched", tags)
	}
}

func TestListDockerHubTagsMatching_KeepsPagingUntilMatchFound(t *testing.T) {
	// No match until page 1 (2.9.0 newer than current 2.7.6); page 2 must
	// still be skipped once page 1's match is found.
	url, fetched := hubPagedTestServer(t, [][]string{
		{"latest", "dev"},
		{"2.9.0"},
		{"2.8.4"},
	})
	include := regexp.MustCompile(`^2\.\d+\.\d+$`)

	tags, err := listDockerHubTagsMatching(url, include, nil, "2.7.6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *fetched != 2 {
		t.Errorf("fetched %d pages, want 2 (page 0 has no match, page 1 does)", *fetched)
	}
	if len(tags) != 3 {
		t.Errorf("tags = %v, want the 3 tags collected across the 2 pages fetched", tags)
	}
}

func TestListDockerHubTagsMatching_NoIncludeFetchesEverything(t *testing.T) {
	// include == nil (the ListTags/listDockerHubTags use case) must not cut
	// pagination short — every page is needed for the unfiltered list.
	url, fetched := hubPagedTestServer(t, [][]string{
		{"latest"},
		{"2.9.0"},
		{"2.8.4"},
	})

	tags, err := listDockerHubTagsMatching(url, nil, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *fetched != 3 {
		t.Errorf("fetched %d pages, want 3 (no include filter means no early exit)", *fetched)
	}
	if len(tags) != 3 {
		t.Errorf("tags = %v, want all 3 tags across all pages", tags)
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
