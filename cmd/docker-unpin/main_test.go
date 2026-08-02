package main

import (
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
	chdir(t, dir)
	return f
}

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

func readCompose(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRun(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25@sha256:abc123
`)
	if err := run("web", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if strings.Contains(got, "@sha256:") {
		t.Errorf("expected digest stripped, got:\n%s", got)
	}
	if !strings.Contains(got, "nginx:1.25") {
		t.Errorf("expected tag kept, got:\n%s", got)
	}
}

func TestRun_NotPinned(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25
`)
	if err := run("web", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if readCompose(t, f) != "services:\n  web:\n    image: nginx:1.25\n" {
		t.Errorf("compose file should be unchanged, got:\n%s", readCompose(t, f))
	}
}

func TestRun_DryRun(t *testing.T) {
	f := writeTempCompose(t, `services:
  web:
    image: nginx:1.25@sha256:abc123
`)
	if err := run("web", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "@sha256:abc123") {
		t.Errorf("dry run should not modify the compose file, got:\n%s", got)
	}
}
