package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A repository whose hook would reject the updater's message must not be ready.
// This is the bazarr failure, caught before any work instead of after a
// container has been replaced.
func TestReadinessCatchesAHookThatWouldRefuse(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")

	hooks := filepath.Join(dir, ".git", "hooks")
	os.MkdirAll(hooks, 0o755)
	// A hook shaped like this repository's: it demands a "<box>/" prefix.
	hook := "#!/bin/sh\ngrep -q '^[a-z0-9]*/' \"$1\" || exit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	// The default template has no host prefix -- exactly what bazarr hit.
	if err := checkMessageAccepted(dir, ""); err == nil {
		t.Error("a template the hook would reject was reported ready")
	} else if !strings.Contains(err.Error(), "commit-msg") {
		t.Errorf("err = %v, want it to name the hook", err)
	}

	// With the prefix the hook wants, it is ready.
	if err := checkMessageAccepted(dir, "optiplex/{{.Container}}: update to {{.NewVersion}}"); err != nil {
		t.Errorf("a template the hook accepts was refused: %v", err)
	}
}

// No hook means nothing would reject anything: ready.
func TestReadinessIsFineWithNoHook(t *testing.T) {
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-q").Run()
	if err := checkMessageAccepted(dir, ""); err != nil {
		t.Errorf("a repository with no commit-msg hook was refused: %v", err)
	}
}
