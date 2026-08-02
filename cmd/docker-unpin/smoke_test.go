package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSmoke_IncludeLocationIndependent is the live compatibility check for
// include: resolution: docker unpin must resolve and rewrite the same file
// for a given service regardless of which directory it's invoked from. It
// builds the real binary and shells out to the real docker CLI, so it is
// gated behind DOCKER_PIN_SMOKE=1 — run manually after touching
// FindFile/ResolveService:
//
//	DOCKER_PIN_SMOKE=1 go test ./cmd/docker-unpin -run Smoke -v
func TestSmoke_IncludeLocationIndependent(t *testing.T) {
	if os.Getenv("DOCKER_PIN_SMOKE") == "" {
		t.Skip("set DOCKER_PIN_SMOKE=1 to run the live include: smoke test (needs docker)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is required for the smoke test")
	}

	// A syntactically valid digest is enough here: docker unpin only strips
	// the "@sha256:..." suffix textually, it never resolves or verifies it.
	const (
		image  = "hello-world"
		digest = "sha256:c3cbe1cc1aa588a64951ac6286e0df7b27fe2e6324b1001c619bb358770c0178"
	)

	bin := buildBinary(t, "docker-unpin")

	// root/compose.yml includes root/sub/other.yml.
	// Service A ("app") is defined in root, already pinned; service B
	// ("worker") in sub/, already pinned.
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rootCompose := filepath.Join(root, "compose.yml")
	subCompose := filepath.Join(sub, "other.yml")
	pinnedRoot := "include:\n  - sub/other.yml\nservices:\n  app:\n    image: " + image + "@" + digest + "\n"
	pinnedSub := "services:\n  worker:\n    image: " + image + "@" + digest + "\n"
	writeFile(t, rootCompose, pinnedRoot)
	writeFile(t, subCompose, pinnedSub)

	// Case 1: unpin A ("app") from root.
	runUnpinBinary(t, bin, root, "app")
	case1 := readFile(t, rootCompose)

	// Case 2: unpin B ("worker") from root.
	runUnpinBinary(t, bin, root, "worker")
	case2 := readFile(t, subCompose)

	// Reset both files, then repeat from sub/'s directory.
	writeFile(t, rootCompose, pinnedRoot)
	writeFile(t, subCompose, pinnedSub)

	// Case 3: unpin A ("app") from sub/.
	runUnpinBinary(t, bin, sub, "app")
	case3 := readFile(t, rootCompose)

	// Case 4: unpin B ("worker") from sub/.
	runUnpinBinary(t, bin, sub, "worker")
	case4 := readFile(t, subCompose)

	if case1 != case3 {
		t.Errorf("unpinning A from root vs from sub/ should be identical:\nroot:\n%s\nsub:\n%s", case1, case3)
	}
	if case2 != case4 {
		t.Errorf("unpinning B from root vs from sub/ should be identical:\nroot:\n%s\nsub:\n%s", case2, case4)
	}
	if strings.Contains(case1, "@sha256:") {
		t.Errorf("expected app to be unpinned, got:\n%s", case1)
	}
	if strings.Contains(case2, "@sha256:") {
		t.Errorf("expected worker to be unpinned, got:\n%s", case2)
	}
}

func buildBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", bin, "./")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, out)
	}
	return bin
}

func runUnpinBinary(t *testing.T, bin, dir, service string) {
	t.Helper()
	cmd := exec.Command(bin, service)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s (in %s): %v\n%s", bin, service, dir, err, out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
