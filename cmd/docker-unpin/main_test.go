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
    image: nginx:1.25@sha256:6ca13d52ca70c883e0f0bb101e425a89e8624de51db2d2392593af6a84118090
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
    image: nginx:1.25@sha256:6ca13d52ca70c883e0f0bb101e425a89e8624de51db2d2392593af6a84118090
`)
	if _, err := run("web", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readCompose(t, f)
	if !strings.Contains(got, "@sha256:6ca13d52ca70c883e0f0bb101e425a89e8624de51db2d2392593af6a84118090") {
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
    image: nginx:1.25@sha256:9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0
  alpha:
    image: alpine:3.20@sha256:3e744b9dc39389baf0c5a0660589b8402f3dbb49b89b3e75f2c9355852a3c677
  mango:
    image: redis:7@sha256:64daa44ad493ff28a96effab6e77f1732a3d97d83241581b37dbd70a7a4900fe
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

// unpin deliberately does NOT validate the digest it removes. Stripping is
// textual -- nothing is resolved or pulled -- so a malformed digest is exactly
// the case where unpinning is most useful: it is how a broken pin gets fixed.
//
// `docker pin` refuses such a reference and points here, so if this ever
// started validating too, a broken compose file would have no way out.
func TestUnpinStripsAMalformedDigest(t *testing.T) {
	f := writeTempCompose(t, "services:\n  web:\n    image: nginx:1.25@sha256:abc\n")

	if _, err := run("web", false); err != nil {
		t.Fatalf("unpin should strip a malformed digest, not refuse it: %v", err)
	}
	got := readCompose(t, f)
	if strings.Contains(got, "@sha256:") {
		t.Errorf("the digest should be gone, got:\n%s", got)
	}
	if !strings.Contains(got, "nginx:1.25") {
		t.Errorf("the tag should survive, got:\n%s", got)
	}
}
