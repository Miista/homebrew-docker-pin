package main

import (
	"io"
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
	if _, err := run("web", false); err != nil {
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
	if _, err := run("web", false); err != nil {
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
	if _, err := run("web", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "@sha256:abc123") {
		t.Errorf("dry run should not modify the compose file, got:\n%s", got)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

// The dry-run summary is sorted rather than left in compose-file order, so it
// is easy to scan and to diff between runs.
func TestRunAll_SummaryIsSorted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(`services:
  web:
    image: nginx:1.25@sha256:aaa
  alpha:
    image: alpine:3.20@sha256:bbb
  mango:
    image: redis:7@sha256:ccc
`), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	out := captureStdout(t, func() {
		if err := runAll(true); err != nil {
			t.Errorf("runAll: %v", err)
		}
	})

	var got []string
	seenHeader := false
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "SERVICE" {
			seenHeader = true
			continue
		}
		if seenHeader {
			got = append(got, f[0])
		}
	}

	want := []string{"alpha", "mango", "web"}
	if len(got) != len(want) {
		t.Fatalf("expected %d rows, got %d:\n%s", len(want), len(got), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("summary not sorted: got %v, want %v", got, want)
		}
	}
}
