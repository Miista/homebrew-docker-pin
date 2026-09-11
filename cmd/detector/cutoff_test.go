package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func useTempState(t *testing.T) {
	t.Helper()
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "detector.json")
	t.Cleanup(func() { stateFile = old })
}

// A first run looks back a window rather than to the beginning of time: a
// detector with no memory reporting every tag ever published would bury the
// run in noise nobody would read.
func TestFirstRunLooksBackAWindow(t *testing.T) {
	useTempState(t)
	cutoff, source, err := loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff: %v", err)
	}
	if time.Since(cutoff) > defaultWindow+time.Minute {
		t.Errorf("cutoff is %v ago, want about %v", time.Since(cutoff), defaultWindow)
	}
	if source == "" {
		t.Error("the source should say where the cutoff came from, for the log")
	}
}

func TestASavedCutoffIsUsed(t *testing.T) {
	useTempState(t)
	want := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := saveCutoff(want); err != nil {
		t.Fatalf("saveCutoff: %v", err)
	}
	got, _, err := loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v", got, want)
	}
}

// The override is how you ask "what appeared in the last week" without
// disturbing the recorded cutoff.
func TestSinceOverrideAcceptsADuration(t *testing.T) {
	useTempState(t)
	t.Setenv("DETECTOR_SINCE", "24h")
	cutoff, source, err := loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff: %v", err)
	}
	if d := time.Since(cutoff); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("cutoff is %v ago, want about 24h", d)
	}
	if source == "" {
		t.Error("the source should name the override")
	}
}

func TestSinceOverrideAcceptsATimestamp(t *testing.T) {
	useTempState(t)
	t.Setenv("DETECTOR_SINCE", "2026-09-01T00:00:00Z")
	cutoff, _, err := loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff: %v", err)
	}
	if !cutoff.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("cutoff = %v", cutoff)
	}
}

// A malformed override is an error, not a silent fallback: falling back would
// mean checking a different window than was asked for, and nobody would know.
func TestAMalformedSinceIsAnError(t *testing.T) {
	useTempState(t)
	t.Setenv("DETECTOR_SINCE", "last tuesday")
	if _, _, err := loadCutoff(zerolog.Nop()); err == nil {
		t.Fatal("want an error for an unparseable DETECTOR_SINCE")
	}
}

// An unreadable state file falls back to the window rather than to the zero
// time: the zero time would report every tag ever published.
func TestAnUnreadableStateFileFallsBackToTheWindow(t *testing.T) {
	useTempState(t)
	if err := os.WriteFile(stateFile, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	cutoff, _, err := loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff: %v", err)
	}
	if time.Since(cutoff) > defaultWindow+time.Minute {
		t.Errorf("cutoff is %v ago, want the window", time.Since(cutoff))
	}
}

// A zero timestamp in an otherwise valid file is the same case.
func TestAZeroCutoffFallsBackToTheWindow(t *testing.T) {
	useTempState(t)
	if err := os.WriteFile(stateFile, []byte(`{"last_check":"0001-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cutoff, _, err := loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff: %v", err)
	}
	if time.Since(cutoff) > defaultWindow+time.Minute {
		t.Errorf("cutoff is %v ago, want the window", time.Since(cutoff))
	}
}

// Saving creates its directory: a fresh /data volume has nothing in it.
func TestSaveCreatesItsDirectory(t *testing.T) {
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "nested", "detector.json")
	t.Cleanup(func() { stateFile = old })

	if err := saveCutoff(time.Now()); err != nil {
		t.Fatalf("saveCutoff: %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Errorf("the state file was not written: %v", err)
	}
}

// Saving leaves nothing behind: the atomic write uses a temp file in the same
// directory, and a stray one per run would accumulate on the volume.
func TestSaveLeavesNoTempFiles(t *testing.T) {
	useTempState(t)
	if err := saveCutoff(time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(stateFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d files, want only the state file: %v", len(entries), entries)
	}
}

// --- the project path --------------------------------------------------------

func TestProjectFileHonoursASubdirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pi")
	os.MkdirAll(sub, 0o755)
	want := filepath.Join(sub, "docker-compose.yml")
	os.WriteFile(want, []byte("services: {}\n"), 0o644)

	old := composeDir
	composeDir = dir
	t.Cleanup(func() { composeDir = old })

	got, err := projectFile("pi")
	if err != nil {
		t.Fatalf("projectFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A subdirectory that climbs out must be refused rather than silently
// resolving somewhere else.
func TestProjectFileRefusesToEscape(t *testing.T) {
	old := composeDir
	composeDir = t.TempDir()
	t.Cleanup(func() { composeDir = old })

	for _, sub := range []string{"../etc", "..", "/absolute"} {
		if _, err := projectFile(sub); err == nil {
			t.Errorf("projectFile(%q) was accepted", sub)
		}
	}
}
