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

// Docker Hub's tag summary omits the top-level digest for tags pushed before
// it began recording one: willfarrell/autoheal:1.2.0, from 2021, carries
// per-architecture digests under images[] and nothing above them, while the
// same repository's tags pushed this year carry both.
//
// The digest is not missing, only unreported -- the registry serves it -- so a
// service pinned to an old tag must not become permanently unwatchable.
func TestRemoteDigestFallsBackWhenHubOmitsTheDigest(t *testing.T) {
	const want = "sha256:31f580ef0279eaced5b38d631b08c474d70d8403c1c2fdd6ddcf2e879d5f3f7c"

	// Hub's summary, shaped like the real 2021 response: images[] populated,
	// no digest above them.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"images":[{"architecture":"amd64","digest":"sha256:aaaa"}]}`)
	}))
	defer hub.Close()

	asked := ""
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.Header().Set("Docker-Content-Digest", want)
	}))
	defer reg.Close()

	hubSummaryBase, hubRegistryBase = hub.URL, reg.URL
	defer func() {
		hubSummaryBase, hubRegistryBase = "https://hub.docker.com", "https://registry-1.docker.io"
	}()

	got, err := RemoteDigest("willfarrell/autoheal", "1.2.0")
	if err != nil {
		t.Fatalf("a tag whose digest hub does not report should still resolve: %v", err)
	}
	if got != want {
		t.Errorf("digest = %q, want the registry's manifest-list digest %q", got, want)
	}
	// Not images[]: those are per-architecture, and pinning one would pin a
	// multi-arch image to a single architecture.
	if got == "sha256:aaaa" {
		t.Error("took a per-architecture digest from images[] instead of asking the registry")
	}
	if !strings.Contains(asked, "willfarrell/autoheal") {
		t.Errorf("the registry was asked for %q, not the image's repository", asked)
	}
}

// The opposite: when hub does report a digest, that is the answer and the
// registry is not troubled for it. A fallback that ran every time would double
// the requests and the bearer-token round trips for no gain.
func TestRemoteDigestPrefersHubsOwnAnswer(t *testing.T) {
	const want = "sha256:f1c3376c26f2609ab9f29f71f824103fe2fcd8ee0346485cb6122a4f93df6f94"

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"digest":%q}`, want)
	}))
	defer hub.Close()

	reached := false
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer reg.Close()

	hubSummaryBase, hubRegistryBase = hub.URL, reg.URL
	defer func() {
		hubSummaryBase, hubRegistryBase = "https://hub.docker.com", "https://registry-1.docker.io"
	}()

	got, err := RemoteDigest("postgres", "16")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("digest = %q, want %q", got, want)
	}
	if reached {
		t.Error("the registry was asked even though hub had already answered")
	}
}
