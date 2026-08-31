package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- isVersionTag ---

func TestIsVersionTag(t *testing.T) {
	valid := []string{
		"1.0", "1.2.3", "2024.6.1", "1.0.0-beta1", "1.0.0.rc1", "3.12.4",
		"v1.2.3", // v-prefixed releases are the norm on GHCR
	}
	invalid := []string{
		"latest", "stable", "main", "edge", "", "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "version1.2",
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

func TestGHCRTagDigestFromBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token") || r.URL.RawQuery != "":
			json.NewEncoder(w).Encode(map[string]string{"token": "testtoken"})
		default:
			w.Header().Set("Docker-Content-Digest", "sha256:265ef19d494673653e56a90f9067be958ac75a000d2e474eebe95e0769501532")
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	got, err := ghcrTagDigestFromBase("foo/bar", "latest", srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sha256:265ef19d494673653e56a90f9067be958ac75a000d2e474eebe95e0769501532" {
		t.Errorf("got %q, want %q", got, "sha256:265ef19d494673653e56a90f9067be958ac75a000d2e474eebe95e0769501532")
	}
}

func TestOCITagDigestFromBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:d5eedf357872799474bd48cd277d5648201d0659ce59fb157bb2d9d035729da6")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got, err := ociTagDigestFromBase(srv.URL, "foo/bar", "latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sha256:d5eedf357872799474bd48cd277d5648201d0659ce59fb157bb2d9d035729da6" {
		t.Errorf("got %q, want %q", got, "sha256:d5eedf357872799474bd48cd277d5648201d0659ce59fb157bb2d9d035729da6")
	}
}

func TestDockerHubTagDigestFromURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"digest": "sha256:ccb08e1cf3be7a93a136e31c165130367c2be32d6a69f0b05b12a11f3054bda6"})
	}))
	defer srv.Close()

	got, err := dockerHubTagDigestFromURL(srv.URL + "/v2/repositories/library/redis/tags/latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sha256:ccb08e1cf3be7a93a136e31c165130367c2be32d6a69f0b05b12a11f3054bda6" {
		t.Errorf("got %q, want %q", got, "sha256:ccb08e1cf3be7a93a136e31c165130367c2be32d6a69f0b05b12a11f3054bda6")
	}
}

func TestDockerHubTagDigestFromURL_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := dockerHubTagDigestFromURL(srv.URL + "/v2/repositories/library/redis/tags/nope")
	if err == nil {
		t.Fatal("expected error for missing tag, got nil")
	}
}
