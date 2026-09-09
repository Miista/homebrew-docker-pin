package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withComposeDir points composeDir at a fixture directory, restoring it after
// the test -- the same pattern setupFixture uses for composeDir/stateFile.
func withComposeDir(t *testing.T, dir string) {
	t.Helper()
	orig := composeDir
	t.Cleanup(func() { composeDir = orig })
	composeDir = dir
}

// withEnv sets an env var for the duration of the test, restoring whatever
// was there before (including "unset", so a test does not leak into another).
func withEnv(t *testing.T, key, value string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, orig)
		} else {
			os.Unsetenv(key)
		}
	})
	os.Setenv(key, value)
}

func TestProjectDir_DefaultsToComposeDir(t *testing.T) {
	withComposeDir(t, "/compose")
	withEnv(t, "DUVA_COMPOSE_SUBDIR", "")

	got, err := projectDir()
	if err != nil {
		t.Fatalf("projectDir: %v", err)
	}
	if got != "/compose" {
		t.Errorf("projectDir() = %q, want /compose", got)
	}
}

func TestProjectDir_JoinsSubdir(t *testing.T) {
	withComposeDir(t, "/compose")
	withEnv(t, "DUVA_COMPOSE_SUBDIR", "optiplex")

	got, err := projectDir()
	if err != nil {
		t.Fatalf("projectDir: %v", err)
	}
	want := filepath.Join("/compose", "optiplex")
	if got != want {
		t.Errorf("projectDir() = %q, want %q", got, want)
	}
}

func TestProjectDir_RejectsAbsoluteSubdir(t *testing.T) {
	withComposeDir(t, "/compose")
	withEnv(t, "DUVA_COMPOSE_SUBDIR", "/etc")

	if _, err := projectDir(); err == nil {
		t.Error("projectDir() with an absolute DUVA_COMPOSE_SUBDIR should error, got nil")
	}
}

func TestProjectDir_RejectsEscape(t *testing.T) {
	withComposeDir(t, "/compose")
	withEnv(t, "DUVA_COMPOSE_SUBDIR", "../etc")

	if _, err := projectDir(); err == nil {
		t.Error("projectDir() with a DUVA_COMPOSE_SUBDIR that escapes composeDir should error, got nil")
	}
}
