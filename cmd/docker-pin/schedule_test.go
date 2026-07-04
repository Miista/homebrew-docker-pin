package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// noResolve is a resolve fake that keeps the pulled tag, avoiding network.
func noResolve(baseImage, pullTag, digest, service string) string { return pullTag }

func fakeSys(goos string, euid int) sysFuncs {
	return sysFuncs{
		goos:      goos,
		euid:      func() int { return euid },
		execPath:  func() (string, error) { return "/usr/bin/docker-pin", nil },
		systemctl: func(args ...string) (string, error) { return "", nil },
		analyze:   func(args ...string) (string, error) { return "", nil },
		shell:     func(dir, command string) error { return nil },
		composeUp: func(file string) error { return nil },
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
		{"ok listed", schedule.Config{Schedule: "0 6 * * 1", Services: []string{"caddy"}}, ""},
		{"unknown service", schedule.Config{Schedule: "0 6 * * 1", Services: []string{"nope"}}, `service "nope"`},
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
		resolve:   noResolve,
	}
	composeUps, shells := 0, 0
	sys := fakeSys("linux", 1000)
	sys.composeUp = func(file string) error {
		composeUps++
		if file != filepath.Join(dir, "docker-compose.yml") {
			t.Errorf("composeUp got %q", file)
		}
		return nil
	}
	sys.shell = func(shellDir, command string) error {
		shells++
		if shellDir != dir || command != "./hook.sh" {
			t.Errorf("shell got dir=%q command=%q", shellDir, command)
		}
		return nil
	}

	if err := scheduleRun(d, sys); err != nil {
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
		resolve: noResolve,
	}
	sys := fakeSys("linux", 1000)
	sys.composeUp = func(file string) error { t.Error("composeUp should not run"); return nil }
	sys.shell = func(dir, command string) error { t.Error("on_change should not run"); return nil }

	if err := scheduleRun(d, sys); err != nil {
		t.Fatalf("unexpected error: %v", err)
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
		resolve: noResolve,
	}
	sys := fakeSys("linux", 1000)
	composeUps := 0
	sys.composeUp = func(file string) error { composeUps++; return nil }

	err := scheduleRun(d, sys)
	if err == nil || !strings.Contains(err.Error(), "caddy") {
		t.Fatalf("want failure mentioning caddy, got %v", err)
	}
	// cloudflared still upgraded, so the compose file changed and up -d ran.
	if composeUps != 1 {
		t.Errorf("composeUp=%d, want 1 despite a failed service", composeUps)
	}
}
