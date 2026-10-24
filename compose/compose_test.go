package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFile returns a file's contents, for asserting on a rewrite's result.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- splitImage ---

func TestSplitImage(t *testing.T) {
	tests := []struct {
		input    string
		wantBase string
		wantTag  string
	}{
		{"nginx", "nginx", "latest"},
		{"nginx:1.25", "nginx", "1.25"},
		{"nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "nginx", "1.25"},
		{"ghcr.io/home-assistant/home-assistant:stable", "ghcr.io/home-assistant/home-assistant", "stable"},
		{"ghcr.io/home-assistant/home-assistant:2024.6.1@sha256:2baf1f40105d9501fe319a8ec463fdf4325a2a5df445adf3f572f626253678c9", "ghcr.io/home-assistant/home-assistant", "2024.6.1"},
		{"myregistry.example.com:5000/myimage:v1.2", "myregistry.example.com:5000/myimage", "v1.2"},
		{"cloudflare/cloudflared:latest", "cloudflare/cloudflared", "latest"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			base, tag, err := splitImage(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if base != tt.wantBase || tag != tt.wantTag {
				t.Errorf("splitImage(%q) = (%q, %q), want (%q, %q)",
					tt.input, base, tag, tt.wantBase, tt.wantTag)
			}
		})
	}
}

// --- FindFile ---

func TestFindFile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	composeFile := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(composeFile, []byte("services: {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := FindFile(sub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != composeFile {
		t.Errorf("FindFile(%q) = %q, want %q", sub, got, composeFile)
	}
}

func TestFindFile_NotFound(t *testing.T) {
	root := t.TempDir()
	_, err := FindFile(root)
	if err == nil {
		t.Error("expected error when no compose file exists")
	}
}

func TestFindFile_PreferTopmost(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "project")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	parentCompose := filepath.Join(root, "docker-compose.yml")
	subCompose := filepath.Join(sub, "docker-compose.yml")
	for _, f := range []string{parentCompose, subCompose} {
		if err := os.WriteFile(f, []byte("services: {}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := FindFile(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got != parentCompose {
		t.Errorf("FindFile should return topmost file of the contiguous run, got %q, want %q", got, parentCompose)
	}
}

func TestFindFile_StopsAtGap(t *testing.T) {
	// root has a compose file, then a directory with none, then sub does.
	// The gap should stop the upward climb, so sub's own file wins.
	root := t.TempDir()
	gap := filepath.Join(root, "gap")
	sub := filepath.Join(gap, "project")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	rootCompose := filepath.Join(root, "docker-compose.yml")
	subCompose := filepath.Join(sub, "docker-compose.yml")
	for _, f := range []string{rootCompose, subCompose} {
		if err := os.WriteFile(f, []byte("services: {}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := FindFile(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got != subCompose {
		t.Errorf("FindFile should stop at the gap, got %q, want %q", got, subCompose)
	}
}

// --- ListServices ---

func TestListServices(t *testing.T) {
	file := writeTempCompose(t, sampleCompose)

	services, err := ListServices(file)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]bool{"web": true, "db": true, "cache": true}
	if len(services) != len(want) {
		t.Fatalf("ListServices() = %v, want %d services", services, len(want))
	}
	for _, s := range services {
		if !want[s] {
			t.Errorf("ListServices() returned unexpected service %q", s)
		}
	}
}

func TestListServices_Empty(t *testing.T) {
	file := writeTempCompose(t, "services: {}\n")

	services, err := ListServices(file)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 0 {
		t.Errorf("ListServices() = %v, want empty", services)
	}
}

func TestListServices_Includes(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "compose.yml")
	included := filepath.Join(dir, "sub", "other.yml")
	writeFile(t, root, "include:\n  - sub/other.yml\nservices:\n  web:\n    image: nginx:latest\n")
	writeFile(t, included, "services:\n  db:\n    image: postgres:16\n")

	services, err := ListServices(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"web": true, "db": true}
	if len(services) != len(want) {
		t.Fatalf("ListServices() = %v, want %d services", services, len(want))
	}
	for _, s := range services {
		if !want[s] {
			t.Errorf("ListServices() returned unexpected service %q", s)
		}
	}
}

func TestListServices_ShadowedNameCountedOnce(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "compose.yml")
	included := filepath.Join(dir, "sub", "other.yml")
	writeFile(t, root, "include:\n  - sub/other.yml\nservices:\n  db:\n    image: mysql:8\n")
	writeFile(t, included, "services:\n  db:\n    image: postgres:16\n")

	services, err := ListServices(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 1 || services[0] != "db" {
		t.Errorf("ListServices() = %v, want [db] (shadowed name counted once)", services)
	}
}

// --- RawImage ---

func TestRawImage(t *testing.T) {
	file := writeTempCompose(t, sampleCompose)

	tests := []struct {
		service string
		want    string
	}{
		{"web", "nginx:1.25"},
		{"db", "postgres:16.2@sha256:6ca13d52ca70c883e0f0bb101e425a89e8624de51db2d2392593af6a84118090"},
		{"cache", "redis"},
	}
	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			got, err := RawImage(file, tt.service)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("RawImage(%q) = %q, want %q", tt.service, got, tt.want)
			}
		})
	}
}

func TestRawImage_UnknownService(t *testing.T) {
	file := writeTempCompose(t, sampleCompose)
	if _, err := RawImage(file, "nonexistent"); err == nil {
		t.Error("expected error for unknown service")
	}
}

// --- Locate ---

// chdir switches the process working directory to dir for the duration of
// the test, restoring it afterward.
func chdir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLocate(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(composeFile, []byte("services: {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	chdir(t, sub)

	got, err := Locate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != composeFile {
		t.Errorf("Locate() = %q, want %q", got, composeFile)
	}
}

func TestLocate_NotFound(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)

	if _, err := Locate(); err == nil {
		t.Error("expected error when no compose file exists")
	}
}

// --- ResolveService ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveService_Direct(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "compose.yml")
	writeFile(t, root, sampleCompose)
	chdir(t, dir)

	got, err := ResolveService("web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != root {
		t.Errorf("ResolveService() = %q, want %q", got, root)
	}
}

func TestResolveService_BareStringInclude(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "compose.yml")
	included := filepath.Join(dir, "sub", "other.yml")
	writeFile(t, root, "include:\n  - sub/other.yml\nservices:\n  web:\n    image: nginx:latest\n")
	writeFile(t, included, "services:\n  db:\n    image: postgres:16\n")
	chdir(t, dir)

	got, err := ResolveService("db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != included {
		t.Errorf("ResolveService() = %q, want %q", got, included)
	}
}

func TestResolveService_PathMapInclude(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "compose.yml")
	included := filepath.Join(dir, "sub", "other.yml")
	writeFile(t, root, "include:\n  - path: sub/other.yml\nservices:\n  web:\n    image: nginx:latest\n")
	writeFile(t, included, "services:\n  db:\n    image: postgres:16\n")
	chdir(t, dir)

	got, err := ResolveService("db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != included {
		t.Errorf("ResolveService() = %q, want %q", got, included)
	}
}

func TestResolveService_RootTakesPrecedenceOverInclude(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "compose.yml")
	included := filepath.Join(dir, "sub", "other.yml")
	writeFile(t, root, "include:\n  - sub/other.yml\nservices:\n  db:\n    image: mysql:8\n")
	writeFile(t, included, "services:\n  db:\n    image: postgres:16\n")
	chdir(t, dir)

	got, err := ResolveService("db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != root {
		t.Errorf("ResolveService() = %q, want root %q (root should shadow include)", got, root)
	}
}

func TestResolveService_FirstIncludeWins(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "compose.yml")
	a := filepath.Join(dir, "a.yml")
	b := filepath.Join(dir, "b.yml")
	writeFile(t, root, "include:\n  - a.yml\n  - b.yml\n")
	writeFile(t, a, "services:\n  x:\n    image: aaa:1\n")
	writeFile(t, b, "services:\n  x:\n    image: bbb:2\n")
	chdir(t, dir)

	got, err := ResolveService("x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != a {
		t.Errorf("ResolveService() = %q, want %q (first include should win)", got, a)
	}
}

func TestResolveService_NestedInclude(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "compose.yml")
	mid := filepath.Join(dir, "sub", "other.yml")
	leaf := filepath.Join(dir, "sub", "deeper", "thing.yml")
	writeFile(t, root, "include:\n  - sub/other.yml\n")
	writeFile(t, mid, "include:\n  - deeper/thing.yml\nservices:\n  db:\n    image: postgres:16\n")
	writeFile(t, leaf, "services:\n  cache:\n    image: redis:7\n")
	chdir(t, dir)

	got, err := ResolveService("cache")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != leaf {
		t.Errorf("ResolveService() = %q, want %q", got, leaf)
	}
}

func TestResolveService_NotFound(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "compose.yml")
	writeFile(t, root, sampleCompose)
	chdir(t, dir)

	if _, err := ResolveService("nonexistent"); err == nil {
		t.Error("expected error for service not found anywhere in the tree")
	}
}

// --- ParseImage ---

const sampleCompose = `services:
  web:
    image: nginx:1.25
  db:
    image: postgres:16.2@sha256:6ca13d52ca70c883e0f0bb101e425a89e8624de51db2d2392593af6a84118090
  cache:
    image: redis
`

func writeTempCompose(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "compose-*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestParseImage(t *testing.T) {
	file := writeTempCompose(t, sampleCompose)

	tests := []struct {
		service  string
		wantBase string
		wantTag  string
	}{
		{"web", "nginx", "1.25"},
		{"db", "postgres", "16.2"},   // digest stripped
		{"cache", "redis", "latest"}, // no tag → latest
	}
	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			base, tag, err := ParseImage(file, tt.service)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if base != tt.wantBase || tag != tt.wantTag {
				t.Errorf("ParseImage(%q) = (%q, %q), want (%q, %q)",
					tt.service, base, tag, tt.wantBase, tt.wantTag)
			}
		})
	}
}

func TestParseImage_UnknownService(t *testing.T) {
	file := writeTempCompose(t, sampleCompose)
	_, _, err := ParseImage(file, "nonexistent")
	if err == nil {
		t.Error("expected error for unknown service")
	}
}

// --- PinImage ---

func TestPinImage(t *testing.T) {
	tests := []struct {
		name     string
		compose  string
		service  string
		pinned   string
		wantLine string
	}{
		{
			name: "simple tag",
			compose: `services:
  web:
    image: nginx:1.25
`,
			service:  "web",
			pinned:   "nginx:1.25@sha256:2baf1f40105d9501fe319a8ec463fdf4325a2a5df445adf3f572f626253678c9",
			wantLine: "    image: nginx:1.25@sha256:2baf1f40105d9501fe319a8ec463fdf4325a2a5df445adf3f572f626253678c9",
		},
		{
			name: "replaces existing digest",
			compose: `services:
  web:
    image: nginx:1.25@sha256:8978c8879951eb9d4bd0a19a322f61159b1788791fe00239eb2af28d66fcd769
`,
			service:  "web",
			pinned:   "nginx:1.25@sha256:ce97e44208e0cb8c903bf0818c0c7d1ca3462393f94a4926ab880482ce7a4e30",
			wantLine: "    image: nginx:1.25@sha256:ce97e44208e0cb8c903bf0818c0c7d1ca3462393f94a4926ab880482ce7a4e30",
		},
		{
			name: "second service not affected",
			compose: `services:
  web:
    image: nginx:1.25
  db:
    image: postgres:16
`,
			service:  "db",
			pinned:   "postgres:16@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			wantLine: "    image: postgres:16@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
		{
			name: "preserves surrounding content",
			compose: `services:
  web:
    image: nginx:1.25
    ports:
      - "80:80"
`,
			service:  "web",
			pinned:   "nginx:1.25@sha256:3608bca1e44ea6c4d268eb6db02260269892c0b42b86bbf1e77a6fa16c3c9282",
			wantLine: "    image: nginx:1.25@sha256:3608bca1e44ea6c4d268eb6db02260269892c0b42b86bbf1e77a6fa16c3c9282",
		},
		{
			// The Pi's dnscrypt-proxy shape: an earlier service references the
			// target service in depends_on (same-named key!), and the target's
			// image line comes after labels.
			name: "depends_on key must not shadow the service",
			compose: `services:
  web:
    image: nginx:1.25
    depends_on:
      dns:
        condition: service_healthy
  dns:
    labels:
      diun.include_tags: '^\d+$'
    image: dnscrypt:2.1.15
`,
			service:  "dns",
			pinned:   "dnscrypt:2.1.15@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			wantLine: "    image: dnscrypt:2.1.15@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
		{
			name: "image-like key under labels is not rewritten",
			compose: `services:
  web:
    labels:
      image: not-an-image
    image: nginx:1.25
`,
			service:  "web",
			pinned:   "nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			wantLine: "    image: nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
		{
			name: "top-level key matching service name is ignored",
			compose: `volumes:
  web:
services:
  web:
    image: nginx:1.25
`,
			service:  "web",
			pinned:   "nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			wantLine: "    image: nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
		{
			name: "inline comment on the image line is preserved",
			compose: `services:
  web:
    image: nginx:1.25  # renovate: keep on 1.x
`,
			service:  "web",
			pinned:   "nginx:1.26@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			wantLine: "    image: nginx:1.26@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad  # renovate: keep on 1.x",
		},
		{
			name: "comments and blank lines inside blocks are neutral",
			compose: `services:
  web:
    # pinned by docker-pin
    restart: unless-stopped

    image: nginx:1.25
`,
			service:  "web",
			pinned:   "nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			wantLine: "    image: nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := writeTempCompose(t, tt.compose)
			if err := PinImage(file, tt.service, tt.pinned); err != nil {
				t.Fatalf("PinImage error: %v", err)
			}

			data, _ := os.ReadFile(file)
			found := false
			for _, line := range splitLines(string(data)) {
				if line == tt.wantLine {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected line %q not found in:\n%s", tt.wantLine, string(data))
			}
		})
	}
}

func TestPinImage_UnknownService(t *testing.T) {
	file := writeTempCompose(t, sampleCompose)
	err := PinImage(file, "nonexistent", "image:tag@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
	if err == nil {
		t.Error("expected error for unknown service")
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func TestIsBuilt(t *testing.T) {
	f := filepath.Join(t.TempDir(), "compose.yml")
	writeFile(t, f, `services:
  pulled:
    image: nginx:1.25
  builtShort:
    build: ./app
  builtLong:
    build:
      context: ./app
      dockerfile: Dockerfile
  builtWithImage:
    build: ./app
    image: myapp:local
`)
	for _, tc := range []struct {
		service string
		want    bool
	}{
		{"pulled", false},
		{"builtShort", true},
		{"builtLong", true},
		// `image:` alongside `build:` names the tag the build is written to --
		// it is still a local build, not something a registry serves.
		{"builtWithImage", true},
	} {
		got, err := IsBuilt(f, tc.service)
		if err != nil {
			t.Fatalf("IsBuilt(%s): %v", tc.service, err)
		}
		if got != tc.want {
			t.Errorf("IsBuilt(%s) = %v, want %v", tc.service, got, tc.want)
		}
	}

	if _, err := IsBuilt(f, "nope"); err == nil {
		t.Error("expected an error for an unknown service")
	}
}

// --- error paths and edge cases -----------------------------------------

func TestLocate_NoComposeFileAnywhere(t *testing.T) {
	dir := t.TempDir()
	if _, err := FindFile(dir); err == nil {
		t.Error("a directory with no compose file (and no parent with one) must be an error")
	}
}

func TestParseImage_Errors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	writeFile(t, f, "services:\n  app:\n    image: x/y:1.0.0\n  noimage:\n    build: ./x\n")

	if _, _, err := ParseImage(f, "nope"); err == nil {
		t.Error("unknown service must be an error")
	}
	if _, _, err := ParseImage(f, "noimage"); err == nil {
		t.Error("a service with no image: must be an error")
	}
	if _, _, err := ParseImage(filepath.Join(t.TempDir(), "gone.yml"), "app"); err == nil {
		t.Error("missing file must be an error")
	}
}

// A tag is optional in compose; an image without one means latest.
func TestParseImage_DefaultsToLatest(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	writeFile(t, f, "services:\n  app:\n    image: nginx\n")
	base, tag, err := ParseImage(f, "app")
	if err != nil {
		t.Fatal(err)
	}
	if base != "nginx" || tag != "latest" {
		t.Errorf("got %q / %q, want nginx / latest", base, tag)
	}
}

func TestRawImage_Errors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	writeFile(t, f, "services:\n  app:\n    image: x/y:1.0.0\n")

	if _, err := RawImage(f, "nope"); err == nil {
		t.Error("unknown service must be an error")
	}
	if _, err := RawImage(filepath.Join(t.TempDir(), "gone.yml"), "app"); err == nil {
		t.Error("missing file must be an error")
	}
}

func TestIsBuilt_Errors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	writeFile(t, f, "services:\n  app:\n    image: x/y:1.0.0\n")

	if _, err := IsBuilt(f, "nope"); err == nil {
		t.Error("unknown service must be an error")
	}
	if _, err := IsBuilt(filepath.Join(t.TempDir(), "gone.yml"), "app"); err == nil {
		t.Error("missing file must be an error")
	}
}

// The rewrite is line-based precisely so that formatting, comments and
// ordering survive: a YAML round trip would reformat the whole file.
func TestPinImage_PreservesEverythingElse(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	const before = `# top comment
services:

  app:
    image: x/y:1.0.0   # keep me
    environment:
      - A=1

  other:
    image: z/w:2.0.0
`
	writeFile(t, f, before)
	if err := PinImage(f, "app", "x/y:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, f)

	for _, want := range []string{"# top comment", "# keep me", "- A=1", "z/w:2.0.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("rewrite lost %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "x/y:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4") {
		t.Errorf("pin not written:\n%s", got)
	}
}

// Two services can share an image; the rewrite must touch only the one named.
func TestPinImage_TouchesOnlyTheNamedService(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	writeFile(t, f, "services:\n  a:\n    image: same:1.0.0\n  b:\n    image: same:1.0.0\n")
	if err := PinImage(f, "b", "same:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, f)
	if strings.Count(got, "@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4") != 1 {
		t.Errorf("expected exactly one line rewritten:\n%s", got)
	}
	// a is declared first and must be untouched.
	if !strings.Contains(got, "  a:\n    image: same:1.0.0\n") {
		t.Errorf("wrong service rewritten:\n%s", got)
	}
}

func TestListServices_MissingFile(t *testing.T) {
	if _, err := ListServices(filepath.Join(t.TempDir(), "gone.yml")); err == nil {
		t.Error("missing file must be an error")
	}
}

// A broken include must be reported: silently returning the services it could
// read would hide half a project.
func TestListServices_BrokenInclude(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "docker-compose.yml")
	writeFile(t, root, "include:\n  - missing/compose.yml\nservices:\n  a:\n    image: x:1\n")
	if _, err := ListServices(root); err == nil {
		t.Error("an unreadable include must be an error, not a partial result")
	}
}

func TestResolveServiceIn_UnknownService(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "docker-compose.yml")
	writeFile(t, root, "services:\n  a:\n    image: x:1\n")
	if _, err := ResolveServiceIn(root, "nope"); err == nil {
		t.Error("a service in no file must be an error")
	}
}

// A PinImage that cannot find its service must not have written anything.
//
// restorePin is duva's one undo, and it is a PinImage call: if a failed pin
// could leave a partial write behind, the undo would be the thing that
// corrupts the file. The guarantee holds by where the error returns sit, so
// it is worth pinning down rather than leaving to the next refactor.
func TestPinImage_FailureLeavesTheFileByteIdentical(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	original := "services:\n  web:\n    image: nginx:1.27  # keep this comment\n"
	if err := os.WriteFile(file, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := PinImage(file, "not-a-service", "nginx:1.27@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"); err == nil {
		t.Fatal("expected an error for a service that is not there")
	}

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("a failed pin rewrote the file:\n--- was ---\n%s\n--- now ---\n%s", original, got)
	}
}

// HasUnexpandedVariable is the shared answer to "can this reference be acted
// on?", so it has to be right about both halves: catching a variable, and not
// crying wolf on a reference that merely contains a $ or a brace.
func TestHasUnexpandedVariable(t *testing.T) {
	variable := []string{
		"nginx:${TAG}",
		"${REGISTRY}/app:1.0",
		"nginx:${TAG}@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		"nginx:$(TAG)",
		"${IMAGE}",
	}
	for _, image := range variable {
		if !HasUnexpandedVariable(image) {
			t.Errorf("HasUnexpandedVariable(%q) = false, want true", image)
		}
	}

	literal := []string{
		"nginx",
		"nginx:1.25",
		"localhost:5555/app:1.0.1",
		"ghcr.io/miista/duva:1.0.0@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		// A lone $ or brace is not interpolation. Compose only substitutes
		// ${...} and $(...), so neither of these should be refused.
		"weird$name:1.0",
		"weird{name}:1.0",
	}
	for _, image := range literal {
		if HasUnexpandedVariable(image) {
			t.Errorf("HasUnexpandedVariable(%q) = true, want false", image)
		}
	}
}

// findImageLine decides which line gets overwritten, so it is the one function
// here where a wrong answer edits somebody's compose file. Both directions
// matter: finding the right line, and refusing rather than guessing.
func TestFindImageLine_FindsTheRightLine(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		service string
		want    string // the line content expected at the returned index
		prefix  string
	}{
		{
			name:    "the obvious case",
			body:    "services:\n  web:\n    image: nginx:1.25\n",
			service: "web",
			want:    "    image: nginx:1.25",
			prefix:  "    image: ",
		},
		{
			// The second service's image, not the first: the walk has to leave
			// one block before matching in the next.
			name:    "a later service",
			body:    "services:\n  web:\n    image: nginx\n  db:\n    image: postgres:16\n",
			service: "db",
			want:    "    image: postgres:16",
			prefix:  "    image: ",
		},
		{
			// A nested image: key -- under build: -- is at a different indent
			// and must not be mistaken for the service's own.
			name:    "not the nested one",
			body:    "services:\n  web:\n    build:\n      image: ignore-me\n    image: nginx:1.25\n",
			service: "web",
			want:    "    image: nginx:1.25",
			prefix:  "    image: ",
		},
		{
			name:    "quoted service name",
			body:    "services:\n  \"web\":\n    image: nginx:1.25\n",
			service: "web",
			want:    "    image: nginx:1.25",
			prefix:  "    image: ",
		},
		{
			// Blank lines and comments are neutral, not block boundaries.
			name:    "blanks and comments between",
			body:    "services:\n\n  # the front end\n  web:\n\n    # what it runs\n    image: nginx:1.25\n",
			service: "web",
			want:    "    image: nginx:1.25",
			prefix:  "    image: ",
		},
		{
			name:    "unusual indentation",
			body:    "services:\n    web:\n        image: nginx:1.25\n",
			service: "web",
			want:    "        image: nginx:1.25",
			prefix:  "        image: ",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := strings.Split(c.body, "\n")
			idx, prefix, err := findImageLine(lines, c.service)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if lines[idx] != c.want {
				t.Errorf("line %d is %q, want %q", idx, lines[idx], c.want)
			}
			if prefix != c.prefix {
				t.Errorf("prefix = %q, want %q", prefix, c.prefix)
			}
		})
	}
}

// The negative half. Every one of these must be an error: rewriting a line
// picked by a wrong guess is worse than refusing, because the caller has no
// way to tell a bad edit from a good one.
func TestFindImageLine_RefusesRatherThanGuessing(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		service string
	}{
		{
			name:    "no services section at all",
			body:    "version: '3'\nvolumes:\n  data:\n",
			service: "web",
		},
		{
			// services: exists, this service does not. The image: below
			// belongs to someone else and must not be taken.
			name:    "service not present",
			body:    "services:\n  web:\n    image: nginx\n",
			service: "db",
		},
		{
			// A top-level key ends the services section. Anything after it is
			// not a service, however much it looks like one.
			name:    "service named only after services: ended",
			body:    "services:\n  web:\n    image: nginx\nvolumes:\n  db:\n    image: postgres\n",
			service: "db",
		},
		{
			name:    "service present but has no image",
			body:    "services:\n  web:\n    build: .\n    ports:\n      - 80:80\n",
			service: "web",
		},
		{
			// The next service starts before an image: is found, so the first
			// one genuinely has none.
			name:    "image belongs to the next service",
			body:    "services:\n  web:\n    build: .\n  db:\n    image: postgres\n",
			service: "web",
		},
		{
			name:    "empty file",
			body:    "",
			service: "web",
		},
		{
			// A partial name must not match: pinning "web" should never edit
			// "web-backup".
			name:    "a longer service with the same prefix",
			body:    "services:\n  web-backup:\n    image: nginx\n",
			service: "web",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := strings.Split(c.body, "\n")
			idx, prefix, err := findImageLine(lines, c.service)
			if err == nil {
				t.Fatalf("expected an error, got line %d (%q) with prefix %q",
					idx, lines[idx], prefix)
			}
			// An error must come with no line, not with a line the caller is
			// trusted to ignore. PinImage writes to whatever index it is
			// given, so a non-zero index alongside an error is one missed
			// check away from overwriting an unrelated line.
			if idx != 0 || prefix != "" {
				t.Errorf("a refusal should carry no target, got line %d with prefix %q",
					idx, prefix)
			}
		})
	}
}

// The specific way this goes wrong: a top-level key ends the services section,
// and without that boundary the walk keeps going into volumes: or networks:
// and matches a key there. "db" under volumes: is not a service, and its
// image: line -- if the file happens to have one -- is not duva's to rewrite.
//
// Written as its own test because it is the mutation that matters: removing
// the indent == 0 guard makes the "service named only after services: ended"
// case above return a real line number instead of an error, and a test that
// only asserted "some error" would still have caught it while saying nothing
// about what went wrong.
func TestFindImageLine_StopsAtTheEndOfTheServicesSection(t *testing.T) {
	body := "services:\n" +
		"  web:\n" +
		"    image: nginx\n" +
		"volumes:\n" +
		"  db:\n" +
		"    image: postgres\n"
	lines := strings.Split(body, "\n")

	idx, _, err := findImageLine(lines, "db")
	if err == nil {
		t.Fatalf("db is a volume, not a service, but line %d (%q) was offered for rewriting",
			idx, lines[idx])
	}

	// And the service that IS there is still found, so the boundary check has
	// not simply broken the walk.
	idx, _, err = findImageLine(lines, "web")
	if err != nil {
		t.Fatalf("web is a service and should still be found: %v", err)
	}
	if got := lines[idx]; got != "    image: nginx" {
		t.Errorf("found %q, want web's own image line", got)
	}
}

// --- ContainerIndex ---

func TestContainerIndex(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "docker-compose.yml")
	writeFile(t, file, `
services:
  web:
    image: nginx:1.25
    container_name: my-web
  db:
    image: postgres:17
    container_name: my-db
`)

	index, err := ContainerIndex(file)
	if err != nil {
		t.Fatalf("ContainerIndex: %v", err)
	}
	if len(index) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(index), index)
	}
	if got := index["my-web"]; got.Service != "web" || got.File != file {
		t.Errorf("my-web = %+v, want web in %s", got, file)
	}
	if got := index["my-db"]; got.Service != "db" {
		t.Errorf("my-db = %+v, want db", got)
	}
}

// A service with no container_name is absent rather than guessed at: compose
// derives a default from the project and an ordinal, and a table that was
// right most of the time would be worse than one that admits what it does not
// know.
func TestContainerIndex_SkipsServicesWithoutAContainerName(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "docker-compose.yml")
	writeFile(t, file, `
services:
  named:
    image: nginx
    container_name: the-name
  unnamed:
    image: redis
`)

	index, err := ContainerIndex(file)
	if err != nil {
		t.Fatalf("ContainerIndex: %v", err)
	}
	if len(index) != 1 {
		t.Errorf("got %d entries, want only the named one: %+v", len(index), index)
	}
	if _, ok := index["the-name"]; !ok {
		t.Error("the named service is missing")
	}
}

// include: is walked, and a row records the file the service is actually
// declared in -- not the root, which is what the actor would need to rewrite.
func TestContainerIndex_WalksIncludes(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "media", "docker-compose.yml")
	writeFile(t, inner, `
services:
  radarr:
    image: radarr:5
    container_name: radarr
`)
	file := filepath.Join(root, "docker-compose.yml")
	writeFile(t, file, `
include:
  - media/docker-compose.yml
services:
  caddy:
    image: caddy:2
    container_name: caddy
`)

	index, err := ContainerIndex(file)
	if err != nil {
		t.Fatalf("ContainerIndex: %v", err)
	}
	if len(index) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(index), index)
	}
	if got := index["radarr"]; got.File != inner {
		t.Errorf("radarr declared in %q, want %q", got.File, inner)
	}
	if got := index["caddy"]; got.File != file {
		t.Errorf("caddy declared in %q, want %q", got.File, file)
	}
}

// Two services claiming one container name is a compose file that would not
// come up. Picking one silently would decide an update against whichever
// happened to parse last.
func TestContainerIndex_RejectsDuplicateContainerNames(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "other.yml")
	writeFile(t, inner, `
services:
  b:
    image: redis
    container_name: clash
`)
	file := filepath.Join(root, "docker-compose.yml")
	writeFile(t, file, `
include:
  - other.yml
services:
  a:
    image: nginx
    container_name: clash
`)

	_, err := ContainerIndex(file)
	if err == nil {
		t.Fatal("want an error for a duplicated container_name")
	}
	if !strings.Contains(err.Error(), "clash") {
		t.Errorf("the error should name the container, got %v", err)
	}
}

func TestContainerIndex_MissingFile(t *testing.T) {
	if _, err := ContainerIndex(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

// An empty file is not an error -- it is a project with nothing named yet.
func TestContainerIndex_EmptyFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "docker-compose.yml")
	writeFile(t, file, "services: {}\n")

	index, err := ContainerIndex(file)
	if err != nil {
		t.Fatalf("ContainerIndex: %v", err)
	}
	if len(index) != 0 {
		t.Errorf("got %+v, want empty", index)
	}
}

// FileIn requires the compose file in the directory it is given, and says so
// when there is none.
//
// The difference from FindFile is the whole point: a caller whose directory is
// a mount must not be handed a file from outside it. Walking up would leave
// the mount and answer with whatever it found out there -- a wrong answer
// wearing the shape of a right one.
func TestFileInDoesNotWalkUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "empty")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// FindFile climbs out and finds the parent's.
	if got, err := FindFile(sub); err != nil || got == "" {
		t.Fatalf("FindFile(%s) = %q, %v -- it is expected to walk up", sub, got, err)
	}
	// FileIn does not.
	if got, err := FileIn(sub); err == nil {
		t.Errorf("FileIn(%s) = %q, want an error -- there is no compose file there", sub, got)
	}
}

// And it finds one that is there.
func TestFileInFindsTheFileInTheDirectory(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(want, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FileIn(dir)
	if err != nil {
		t.Fatalf("FileIn: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The message names the directory and what was expected, because the fix is to
// mount something else -- and a message that only said "not found" would leave
// someone guessing which of the two mounts was wrong.
func TestFileInSaysWhereItLooked(t *testing.T) {
	dir := t.TempDir()
	_, err := FileIn(dir)
	if err == nil {
		t.Fatal("no error for a directory with no compose file")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the error does not name the directory: %v", err)
	}
	if !strings.Contains(err.Error(), "docker-compose.yml") {
		t.Errorf("the error does not say what it expected: %v", err)
	}
}
