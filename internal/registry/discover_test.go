package registry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// discoverServer serves manifest HEADs, returning 200 only for tags in present.
func discoverServer(t *testing.T, present map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No auth challenge for simplicity — tagExists works without it too.
		tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if present[tag] {
			w.Header().Set("Docker-Content-Digest", "sha256:whatever")
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestDiscoverMovingTag_Bare(t *testing.T) {
	// caddy-style: only "alpine" exists, not "latest-alpine".
	srv := discoverServer(t, map[string]bool{"alpine": true})
	defer srv.Close()

	client := &http.Client{}
	tag, ok, err := discoverMovingTagAt(client, srv.URL, "repo", "alpine")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || tag != "alpine" {
		t.Errorf("got (%q, %v), want (\"alpine\", true)", tag, ok)
	}
}

func TestDiscoverMovingTag_None(t *testing.T) {
	// An immutable-build suffix like g<sha> has no moving tag -> bail.
	srv := discoverServer(t, map[string]bool{"latest": true, "alpine": true})
	defer srv.Close()

	client := &http.Client{}
	_, ok, err := discoverMovingTagAt(client, srv.URL, "repo", "ga1b2c3d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected no moving tag for unknown variant")
	}
}

func TestDiscoverMovingTag_Ambiguous(t *testing.T) {
	// Both "alpine" and "latest-alpine" exist -> ambiguous -> bail.
	srv := discoverServer(t, map[string]bool{"alpine": true, "latest-alpine": true})
	defer srv.Close()

	client := &http.Client{}
	_, ok, err := discoverMovingTagAt(client, srv.URL, "repo", "alpine")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected bail when multiple candidates exist")
	}
}

func TestMovingPullTag_NonNetwork(t *testing.T) {
	tests := []struct {
		current string
		want    string
		wantErr bool
	}{
		{"latest", "latest", false},
		{"2.11.4", "latest", false},
		{"2", "latest", false},
		{"alpine", "alpine", false}, // bare non-version tag is already moving
		{"stable", "stable", false},
	}
	for _, tt := range tests {
		// Docker Hub official image path; these branches never hit the network.
		got, err := MovingPullTag("caddy", tt.current)
		if (err != nil) != tt.wantErr {
			t.Errorf("MovingPullTag(caddy, %q) err = %v, wantErr %v", tt.current, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("MovingPullTag(caddy, %q) = %q, want %q", tt.current, got, tt.want)
		}
	}
}

func TestRegistryEndpoint(t *testing.T) {
	tests := []struct {
		image    string
		wantBase string
		wantRepo string
	}{
		{"caddy", "https://registry-1.docker.io", "library/caddy"},
		{"cloudflare/cloudflared", "https://registry-1.docker.io", "cloudflare/cloudflared"},
		{"ghcr.io/miista/app", "https://ghcr.io", "miista/app"},
		{"quay.io/org/app", "https://quay.io", "org/app"},
	}
	for _, tt := range tests {
		base, repo := registryEndpoint(tt.image)
		if base != tt.wantBase || repo != tt.wantRepo {
			t.Errorf("registryEndpoint(%q) = (%q, %q), want (%q, %q)",
				tt.image, base, repo, tt.wantBase, tt.wantRepo)
		}
	}
}
