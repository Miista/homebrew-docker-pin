package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/watch"
)

func useTempState(t *testing.T) {
	t.Helper()
	old := stateFile
	stateFile = filepath.Join(t.TempDir(), "detector.json")
	t.Cleanup(func() { stateFile = old })
}

func loadOrFail(t *testing.T) *memory {
	t.Helper()
	mem, err := loadMemory(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadMemory: %v", err)
	}
	return mem
}

// A service never checked looks back a window rather than to the beginning of
// time: a watcher with no memory reporting every tag ever published would
// bury the run in noise nobody would read.
func TestAnUncheckedServiceLooksBackAWindow(t *testing.T) {
	useTempState(t)
	mem := loadOrFail(t)

	got := mem.Cutoff("never-seen")
	if time.Since(got) > defaultWindow+time.Minute {
		t.Errorf("cutoff is %v ago, want about %v", time.Since(got), defaultWindow)
	}
	if mem.why == "" {
		t.Error("the memory should say where its cutoffs came from, for the log")
	}
}

func TestARecordedCutoffIsUsed(t *testing.T) {
	useTempState(t)
	want := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	mem := loadOrFail(t)
	mem.Checked("app", want)
	if err := mem.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := loadOrFail(t).Cutoff("app"); !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v", got, want)
	}
}

// The rule the whole split exists for: one service's line is its own.
func TestOneServicesLineDoesNotAffectAnothers(t *testing.T) {
	useTempState(t)
	recorded := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	mem := loadOrFail(t)
	mem.Checked("checked", recorded)
	if err := mem.save(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadOrFail(t)
	if got := reloaded.Cutoff("checked"); !got.Equal(recorded) {
		t.Errorf("the checked service = %v, want %v", got, recorded)
	}
	// The other one still looks back a window, rather than inheriting a line
	// it never earned.
	if got := reloaded.Cutoff("untouched"); time.Since(got) > defaultWindow+time.Minute {
		t.Errorf("an unchecked service inherited %v; it should look back a window", got)
	}
}

// An older state file, from when one line covered every service, still means
// something: it seeds anything without a line of its own, so upgrading does
// not re-report a week.
func TestAnOldWholeRunCutoffSeedsEveryService(t *testing.T) {
	useTempState(t)
	old := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(stateFile,
		[]byte(`{"last_check":"`+old.Format(time.RFC3339)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := loadOrFail(t).Cutoff("anything"); !got.Equal(old) {
		t.Errorf("cutoff = %v, want the old whole-run line %v", got, old)
	}
}

// The override is how you ask "what appeared in the last week" without
// disturbing what is recorded.
func TestSinceOverrideAcceptsADuration(t *testing.T) {
	useTempState(t)
	t.Setenv("DUVA_WATCH_SINCE", "24h")

	mem := loadOrFail(t)
	got := mem.Cutoff("app")
	if d := time.Since(got); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("cutoff is %v ago, want about 24h", d)
	}
	if !strings.Contains(mem.why, "DUVA_WATCH_SINCE") {
		t.Errorf("why = %q, want it to name the override", mem.why)
	}
}

func TestSinceOverrideAcceptsATimestamp(t *testing.T) {
	useTempState(t)
	want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	t.Setenv("DUVA_WATCH_SINCE", want.Format(time.RFC3339))

	if got := loadOrFail(t).Cutoff("app"); !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v", got, want)
	}
}

// An override asks a question; it must not answer for the watcher's memory.
func TestAnOverrideRecordsNothing(t *testing.T) {
	useTempState(t)
	recorded := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	mem := loadOrFail(t)
	mem.Checked("app", recorded)
	if err := mem.save(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DUVA_WATCH_SINCE", "1h")
	overridden := loadOrFail(t)
	overridden.Checked("app", time.Now())
	if err := overridden.save(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DUVA_WATCH_SINCE", "")
	if got := loadOrFail(t).Cutoff("app"); !got.Equal(recorded) {
		t.Errorf("an override overwrote the recorded line: %v, want %v", got, recorded)
	}
}

func TestAMalformedSinceIsAnError(t *testing.T) {
	useTempState(t)
	t.Setenv("DUVA_WATCH_SINCE", "yesterday-ish")

	if _, err := loadMemory(zerolog.Nop()); err == nil {
		t.Error("a malformed DUVA_WATCH_SINCE was accepted")
	}
}

func TestAnUnreadableStateFileFallsBackToTheWindow(t *testing.T) {
	useTempState(t)
	if err := os.WriteFile(stateFile, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadOrFail(t).Cutoff("app")
	if time.Since(got) > defaultWindow+time.Minute {
		t.Errorf("cutoff is %v ago, want about %v", time.Since(got), defaultWindow)
	}
}

// --- what is held ------------------------------------------------------------

// A finding that could not be handed on waits, and survives a restart. This
// is what makes running with no webhook configured a way to defer publishing
// rather than a way to lose findings.
func TestHeldFindingsSurviveARestart(t *testing.T) {
	useTempState(t)

	mem := loadOrFail(t)
	mem.Hold(watch.Finding{Service: "app", Tag: "1.1.0", Image: "example.com/app"})
	if err := mem.save(); err != nil {
		t.Fatal(err)
	}

	held := loadOrFail(t).Held()
	if len(held) != 1 {
		t.Fatalf("held %d findings after a reload, want 1", len(held))
	}
	if held[0].Service != "app" || held[0].Tag != "1.1.0" {
		t.Errorf("held = %+v, want app 1.1.0", held[0])
	}
}

// Handing one on drops it, so it is not sent twice.
func TestAHandedFindingIsNoLongerHeld(t *testing.T) {
	useTempState(t)
	f := watch.Finding{Service: "app", Tag: "1.1.0"}

	mem := loadOrFail(t)
	mem.Hold(f)
	mem.Handed(f)
	if err := mem.save(); err != nil {
		t.Fatal(err)
	}

	if held := loadOrFail(t).Held(); len(held) != 0 {
		t.Errorf("held %d findings, want none after it was handed on", len(held))
	}
}

// A moving tag that moves repeatedly during an outage leaves one entry, not
// one per move -- the gate's queue is keyed by service and would discard all
// but the last anyway.
func TestHoldingTheSameTagTwiceKeepsOne(t *testing.T) {
	useTempState(t)

	mem := loadOrFail(t)
	mem.Hold(watch.Finding{Service: "app", Tag: "latest", Digest: "sha256:aaa"})
	mem.Hold(watch.Finding{Service: "app", Tag: "latest", Digest: "sha256:bbb"})

	held := mem.Held()
	if len(held) != 1 {
		t.Fatalf("held %d, want 1 -- the same tag twice is one finding", len(held))
	}
	if held[0].Digest != "sha256:bbb" {
		t.Errorf("held the older digest %q; the newer one supersedes it", held[0].Digest)
	}
}

// --- writing ------------------------------------------------------------------

func TestSaveCreatesItsDirectory(t *testing.T) {
	dir := t.TempDir()
	old := stateFile
	stateFile = filepath.Join(dir, "nested", "detector.json")
	t.Cleanup(func() { stateFile = old })

	mem := loadOrFail(t)
	mem.Checked("app", time.Now())
	if err := mem.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Errorf("the state file was not written: %v", err)
	}
}

// The write is atomic through a temp file and a rename, so a crash mid-write
// cannot leave a truncated file. The temp must not survive a success.
func TestSaveLeavesNoTempFiles(t *testing.T) {
	useTempState(t)

	mem := loadOrFail(t)
	mem.Checked("app", time.Now())
	if err := mem.save(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Dir(stateFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".detector-") {
			t.Errorf("a temp file survived the save: %s", e.Name())
		}
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

// A service with no recorded cutoff is on its first check, and one that has
// been checked is not.
func TestFirstCheckIsOnlyForAServiceWithNoHistory(t *testing.T) {
	m := &memory{services: map[string]serviceState{
		"checked": {Cutoff: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		// Recorded, but never completed a check: its cutoff is zero, so it is
		// still owed a first one.
		"held": {Pending: map[string]watch.Finding{"1.0.0": {}}},
	}}

	if m.FirstCheck("checked") {
		t.Error("a service with a recorded cutoff was called a first check")
	}
	if !m.FirstCheck("held") {
		t.Error("a service with findings but no completed check is still owed a first one")
	}
	if !m.FirstCheck("brand-new") {
		t.Error("a service never seen before is a first check")
	}
}

// An override is not a first check: DUVA_WATCH_SINCE asks a question against a
// window someone chose, and widening it would make the flag mean something
// else.
func TestAnOverrideIsNeverAFirstCheck(t *testing.T) {
	m := &memory{services: map[string]serviceState{}, override: true}
	if m.FirstCheck("anything") {
		t.Error("DUVA_WATCH_SINCE was treated as a first check, so it would report more than it was asked for")
	}
}
