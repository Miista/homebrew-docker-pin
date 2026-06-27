package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- isVersionTag ---

func TestIsVersionTag(t *testing.T) {
	valid := []string{
		"1.0", "1.2.3", "2024.6.1", "1.0.0-beta1", "1.0.0.rc1", "3.12.4",
	}
	invalid := []string{
		"latest", "stable", "main", "edge", "", "v1.2.3", "sha256:abc",
	}
	for _, tag := range valid {
		if !isVersionTag(tag) {
			t.Errorf("isVersionTag(%q) = false, want true", tag)
		}
	}
	for _, tag := range invalid {
		if isVersionTag(tag) {
			t.Errorf("isVersionTag(%q) = true, want false", tag)
		}
	}
}

// --- tagSpecificity ---

func TestTagSpecificity_Ordering(t *testing.T) {
	tags := []string{"1", "1.2", "1.2.3", "1.2.3.4"}
	for i := 1; i < len(tags); i++ {
		di, li := tagSpecificity(tags[i])
		dprev, lprev := tagSpecificity(tags[i-1])
		if di < dprev || (di == dprev && li < lprev) {
			t.Errorf("expected %q to be more specific than %q", tags[i], tags[i-1])
		}
	}
}

// --- splitDockerHubImage ---

func TestSplitDockerHubImage(t *testing.T) {
	tests := []struct {
		input         string
		wantNamespace string
		wantRepo      string
	}{
		{"nginx", "library", "nginx"},
		{"cloudflare/cloudflared", "cloudflare", "cloudflared"},
		{"docker.io/library/redis", "library", "redis"},
		{"sissbruecker/linkding", "sissbruecker", "linkding"},
	}
	for _, tt := range tests {
		ns, repo := splitDockerHubImage(tt.input)
		if ns != tt.wantNamespace || repo != tt.wantRepo {
			t.Errorf("splitDockerHubImage(%q) = (%q, %q), want (%q, %q)",
				tt.input, ns, repo, tt.wantNamespace, tt.wantRepo)
		}
	}
}

// --- resolveDockerHub (mocked) ---

func TestResolveDockerHub(t *testing.T) {
	const targetDigest = "sha256:deadbeef"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"name": "latest", "digest": targetDigest},
				{"name": "1.25", "digest": targetDigest},
				{"name": "1", "digest": targetDigest},
				{"name": "1.25.3", "digest": targetDigest},
				{"name": "stable", "digest": "sha256:other"},
			},
			"next": nil,
		})
	}))
	defer srv.Close()

	// Patch the Hub URL by wrapping the function — instead, we test via
	// a thin helper that accepts a base URL override.
	got, err := resolveDockerHubWithBase("nginx", targetDigest, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should pick most specific version tag: "1.25.3" (3 dots beats "1.25")
	if got != "1.25.3" {
		t.Errorf("got %q, want %q", got, "1.25.3")
	}
}

func TestResolveDockerHub_NoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"name": "latest", "digest": "sha256:other"},
			},
		})
	}))
	defer srv.Close()

	got, err := resolveDockerHubWithBase("nginx", "sha256:target", srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty result, got %q", got)
	}
}

// --- resolveGHCR (mocked) ---

func TestResolveGHCR(t *testing.T) {
	const targetDigest = "sha256:cafebabe"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token") || r.URL.RawQuery != "":
			json.NewEncoder(w).Encode(map[string]string{"token": "testtoken"})
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			json.NewEncoder(w).Encode(map[string]any{
				"tags": []string{"latest", "2024.6.1", "2024.6", "stable"},
			})
		default:
			// Manifest HEAD request — match only "2024.6.1"
			tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if tag == "2024.6.1" {
				w.Header().Set("Docker-Content-Digest", targetDigest)
			} else {
				w.Header().Set("Docker-Content-Digest", "sha256:other")
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	got, err := resolveGHCRWithBase("ghcr.io/home-assistant/home-assistant", targetDigest, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "2024.6.1" {
		t.Errorf("got %q, want %q", got, "2024.6.1")
	}
}

func TestResolveGHCR_NoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token") || r.URL.RawQuery != "":
			json.NewEncoder(w).Encode(map[string]string{"token": "testtoken"})
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			json.NewEncoder(w).Encode(map[string]any{"tags": []string{"latest", "stable"}})
		default:
			w.Header().Set("Docker-Content-Digest", "sha256:other")
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	got, err := resolveGHCRWithBase("ghcr.io/foo/bar", "sha256:target", srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty result, got %q", got)
	}
}

// --- ResolveVersionTag dispatch ---

func TestResolveVersionTag_UnknownRegistry(t *testing.T) {
	got, err := ResolveVersionTag("myregistry.example.com/myimage", "sha256:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for unknown registry, got %q", got)
	}
}

// Helpers that accept a base URL so tests can point at httptest servers.

func resolveDockerHubWithBase(image, digest, baseURL string) (string, error) {
	namespace, repo := splitDockerHubImage(image)
	url := fmt.Sprintf("%s/v2/repositories/%s/%s/tags?page_size=100&ordering=last_updated",
		baseURL, namespace, repo)
	return resolveDockerHubFromURL(digest, url)
}

func resolveGHCRWithBase(image, digest, baseURL string) (string, error) {
	path := strings.TrimPrefix(image, "ghcr.io/")
	return resolveGHCRFromBase(path, digest, baseURL)
}
