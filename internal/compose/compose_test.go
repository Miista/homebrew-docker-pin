package compose

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestFindFile_PreferNearest(t *testing.T) {
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
	if got != subCompose {
		t.Errorf("FindFile should return nearest file, got %q, want %q", got, subCompose)
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
