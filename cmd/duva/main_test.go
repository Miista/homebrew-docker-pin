package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// setupFixture creates a temp dir with a compose file and points the
// composeDir / stateFile package variables at fixtures (in the container
// these are the fixed /compose and /data/duva.json mounts).
func setupFixture(t *testing.T, composeContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeContent), 0o644); err != nil {
		t.Fatal(err)
	}
	origCompose, origState := composeDir, stateFile
	t.Cleanup(func() { composeDir, stateFile = origCompose, origState })
	composeDir = dir
	stateFile = filepath.Join(dir, "state.json")
	return dir
}

const pinnedConstrainedService = `services:
  qui:
    image: ghcr.io/autobrr/qui:1.2.0@sha256:aaa
    labels:
      duva.include: '^\d+\.\d+\.\d+$'
`

const pinnedUnconstrainedService = `services:
  qui:
    image: ghcr.io/autobrr/qui:latest@sha256:aaa
`

const unpinnedService = `services:
  qui:
    image: ghcr.io/autobrr/qui:1.2.0
`

func fakeReg(tags []string) regFuncs {
	return regFuncs{listMatchingTags: func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error) {
		return tags, nil
	}}
}

func fakeDigestReg(digest string) regFuncs {
	return regFuncs{remoteDigest: func(baseImage, tag string) (string, error) { return digest, nil }}
}

func TestCheckServiceFindsNewerTag(t *testing.T) {
	composeFile := filepath.Join(setupFixture(t, pinnedConstrainedService), "docker-compose.yml")
	svc, err := loadServiceRules(composeFile, "qui")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := checkService(composeFile, svc, fakeReg([]string{"1.2.0", "1.3.0", "1.2.1"}), map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate != "1.3.0" {
		t.Fatalf("candidate = %q, want 1.3.0", candidate)
	}
}

func TestCheckServiceNoNewerTag(t *testing.T) {
	composeFile := filepath.Join(setupFixture(t, pinnedConstrainedService), "docker-compose.yml")
	svc, err := loadServiceRules(composeFile, "qui")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := checkService(composeFile, svc, fakeReg([]string{"1.2.0", "1.1.0"}), map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate != "" {
		t.Fatalf("candidate = %q, want empty", candidate)
	}
}

func TestCheckServiceMovingTag_FirstCheckRecordsBaselineWithoutNotifying(t *testing.T) {
	composeFile := filepath.Join(setupFixture(t, pinnedUnconstrainedService), "docker-compose.yml")
	svc, err := loadServiceRules(composeFile, "qui")
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]string{}
	candidate, err := checkService(composeFile, svc, fakeDigestReg("sha256:aaa"), st)
	if err != nil {
		t.Fatal(err)
	}
	if candidate != "" {
		t.Fatalf("candidate = %q, want empty on first check (baseline recorded, not notified)", candidate)
	}
	if st["qui"] != "sha256:aaa" {
		t.Fatalf("st[qui] = %q, want sha256:aaa recorded as baseline", st["qui"])
	}
}

func TestCheckServiceMovingTag_DigestChangeIsReported(t *testing.T) {
	composeFile := filepath.Join(setupFixture(t, pinnedUnconstrainedService), "docker-compose.yml")
	svc, err := loadServiceRules(composeFile, "qui")
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]string{"qui": "sha256:aaa"}
	candidate, err := checkService(composeFile, svc, fakeDigestReg("sha256:bbb"), st)
	if err != nil {
		t.Fatal(err)
	}
	if candidate != "sha256:bbb" {
		t.Fatalf("candidate = %q, want sha256:bbb", candidate)
	}
}

func TestCheckServiceMovingTag_SameDigestNotReported(t *testing.T) {
	composeFile := filepath.Join(setupFixture(t, pinnedUnconstrainedService), "docker-compose.yml")
	svc, err := loadServiceRules(composeFile, "qui")
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]string{"qui": "sha256:aaa"}
	candidate, err := checkService(composeFile, svc, fakeDigestReg("sha256:aaa"), st)
	if err != nil {
		t.Fatal(err)
	}
	if candidate != "" {
		t.Fatalf("candidate = %q, want empty (digest unchanged)", candidate)
	}
}

func TestRunOnceNotifiesOnceThenDedupes(t *testing.T) {
	setupFixture(t, pinnedConstrainedService)

	reg := fakeReg([]string{"1.2.0", "1.3.0"})
	var out bytes.Buffer
	if err := runOnce(reg, &out); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	if !bytes.Contains([]byte(first), []byte("1.3.0 available\n")) {
		t.Fatalf("first run output = %q, want it to report 1.3.0 available", first)
	}

	st, err := loadState(stateFile)
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

func TestRunOnceMovingTagBaselineThenNotifiesOnChange(t *testing.T) {
	setupFixture(t, pinnedUnconstrainedService)

	var out bytes.Buffer
	if err := runOnce(fakeDigestReg("sha256:aaa"), &out); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("available\n")) {
		t.Fatalf("first run output = %q, want no notification on initial baseline", out.String())
	}
	st, err := loadState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if st["qui"] != "sha256:aaa" {
		t.Fatalf("state[qui] = %q, want sha256:aaa baseline recorded", st["qui"])
	}

	out.Reset()
	if err := runOnce(fakeDigestReg("sha256:bbb"), &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("sha256:bbb available\n")) {
		t.Fatalf("second run output = %q, want it to report the changed digest", out.String())
	}
}

func TestRunOnceSkipsUnpinnedServices(t *testing.T) {
	setupFixture(t, unpinnedService)

	var out bytes.Buffer
	if err := runOnce(fakeReg(nil), &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("qui: not pinned, skipping\n")) {
		t.Fatalf("output = %q, want it to skip the unpinned service", out.String())
	}

	st, err := loadState(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 0 {
		t.Fatalf("state = %v, want empty — unpinned services must never be state-tracked", st)
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
