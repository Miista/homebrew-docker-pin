package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// chdirTemp creates a temp dir with a compose file and pin.yaml and makes
// it the working directory for the test, mirroring docker-pin's own
// schedule_test.go fixture helper.
func chdirTemp(t *testing.T, composeContent, pinContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if pinContent != "" {
		if err := os.WriteFile(filepath.Join(dir, "pin.yaml"), []byte(pinContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

const oneService = `services:
  qui:
    image: ghcr.io/autobrr/qui:1.2.0
`

const pinConfig = `
schedule: "0 0 * * *"
services:
  - name: qui
    tags: '^\d+\.\d+\.\d+$'
`

func fakeReg(tags []string) regFuncs {
	return regFuncs{listTags: func(baseImage string) ([]string, error) { return tags, nil }}
}

func TestCheckServiceFindsNewerTag(t *testing.T) {
	dir := chdirTemp(t, oneService, pinConfig)
	cfg, composeFile, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	_ = dir
	candidate, err := checkService(composeFile, cfg.Services[0], fakeReg([]string{"1.2.0", "1.3.0", "1.2.1"}))
	if err != nil {
		t.Fatal(err)
	}
	if candidate != "1.3.0" {
		t.Fatalf("candidate = %q, want 1.3.0", candidate)
	}
}

func TestCheckServiceNoNewerTag(t *testing.T) {
	chdirTemp(t, oneService, pinConfig)
	cfg, composeFile, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := checkService(composeFile, cfg.Services[0], fakeReg([]string{"1.2.0", "1.1.0"}))
	if err != nil {
		t.Fatal(err)
	}
	if candidate != "" {
		t.Fatalf("candidate = %q, want empty", candidate)
	}
}

func TestCheckServiceSkipsUnconstrained(t *testing.T) {
	chdirTemp(t, oneService, `
schedule: "0 0 * * *"
services:
  - qui
`)
	cfg, composeFile, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := checkService(composeFile, cfg.Services[0], fakeReg([]string{"1.9.9"}))
	if err != nil {
		t.Fatal(err)
	}
	if candidate != "" {
		t.Fatalf("candidate = %q, want empty (unconstrained services are skipped)", candidate)
	}
}

func TestRunOnceNotifiesOnceThenDedupes(t *testing.T) {
	chdirTemp(t, oneService, pinConfig)
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("TAGWATCH_STATE_FILE", statePath)

	reg := fakeReg([]string{"1.2.0", "1.3.0"})
	var out bytes.Buffer
	if err := runOnce(reg, &out); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	if !bytes.Contains([]byte(first), []byte("1.3.0 available\n")) {
		t.Fatalf("first run output = %q, want it to report 1.3.0 available", first)
	}

	st, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st["qui"] != "1.3.0" {
		t.Fatalf("state[qui] = %q, want 1.3.0", st["qui"])
	}

	out.Reset()
	if err := runOnce(reg, &out); err != nil {
		t.Fatal(err)
	}
	second := out.String()
	if !bytes.Contains([]byte(second), []byte("already notified")) {
		t.Fatalf("second run output = %q, want it to note already notified (no repeat notification)", second)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 0 {
		t.Fatalf("loadState on missing file = %v, want empty map", st)
	}
	st["qui"] = "1.3.0"
	if err := saveState(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["qui"] != "1.3.0" {
		t.Fatalf("reloaded state[qui] = %q, want 1.3.0", got["qui"])
	}
}
