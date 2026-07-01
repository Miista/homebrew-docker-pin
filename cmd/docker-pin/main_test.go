package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempCompose(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func readCompose(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// --- pinInFile ---

func TestPinInFile_AlreadyPinned(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25@sha256:existing
`)
	called := false
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { called = true; return "", errors.New("should not be called") },
		pull:      func(ref string) error { called = true; return errors.New("should not be called") },
	}
	if err := pinInFile(f, "web", d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("docker should not be called when image is already pinned")
	}
	if !strings.Contains(readCompose(t, f), "nginx:1.25@sha256:existing") {
		t.Error("compose file should be unchanged")
	}
}

func TestPinInFile_LocalImage(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	pulled := false
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:localhash", nil },
		pull:      func(ref string) error { pulled = true; return nil },
	}
	if err := pinInFile(f, "web", d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pulled {
		t.Error("should not pull when image is available locally")
	}
	if !strings.Contains(readCompose(t, f), "nginx:1.25@sha256:localhash") {
		t.Errorf("expected pinned image in compose, got:\n%s", readCompose(t, f))
	}
}

func TestPinInFile_PullsWhenNotLocal(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	callCount := 0
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			callCount++
			if callCount == 1 {
				return "", errors.New("not found locally")
			}
			return "sha256:pulledhash", nil
		},
		pull: func(ref string) error { return nil },
	}
	if err := pinInFile(f, "web", d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "sha256:pulledhash") {
		t.Errorf("expected pinned image after pull, got:\n%s", readCompose(t, f))
	}
}

func TestPinInFile_PullFails(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "", errors.New("not found") },
		pull:      func(ref string) error { return errors.New("pull failed: network error") },
	}
	err := pinInFile(f, "web", d)
	if err == nil {
		t.Fatal("expected error when pull fails")
	}
	if !strings.Contains(err.Error(), "pull failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPinInFile_UnknownService(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:abc", nil },
		pull:      func(ref string) error { return nil },
	}
	if err := pinInFile(f, "nonexistent", d); err == nil {
		t.Error("expected error for unknown service")
	}
}

// --- upgradeInFile ---

func TestUpgradeInFile_AlreadyUpToDate(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16.3@sha256:currenthash
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:currenthash", nil },
		pull:      func(ref string) error { return nil },
	}
	if err := upgradeInFile(f, "db", "16.3", d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(readCompose(t, f), "sha256:currenthash") {
		t.Errorf("compose should be unchanged when already up to date")
	}
}

func TestUpgradeInFile_NewDigest(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16.3@sha256:oldhash
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:newhash", nil },
		pull:      func(ref string) error { return nil },
	}
	if err := upgradeInFile(f, "db", "16.3", d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "sha256:newhash") {
		t.Errorf("expected new digest in compose, got:\n%s", got)
	}
	if strings.Contains(got, "sha256:oldhash") {
		t.Errorf("old digest should be gone, got:\n%s", got)
	}
}

func TestUpgradeInFile_PullFails(t *testing.T) {
	f := writeTempCompose(t, `services:
  db:
    image: postgres:16
`)
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "", errors.New("not found") },
		pull:      func(ref string) error { return errors.New("registry unreachable") },
	}
	err := upgradeInFile(f, "db", "16", d)
	if err == nil {
		t.Fatal("expected error when pull fails")
	}
}

// --- digestOf ---

func TestDigestOf(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"nginx:1.25@sha256:abc123", "sha256:abc123"},
		{"nginx:1.25", ""},
		{"nginx", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := digestOf(tt.input)
			if got != tt.want {
				t.Errorf("digestOf(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- pinAll / upgradeAll via temp dir ---

func TestPinAll(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
  db:
    image: postgres:16
`)
	calls := map[string]int{}
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			calls[ref]++
			return fmt.Sprintf("sha256:digest-%s", ref), nil
		},
		pull: func(ref string) error { return nil },
	}

	// pinAll uses os.Getwd + FindFile, so exercise pinInFile directly per service
	for _, svc := range []string{"web", "db"} {
		if err := pinInFile(f, svc, d); err != nil {
			t.Fatalf("pinInFile(%s): %v", svc, err)
		}
	}

	got := readCompose(t, f)
	if !strings.Contains(got, "@sha256:") {
		t.Errorf("expected both services pinned, got:\n%s", got)
	}
	if strings.Count(got, "@sha256:") != 2 {
		t.Errorf("expected 2 pinned services, got:\n%s", got)
	}
}
