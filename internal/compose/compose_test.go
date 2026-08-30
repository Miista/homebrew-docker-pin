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
		{"nginx:1.25@sha256:abc", "nginx", "1.25"},
		{"ghcr.io/home-assistant/home-assistant:stable", "ghcr.io/home-assistant/home-assistant", "stable"},
		{"ghcr.io/home-assistant/home-assistant:2024.6.1@sha256:deadbeef", "ghcr.io/home-assistant/home-assistant", "2024.6.1"},
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
		{"db", "postgres:16.2@sha256:abc123"},
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
    image: postgres:16.2@sha256:abc123
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
			pinned:   "nginx:1.25@sha256:deadbeef",
			wantLine: "    image: nginx:1.25@sha256:deadbeef",
		},
		{
			name: "replaces existing digest",
			compose: `services:
  web:
    image: nginx:1.25@sha256:olddigest
`,
			service:  "web",
			pinned:   "nginx:1.25@sha256:newdigest",
			wantLine: "    image: nginx:1.25@sha256:newdigest",
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
			pinned:   "postgres:16@sha256:abc",
			wantLine: "    image: postgres:16@sha256:abc",
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
			pinned:   "nginx:1.25@sha256:xyz",
			wantLine: "    image: nginx:1.25@sha256:xyz",
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
			pinned:   "dnscrypt:2.1.15@sha256:abc",
			wantLine: "    image: dnscrypt:2.1.15@sha256:abc",
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
			pinned:   "nginx:1.25@sha256:abc",
			wantLine: "    image: nginx:1.25@sha256:abc",
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
			pinned:   "nginx:1.25@sha256:abc",
			wantLine: "    image: nginx:1.25@sha256:abc",
		},
		{
			name: "inline comment on the image line is preserved",
			compose: `services:
  web:
    image: nginx:1.25  # renovate: keep on 1.x
`,
			service:  "web",
			pinned:   "nginx:1.26@sha256:abc",
			wantLine: "    image: nginx:1.26@sha256:abc  # renovate: keep on 1.x",
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
			pinned:   "nginx:1.25@sha256:abc",
			wantLine: "    image: nginx:1.25@sha256:abc",
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
	err := PinImage(file, "nonexistent", "image:tag@sha256:abc")
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
	if err := PinImage(f, "app", "x/y:1.0.0@sha256:d"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, f)

	for _, want := range []string{"# top comment", "# keep me", "- A=1", "z/w:2.0.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("rewrite lost %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "x/y:1.0.0@sha256:d") {
		t.Errorf("pin not written:\n%s", got)
	}
}

// Two services can share an image; the rewrite must touch only the one named.
func TestPinImage_TouchesOnlyTheNamedService(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	writeFile(t, f, "services:\n  a:\n    image: same:1.0.0\n  b:\n    image: same:1.0.0\n")
	if err := PinImage(f, "b", "same:1.0.0@sha256:d"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, f)
	if strings.Count(got, "@sha256:d") != 1 {
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

	if err := PinImage(file, "not-a-service", "nginx:1.27@sha256:abc"); err == nil {
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
		"nginx:${TAG}@sha256:abc",
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
		"ghcr.io/miista/duva:1.0.0@sha256:abc",
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
