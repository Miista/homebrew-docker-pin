package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateWritableAcceptsAWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := checkStateWritable(filepath.Join(dir, "duva.json")); err != nil {
		t.Errorf("a writable directory was rejected: %v", err)
	}
}

// The directory not existing yet is normal -- a fresh /data volume -- and
// must not be an error as long as it can be created.
func TestStateWritableCreatesAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := checkStateWritable(filepath.Join(dir, "duva.json")); err != nil {
		t.Fatalf("a creatable directory was rejected: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the directory was not created: %v", err)
	}
}

// The case that shipped: the directory exists and duva cannot write to it,
// which is what a bind mount created by the daemon as root looks like to a
// container running as the host user. Today this is discovered after a whole
// check has run and its results are being thrown away.
func TestStateWritableRejectsAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere, so there is nothing to reject")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	err := checkStateWritable(filepath.Join(dir, "duva.json"))
	if err == nil {
		t.Fatal("an unwritable state directory was accepted")
	}
	// The message has to be actionable: someone reading it at 3am should
	// not have to know how bind mounts get their ownership.
	if !strings.Contains(err.Error(), "chown") {
		t.Errorf("the error should say how to fix it, got: %v", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the error should name the directory, got: %v", err)
	}
}

// Leaving no debris behind: the probe file must not survive the check, or
// duva litters its own state directory once per start.
func TestStateWritableLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if err := checkStateWritable(filepath.Join(dir, "duva.json")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the probe was left behind: %v", entries)
	}
}

func TestComposeReadableAcceptsADirectory(t *testing.T) {
	if err := checkComposeReadable(t.TempDir()); err != nil {
		t.Errorf("a real directory was rejected: %v", err)
	}
}

func TestComposeReadableRejectsAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	err := checkComposeReadable(missing)
	if err == nil {
		t.Fatal("a missing compose directory was accepted")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error should name the path, got: %v", err)
	}
}

// A mistyped DUVA_COMPOSE_SUBDIR pointing at a file rather than a directory
// would otherwise surface as every service being unwatched, which reads as
// "nothing to do".
func TestComposeReadableRejectsAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(f, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkComposeReadable(f); err == nil {
		t.Fatal("a file was accepted as the compose project directory")
	}
}

// A hub keeps no state and reads no compose file, so requiring either would
// defeat the point of it holding neither.
func TestPreflightSkipsAHub(t *testing.T) {
	oldState, oldCompose := stateFile, composeDir
	t.Cleanup(func() { stateFile, composeDir = oldState, oldCompose })
	stateFile = "/nonexistent/duva.json"
	composeDir = "/nonexistent"

	if err := preflight(envConfig{Mode: ModeHub}); err != nil {
		t.Errorf("a hub was held to an agent's preconditions: %v", err)
	}
}

// An agent is held to them.
func TestPreflightChecksAnAgent(t *testing.T) {
	oldState, oldCompose := stateFile, composeDir
	t.Cleanup(func() { stateFile, composeDir = oldState, oldCompose })
	stateFile = "/nonexistent/duva.json"
	composeDir = "/nonexistent"

	if err := preflight(envConfig{Mode: ModeAgent}); err == nil {
		t.Error("an agent with no compose project started anyway")
	}
}

// And so is a local duva, which is the same process doing the same work.
func TestPreflightChecksALocalDuva(t *testing.T) {
	oldState, oldCompose := stateFile, composeDir
	t.Cleanup(func() { stateFile, composeDir = oldState, oldCompose })
	stateFile = "/nonexistent/duva.json"
	composeDir = "/nonexistent"

	if err := preflight(envConfig{Mode: ModeLocal}); err == nil {
		t.Error("a local duva with no compose project started anyway")
	}
}

// The happy path: a correctly configured agent passes.
func TestPreflightAcceptsAGoodAgent(t *testing.T) {
	oldState, oldCompose := stateFile, composeDir
	t.Cleanup(func() { stateFile, composeDir = oldState, oldCompose })
	root := t.TempDir()
	composeDir = root
	stateFile = filepath.Join(root, "data", "duva.json")

	if err := preflight(envConfig{Mode: ModeAgent}); err != nil {
		t.Errorf("a correctly configured agent was rejected: %v", err)
	}
}
