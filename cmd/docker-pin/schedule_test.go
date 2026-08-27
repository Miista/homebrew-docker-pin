package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/schedule"
)

// chdirTemp creates a temp dir with a compose file and pin.yaml and makes
// it the working directory for the test.
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
	// Return the resolved path (macOS tempdirs are behind a /private symlink).
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}


func fakeSys(goos string, euid int) sysFuncs {
	return sysFuncs{
		goos:      goos,
		euid:      func() int { return euid },
		execPath:  func() (string, error) { return "/usr/bin/docker-pin", nil },
		systemctl: func(args ...string) (string, error) { return "", nil },
		analyze:   func(args ...string) (string, error) { return "", nil },
		shell:     func(dir, command string, extraEnv []string) error { return nil },
		composeUp: func(file, service string) error { return nil },
	}
}

const twoServices = `services:
  caddy:
    image: caddy:2.7.6@sha256:old1
  cloudflared:
    image: cloudflared:1.0.0@sha256:old2
`

// --- apply / remove gating ---

func TestScheduleApply_RequiresLinux(t *testing.T) {
	err := scheduleApply(fakeSys("darwin", 0))
	if err == nil || !strings.Contains(err.Error(), "requires systemd (Linux)") {
		t.Fatalf("want systemd error, got %v", err)
	}
}

func TestScheduleApply_RequiresRoot(t *testing.T) {
	err := scheduleApply(fakeSys("linux", 1000))
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("want sudo hint, got %v", err)
	}
}

func TestScheduleRemove_RequiresLinux(t *testing.T) {
	err := scheduleRemove(fakeSys("darwin", 0))
	if err == nil || !strings.Contains(err.Error(), "requires systemd (Linux)") {
		t.Fatalf("want systemd error, got %v", err)
	}
}

// --- validateSchedule ---

func TestValidateSchedule(t *testing.T) {
	dir := chdirTemp(t, twoServices, "")
	composeFile := filepath.Join(dir, "docker-compose.yml")

	tests := []struct {
		name    string
		cfg     schedule.Config
		errPart string
	}{
		{"ok all services", schedule.Config{Schedule: "0 6 * * 1"}, ""},
		{"ok listed", schedule.Config{Schedule: "0 6 * * 1", Services: []schedule.Service{{Name: "caddy"}}}, ""},
		{"unknown service", schedule.Config{Schedule: "0 6 * * 1", Services: []schedule.Service{{Name: "nope"}}}, `service "nope"`},
		{"bad cron", schedule.Config{Schedule: "0 6 1 * 1"}, "day-of-month and day-of-week"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSchedule(&tt.cfg, composeFile)
			if tt.errPart == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errPart) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.errPart)
			}
		})
	}
}

// --- schedule run ---

func TestScheduleRun_UpgradesListedAndRunsHooks(t *testing.T) {
	dir := chdirTemp(t, twoServices, "schedule: \"0 6 * * 1\"\nservices:\n  - caddy\non_change: ./hook.sh\n")

	var pulled []string
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:new1", nil },
		pull:      func(ref string) error { pulled = append(pulled, ref); return nil },
	}
	composeUps, shells := 0, 0
	sys := fakeSys("linux", 1000)
	sys.composeUp = func(file, service string) error {
		composeUps++
		if file != filepath.Join(dir, "docker-compose.yml") || service != "caddy" {
			t.Errorf("composeUp got file=%q service=%q", file, service)
		}
		return nil
	}
	sys.shell = func(shellDir, command string, extraEnv []string) error {
		shells++
		if shellDir != dir || command != "./hook.sh" {
			t.Errorf("shell got dir=%q command=%q", shellDir, command)
		}
		env := strings.Join(extraEnv, " ")
		if !strings.Contains(env, "PIN_SERVICE=caddy") ||
			!strings.Contains(env, "PIN_OLD_IMAGE=caddy:2.7.6@sha256:old1") ||
			!strings.Contains(env, "PIN_NEW_IMAGE=caddy:") ||
			!strings.Contains(env, "PIN_OLD_TAG=2.7.6") ||
			!strings.Contains(env, "PIN_OLD_DIGEST=sha256:old1") ||
			!strings.Contains(env, "PIN_NEW_TAG=") ||
			!strings.Contains(env, "PIN_NEW_DIGEST=sha256:new1") {
			t.Errorf("on_change env missing PIN_* vars: %v", extraEnv)
		}
		return nil
	}

	if err := scheduleRun(d, sys, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pulled) != 1 || !strings.HasPrefix(pulled[0], "caddy:") {
		t.Errorf("expected only caddy pulled, got %v", pulled)
	}
	if composeUps != 1 || shells != 1 {
		t.Errorf("composeUp=%d shell=%d, want 1 and 1", composeUps, shells)
	}
}

func TestScheduleRun_NoChangeSkipsHooks(t *testing.T) {
	chdirTemp(t, twoServices, "schedule: \"0 6 * * 1\"\non_change: ./hook.sh\n")

	// Digest matches the pinned one, so nothing changes.
	digests := map[string]string{"caddy": "sha256:old1", "cloudflared": "sha256:old2"}
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) {
			return digests[strings.SplitN(ref, ":", 2)[0]], nil
		},
		pull:    func(ref string) error { return nil },
	}
	sys := fakeSys("linux", 1000)
	sys.composeUp = func(file, service string) error { t.Error("composeUp should not run"); return nil }
	sys.shell = func(dir, command string, extraEnv []string) error { t.Error("on_change should not run"); return nil }

	if err := scheduleRun(d, sys, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestScheduleRun_TagConstraint(t *testing.T) {
	dir := chdirTemp(t, twoServices,
		"schedule: \"0 6 * * 1\"\nservices:\n  - name: caddy\n    tags: '^2\\.\\d+\\.\\d+$'\n")

	var pulled []string
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:new1", nil },
		pull:      func(ref string) error { pulled = append(pulled, ref); return nil },
		listMatchingTags: func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error) {
			return []string{"latest", "2.7.6", "2.8.4", "3.0.0", "2.8.4-beta.1"}, nil
		},
	}
	sys := fakeSys("linux", 1000)

	if err := scheduleRun(d, sys, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Newest tag matching the regex, NOT 3.0.0 and NOT the beta.
	if len(pulled) != 1 || pulled[0] != "caddy:2.8.4" {
		t.Errorf("pulled %v, want [caddy:2.8.4]", pulled)
	}
	data, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "caddy:2.8.4@sha256:new1") {
		t.Errorf("compose file not pinned to 2.8.4:\n%s", data)
	}
}

func TestScheduleRun_ExcludeAndDelay(t *testing.T) {
	dir := chdirTemp(t, twoServices,
		"schedule: \"0 6 * * 1\"\nservices:\n"+
			"  - name: caddy\n    tags: '^2\\.\\d+\\.\\d+$'\n    exclude: 'beta'\n    delay: 7d\n")

	now := time.Now()
	created := map[string]time.Time{
		"2.9.0": now.Add(-24 * time.Hour),      // too fresh
		"2.8.4": now.Add(-30 * 24 * time.Hour), // aged
	}
	var pulled []string
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:new1", nil },
		pull:      func(ref string) error { pulled = append(pulled, ref); return nil },
		listMatchingTags: func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error) {
			return []string{"2.9.0", "2.9.0-beta.2", "2.8.4", "2.7.6"}, nil
		},
		tagCreated: func(baseImage, tag string) (time.Time, error) {
			c, ok := created[tag]
			if !ok {
				t.Errorf("unexpected tagCreated(%q) — beta should have been excluded", tag)
			}
			return c, nil
		},
	}
	sys := fakeSys("linux", 1000)

	if err := scheduleRun(d, sys, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 2.9.0 matches but is 1 day old; 2.9.0-beta.2 is excluded; 2.8.4 is aged.
	if len(pulled) != 1 || pulled[0] != "caddy:2.8.4" {
		t.Errorf("pulled %v, want [caddy:2.8.4]", pulled)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if !strings.Contains(string(data), "caddy:2.8.4@sha256:new1") {
		t.Errorf("compose file not pinned to 2.8.4:\n%s", data)
	}
}

func TestScheduleRun_DelayAllTooFresh(t *testing.T) {
	chdirTemp(t, twoServices,
		"schedule: \"0 6 * * 1\"\nservices:\n  - name: caddy\n    tags: '^2\\.\\d+\\.\\d+$'\n    delay: 7d\n")

	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { t.Error("getDigest should not run"); return "", nil },
		pull:      func(ref string) error { t.Error("pull should not run"); return nil },
		listMatchingTags: func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error) {
			return []string{"2.9.0", "2.8.4"}, nil
		},
		tagCreated: func(baseImage, tag string) (time.Time, error) {
			return time.Now().Add(-time.Hour), nil // everything published an hour ago
		},
	}
	sys := fakeSys("linux", 1000)
	sys.composeUp = func(file, service string) error { t.Error("composeUp should not run"); return nil }

	if err := scheduleRun(d, sys, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestScheduleRun_TagConstraintUpToDate(t *testing.T) {
	chdirTemp(t, twoServices,
		"schedule: \"0 6 * * 1\"\nservices:\n  - name: caddy\n    tags: '^2\\.\\d+\\.\\d+$'\n")

	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { t.Error("getDigest should not run"); return "", nil },
		pull:      func(ref string) error { t.Error("pull should not run"); return nil },
		listMatchingTags: func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error) {
			return []string{"2.7.6", "2.6.0"}, nil // current 2.7.6 is already newest
		},
	}
	sys := fakeSys("linux", 1000)
	sys.composeUp = func(file, service string) error { t.Error("composeUp should not run"); return nil }

	if err := scheduleRun(d, sys, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestScheduleRun_ComposeUpFailureRollsBack(t *testing.T) {
	dir := chdirTemp(t, twoServices, "schedule: \"0 6 * * 1\"\nservices:\n  - caddy\non_change: ./hook.sh\n")
	composeFile := filepath.Join(dir, "docker-compose.yml")
	original, _ := os.ReadFile(composeFile)

	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:new1", nil },
		pull:      func(ref string) error { return nil },
	}
	sys := fakeSys("linux", 1000)
	composeUps := 0
	sys.composeUp = func(file, service string) error {
		composeUps++
		if composeUps == 1 {
			return fmt.Errorf("service caddy failed to start")
		}
		// Rollback re-up must see the ORIGINAL file content back in place.
		data, _ := os.ReadFile(file)
		if !strings.Contains(string(data), "caddy:2.7.6@sha256:old1") {
			t.Errorf("rollback re-up ran against non-restored file:\n%s", data)
		}
		return nil
	}
	sys.shell = func(dir, command string, extraEnv []string) error { t.Error("on_change must not run after rollback"); return nil }

	err := scheduleRun(d, sys, false)
	if err == nil || !strings.Contains(err.Error(), "failed to upgrade: caddy") {
		t.Fatalf("want failure mentioning caddy, got %v", err)
	}
	if composeUps != 2 {
		t.Errorf("composeUp ran %d times, want 2 (failed up + rollback up)", composeUps)
	}
	restored, _ := os.ReadFile(composeFile)
	if string(restored) != string(original) {
		t.Errorf("compose file not restored to original:\n%s", restored)
	}
}

func TestScheduleRun_DryRun(t *testing.T) {
	dir := chdirTemp(t, twoServices, "schedule: \"0 6 * * 1\"\nservices:\n  - caddy\non_change: ./hook.sh\n")
	composeFile := filepath.Join(dir, "docker-compose.yml")
	original := mustRead(t, composeFile)

	pulls := 0
	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:new1", nil },
		pull:      func(ref string) error { pulls++; return nil },
	}
	sys := fakeSys("linux", 1000)
	sys.composeUp = func(file, service string) error { t.Error("composeUp must not run in dry run"); return nil }
	sys.shell = func(dir, command string, extraEnv []string) error { t.Error("on_change must not run in dry run"); return nil }

	if err := scheduleRun(d, sys, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pulls != 1 {
		t.Errorf("pulls = %d, want 1 (dry run still pulls to detect the upgrade)", pulls)
	}
	if mustRead(t, composeFile) != original {
		t.Error("dry run modified the compose file")
	}
}

func TestScheduleRun_OneRollbackDoesNotBlockOthers(t *testing.T) {
	dir := chdirTemp(t, twoServices, "schedule: \"0 6 * * 1\"\n")
	composeFile := filepath.Join(dir, "docker-compose.yml")

	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:new-" + strings.SplitN(ref, ":", 2)[0], nil },
		pull:      func(ref string) error { return nil },
	}
	sys := fakeSys("linux", 1000)
	sys.composeUp = func(file, service string) error {
		if service == "caddy" && strings.Contains(mustRead(t, file), "sha256:new-caddy") {
			return fmt.Errorf("caddy failed to start") // fail only the NEW caddy; rollback re-up succeeds
		}
		return nil
	}

	err := scheduleRun(d, sys, false)
	if err == nil || !strings.Contains(err.Error(), "caddy") {
		t.Fatalf("want failure mentioning caddy, got %v", err)
	}
	data := mustRead(t, composeFile)
	if !strings.Contains(data, "caddy:2.7.6@sha256:old1") {
		t.Errorf("caddy not rolled back:\n%s", data)
	}
	if !strings.Contains(data, "sha256:new-cloudflared") {
		t.Errorf("cloudflared should still have upgraded despite caddy's rollback:\n%s", data)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestScheduleRun_OnChangeFailureIsNonFatal(t *testing.T) {
	chdirTemp(t, twoServices, "schedule: \"0 6 * * 1\"\nservices:\n  - caddy\non_change: git push\n")

	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:new1", nil },
		pull:      func(ref string) error { return nil },
	}
	sys := fakeSys("linux", 1000)
	sys.shell = func(dir, command string, extraEnv []string) error { return fmt.Errorf("push rejected") }

	// A failed push must not fail the run: the upgrade is live and committed
	// locally; the next push carries it.
	if err := scheduleRun(d, sys, false); err != nil {
		t.Fatalf("on_change failure should be non-fatal, got %v", err)
	}
}

func TestScheduleRun_CollectsFailures(t *testing.T) {
	chdirTemp(t, twoServices, "schedule: \"0 6 * * 1\"\n")

	d := dockerFuncs{
		getDigest: func(ref string) (string, error) { return "sha256:new", nil },
		pull: func(ref string) error {
			if strings.HasPrefix(ref, "caddy:") {
				return fmt.Errorf("boom")
			}
			return nil
		},
	}
	sys := fakeSys("linux", 1000)
	composeUps := 0
	sys.composeUp = func(file, service string) error { composeUps++; return nil }

	err := scheduleRun(d, sys, false)
	if err == nil || !strings.Contains(err.Error(), "caddy") {
		t.Fatalf("want failure mentioning caddy, got %v", err)
	}
	// cloudflared still upgraded, so the compose file changed and up -d ran.
	if composeUps != 1 {
		t.Errorf("composeUp=%d, want 1 despite a failed service", composeUps)
	}
}
