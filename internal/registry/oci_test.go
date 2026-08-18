package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// ociTestServer stands up a minimal OCI registry that requires a bearer token
// obtained via the WWW-Authenticate challenge flow.
func ociTestServer(t *testing.T, tags []string, digestFor func(tag string) string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			json.NewEncoder(w).Encode(map[string]string{"token": "testtoken"})

		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer realm="%s/token",service="registry",scope="repository:repo:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"tags": tags})

		default: // manifest HEAD
			if r.Header.Get("Authorization") == "" {
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer realm="%s/token",service="registry",scope="repository:repo:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			w.Header().Set("Docker-Content-Digest", digestFor(tag))
			w.WriteHeader(http.StatusOK)
		}
	}))
	return srv
}

func TestResolveOCI_ChallengeFlow(t *testing.T) {
	const target = "sha256:match"
	srv := ociTestServer(t, []string{"latest", "1.2.3", "1.2", "stable"}, func(tag string) string {
		if tag == "1.2.3" {
			return target
		}
		return "sha256:other"
	})
	defer srv.Close()

	got, err := resolveOCIFromBase(srv.URL, "repo", target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "1.2.3" {
		t.Errorf("got tag %q, want %q", got.Tag, "1.2.3")
	}
}

func TestResolveOCI_Orphaned(t *testing.T) {
	srv := ociTestServer(t, []string{"latest", "1.2.3"}, func(tag string) string {
		return "sha256:newbuild"
	})
	defer srv.Close()

	got, err := resolveOCIFromBase(srv.URL, "repo", "sha256:oldbuild")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "" {
		t.Errorf("expected no match, got %q", got.Tag)
	}
	if got.VersionTagsSeen != 1 {
		t.Errorf("expected 1 version tag seen, got %d", got.VersionTagsSeen)
	}
}

func TestNextPageURL(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{`</v2/x/tags/list?last=foo&n=100>; rel="next"`, "/v2/x/tags/list?last=foo&n=100"},
		{`<https://ghcr.io/v2/x/tags/list?n=100>; rel="next"`, "https://ghcr.io/v2/x/tags/list?n=100"},
		{"", ""},                    // last page: no Link header at all
		{`</v2/x>; rel="prev"`, ""}, // some other relation, not next
		{`</a>; rel="prev", </b>; rel="next"`, "/b"}, // next among multiple relations
	}
	for _, tt := range tests {
		if got := nextPageURL(tt.header); got != tt.want {
			t.Errorf("nextPageURL(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestOCIListTags_WalksAllPages(t *testing.T) {
	// Three pages of one tag each; the endpoint has no sort/early-exit
	// option (see ghcrListTagsFromBase's doc comment), so every page must
	// be fetched to get a complete list.
	pages := [][]string{{"1.0.0"}, {"2.0.0"}, {"9.0.0"}}
	var srv *httptest.Server
	requested := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 0
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		requested++
		if page+1 < len(pages) {
			// Real registries (confirmed against ghcr.io) send a relative
			// path-only reference here, not an absolute URL.
			w.Header().Set("Link", fmt.Sprintf(`</v2/repo/tags/list?page=%d>; rel="next"`, page+1))
		}
		json.NewEncoder(w).Encode(map[string]any{"tags": pages[page]})
	}))
	defer srv.Close()

	client := &http.Client{}
	tags, err := ociListTags(client, srv.URL, "repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requested != 3 {
		t.Errorf("requested %d pages, want 3 (must walk all, no early exit)", requested)
	}
	want := []string{"1.0.0", "2.0.0", "9.0.0"}
	if len(tags) != len(want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Errorf("tags[%d] = %q, want %q", i, tags[i], want[i])
		}
	}
}

func TestSplitRegistryRepo(t *testing.T) {
	tests := []struct {
		image    string
		wantHost string
		wantRepo string
	}{
		{"quay.io/org/app", "quay.io", "org/app"},
		{"registry.example.com:5000/app", "registry.example.com:5000", "app"},
		{"nginx", "", "nginx"},
	}
	for _, tt := range tests {
		host, repo := splitRegistryRepo(tt.image)
		if host != tt.wantHost || repo != tt.wantRepo {
			t.Errorf("splitRegistryRepo(%q) = (%q, %q), want (%q, %q)",
				tt.image, host, repo, tt.wantHost, tt.wantRepo)
		}
	}
}

func TestRegistryKind(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"ghcr.io/miista/app", "GitHub Container Registry"},
		{"nginx", "Docker Hub"},
		{"cloudflare/cloudflared", "Docker Hub"},
		{"docker.io/library/redis", "Docker Hub"},
		{"quay.io/org/app", "the OCI registry API at quay.io"},
	}
	for _, tt := range tests {
		if got := registryKind(tt.image); got != tt.want {
			t.Errorf("registryKind(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}
