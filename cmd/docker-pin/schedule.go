package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/croncal"
	"github.com/Miista/homebrew-docker-pin/internal/notify"
	"github.com/Miista/homebrew-docker-pin/internal/registry"
	"github.com/Miista/homebrew-docker-pin/internal/schedule"
)

const unitDir = "/etc/systemd/system"

// sysFuncs seams out everything OS-specific so the schedule commands can be
// tested on any platform, mirroring the dockerFuncs pattern.
type sysFuncs struct {
	goos     string
	euid     func() int
	execPath func() (string, error)
	// systemctl runs `systemctl args...` and returns combined output.
	systemctl func(args ...string) (string, error)
	// analyze runs `systemd-analyze args...` and returns combined output.
	analyze func(args ...string) (string, error)
	// shell runs `sh -c command` in dir with extraEnv appended to the
	// environment, streaming output to the caller.
	shell func(dir, command string, extraEnv []string) error
	// composeUp runs `docker compose -f file up -d service`, streaming output.
	composeUp func(file, service string) error
}

var realSys = sysFuncs{
	goos:     runtime.GOOS,
	euid:     os.Geteuid,
	execPath: os.Executable,
	systemctl: func(args ...string) (string, error) {
		out, err := exec.Command("systemctl", args...).CombinedOutput()
		return string(out), err
	},
	analyze: func(args ...string) (string, error) {
		out, err := exec.Command("systemd-analyze", args...).CombinedOutput()
		return string(out), err
	},
	shell: func(dir, command string, extraEnv []string) error {
		cmd := exec.Command("sh", "-c", command)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), extraEnv...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	},
	composeUp: func(file, service string) error {
		cmd := exec.Command("docker", "compose", "-f", file, "up", "-d", service)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	},
}

func runSchedule(args []string, d dockerFuncs, sys sysFuncs) error {
	usage := "Usage: docker pin schedule <apply|status|remove|run [--dry-run]>"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	switch args[0] {
	case "apply":
		return scheduleApply(sys)
	case "status":
		return scheduleStatus(sys)
	case "remove":
		return scheduleRemove(sys)
	case "run":
		dryRun := false
		for _, a := range args[1:] {
			switch a {
			case "--dry-run", "-n":
				dryRun = true
			default:
				fmt.Fprintln(os.Stderr, "Usage: docker pin schedule run [--dry-run]")
				os.Exit(1)
			}
		}
		return scheduleRun(d, sys, dryRun)
	default:
		fmt.Fprintf(os.Stderr, "Unknown schedule command: %s\n", args[0])
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
		return nil
	}
}

// loadScheduleConfig locates the compose file from the working directory,
// then the pin.yaml next to it, and parses it.
func loadScheduleConfig() (cfg *schedule.Config, composeFile, pinFile string, err error) {
	composeFile, err = compose.Locate()
	if err != nil {
		return nil, "", "", err
	}
	pinFile, err = schedule.FindFile(composeFile)
	if err != nil {
		return nil, "", "", err
	}
	cfg, err = schedule.Load(pinFile)
	if err != nil {
		return nil, "", "", err
	}
	return cfg, composeFile, pinFile, nil
}

// validateSchedule checks the config against the compose file: the cron
// expression must translate, and every listed service must exist. A missing
// on_change script only warns — it may be a command on PATH.
func validateSchedule(cfg *schedule.Config, composeFile string) error {
	if _, err := croncal.Translate(cfg.Schedule); err != nil {
		return err
	}
	services, err := compose.ListServices(composeFile)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, s := range services {
		known[s] = true
	}
	for _, s := range cfg.Services {
		if !known[s.Name] {
			return fmt.Errorf("service %q in pin.yaml not found in %s", s.Name, composeFile)
		}
	}
	if cfg.OnChange != "" {
		script := strings.Fields(cfg.OnChange)[0]
		if strings.Contains(script, "/") {
			p := script
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(composeFile), p)
			}
			if _, err := os.Stat(p); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: on_change script %s not found (may be created later)\n", script)
			}
		}
	}
	return nil
}

func requireSystemd(sys sysFuncs) error {
	if sys.goos != "linux" {
		return fmt.Errorf("schedule requires systemd (Linux)")
	}
	return nil
}

// apply

func scheduleApply(sys sysFuncs) error {
	if err := requireSystemd(sys); err != nil {
		return err
	}
	if sys.euid() != 0 {
		return fmt.Errorf("writing to %s requires root; re-run with sudo", unitDir)
	}
	cfg, composeFile, pinFile, err := loadScheduleConfig()
	if err != nil {
		return err
	}
	if err := validateSchedule(cfg, composeFile); err != nil {
		return err
	}

	composeDir := filepath.Dir(composeFile)
	bin, err := sys.execPath()
	if err != nil {
		return err
	}
	svcContent, tmrContent, err := schedule.Units(cfg, composeDir, bin, invokingUser())
	if err != nil {
		return err
	}
	svcName, tmrName := schedule.UnitNames(composeDir)
	svcPath := filepath.Join(unitDir, svcName)
	tmrPath := filepath.Join(unitDir, tmrName)

	if unitFileMatches(svcPath, svcContent) && unitFileMatches(tmrPath, tmrContent) {
		fmt.Printf("%s: already up to date (%s, %s)\n", pinFile, svcName, tmrName)
		return nil
	}

	if err := os.WriteFile(svcPath, []byte(svcContent), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(tmrPath, []byte(tmrContent), 0o644); err != nil {
		return err
	}
	if out, err := sys.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v\n%s", err, out)
	}
	if out, err := sys.systemctl("enable", "--now", tmrName); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %v\n%s", tmrName, err, out)
	}
	fmt.Printf("Installed %s and %s (schedule %q)\n", svcName, tmrName, cfg.Schedule)
	return nil
}

func unitFileMatches(path, content string) bool {
	data, err := os.ReadFile(path)
	return err == nil && bytes.Equal(data, []byte(content))
}

// invokingUser returns SUDO_USER if set, else the current user.
func invokingUser() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "root"
}

// status

func scheduleStatus(sys sysFuncs) error {
	cfg, composeFile, pinFile, err := loadScheduleConfig()
	if err != nil {
		return err
	}
	onCal, err := croncal.Translate(cfg.Schedule)
	if err != nil {
		return err
	}

	fmt.Printf("Config:        %s\n", pinFile)
	fmt.Printf("Schedule:      %s  (systemd OnCalendar: %s)\n", cfg.Schedule, onCal)
	if cfg.OnChange != "" {
		fmt.Printf("On change:     %s\n", cfg.OnChange)
	}
	if cfg.Notify != nil && cfg.Notify.Ntfy != nil {
		fmt.Printf("Notify:        ntfy %s, topic %q\n", cfg.Notify.Ntfy.URL, cfg.Notify.Ntfy.Topic)
	}

	fmt.Println("\nServices:")
	if len(cfg.Services) == 0 {
		fmt.Println("  (all services in the compose file, unconstrained)")
	} else {
		w := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
		fmt.Fprintln(w, "  SERVICE\tTAGS\tEXCLUDE\tDELAY")
		for _, s := range cfg.Services {
			dash := func(v string) string {
				if v == "" {
					return "-"
				}
				return v
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", s.Name, dash(s.Tags), dash(s.Exclude), dash(s.Delay))
		}
		w.Flush()
	}
	fmt.Println()

	if err := requireSystemd(sys); err != nil {
		fmt.Printf("Systemd timer: not checked — %v\n", err)
		return nil
	}

	composeDir := filepath.Dir(composeFile)
	svcName, tmrName := schedule.UnitNames(composeDir)
	svcPath := filepath.Join(unitDir, svcName)
	tmrPath := filepath.Join(unitDir, tmrName)

	_, svcErr := os.Stat(svcPath)
	_, tmrErr := os.Stat(tmrPath)
	if svcErr != nil || tmrErr != nil {
		fmt.Printf("Systemd timer: NOT INSTALLED — nothing runs on a schedule yet; run `sudo docker pin schedule apply`\n")
		return nil
	}

	bin, err := sys.execPath()
	if err != nil {
		return err
	}
	svcContent, tmrContent, err := schedule.Units(cfg, composeDir, bin, invokingUser())
	if err != nil {
		return err
	}
	if unitFileMatches(svcPath, svcContent) && unitFileMatches(tmrPath, tmrContent) {
		fmt.Printf("Systemd timer: installed and in sync with pin.yaml (%s)\n", tmrName)
	} else {
		fmt.Printf("Systemd timer: installed but OUT OF SYNC with pin.yaml — run `sudo docker pin schedule apply`\n")
	}

	if out, err := sys.analyze("calendar", onCal); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Next elapse") {
				fmt.Printf("Next run:      %s\n", strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
			}
		}
	} else if out, err := sys.systemctl("list-timers", "--no-pager", tmrName); err == nil {
		fmt.Println(out)
	}
	return nil
}

// remove

func scheduleRemove(sys sysFuncs) error {
	if err := requireSystemd(sys); err != nil {
		return err
	}
	if sys.euid() != 0 {
		return fmt.Errorf("removing units from %s requires root; re-run with sudo", unitDir)
	}
	composeFile, err := compose.Locate()
	if err != nil {
		return err
	}
	composeDir := filepath.Dir(composeFile)
	svcName, tmrName := schedule.UnitNames(composeDir)
	svcPath := filepath.Join(unitDir, svcName)
	tmrPath := filepath.Join(unitDir, tmrName)

	if _, err := os.Stat(tmrPath); err != nil {
		if _, err := os.Stat(svcPath); err != nil {
			fmt.Printf("Nothing to remove: %s and %s are not installed\n", svcName, tmrName)
			return nil
		}
	}

	if out, err := sys.systemctl("disable", "--now", tmrName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: systemctl disable --now %s: %v\n%s", tmrName, err, out)
	}
	for _, p := range []string{tmrPath, svcPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if out, err := sys.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v\n%s", err, out)
	}
	fmt.Printf("Removed %s and %s (pin.yaml left untouched)\n", svcName, tmrName)
	return nil
}

// run

func scheduleRun(d dockerFuncs, sys sysFuncs, dryRun bool) error {
	cfg, composeFile, _, err := loadScheduleConfig()
	if err != nil {
		return err
	}
	if err := validateSchedule(cfg, composeFile); err != nil {
		return err
	}
	if dryRun {
		fmt.Println("Dry run: discovering upgrades; nothing will be changed.")
	}

	services := cfg.Services
	if len(services) == 0 {
		names, err := compose.ListServices(composeFile)
		if err != nil {
			return err
		}
		for _, n := range names {
			services = append(services, schedule.Service{Name: n})
		}
	}

	// Each service is its own transaction: upgrade the pin, up -d just that
	// service, run on_change, notify — independently. One failing service is
	// rolled back alone and never blocks or reverts the others, and each
	// upgrade lands as its own on_change invocation (= its own commit).
	var failed []string
	verdicts := make([]string, 0, len(services))
	width := 0
	for _, s := range services {
		if len(s.Name) > width {
			width = len(s.Name)
		}
	}
	for _, service := range services {
		verdict, err := upgradeServiceTxn(cfg, composeFile, service, d, sys, dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error upgrading %s: %v\n", service.Name, err)
			failed = append(failed, service.Name)
			if verdict == "" {
				verdict = "FAILED — " + err.Error()
			}
		}
		verdicts = append(verdicts, fmt.Sprintf("  %-*s  %s", width, service.Name, verdict))
	}

	fmt.Printf("\nSummary%s:\n%s\n", map[bool]string{true: " (dry run — nothing changed)", false: ""}[dryRun], strings.Join(verdicts, "\n"))
	if len(failed) > 0 {
		return fmt.Errorf("failed to upgrade: %s", strings.Join(failed, ", "))
	}
	return nil
}

// upgradeServiceTxn upgrades one service end to end: pin rewrite, compose up
// of that service only, on_change hook, notification. The pin itself is
// read/written in whichever file actually declares the service (which may
// differ from rootFile via include:), but compose up and on_change always
// run against rootFile — the whole project, not just the declaring file, is
// needed to bring the service up in the right project/network. On a failed
// compose up the service's previous pin is restored and re-asserted, leaving
// the system exactly as before; the upgrade retries on the next scheduled
// run. The returned verdict is a one-line human summary of what happened.
func upgradeServiceTxn(cfg *schedule.Config, rootFile string, service schedule.Service, d dockerFuncs, sys sysFuncs, dryRun bool) (verdict string, err error) {
	serviceFile, err := compose.ResolveServiceIn(rootFile, service.Name)
	if err != nil {
		return "", err
	}

	target := ""
	if service.Tags != "" {
		var hold string
		target, hold, err = constrainedTarget(serviceFile, service, d)
		if err != nil {
			if !dryRun {
				notifyFailed(cfg, service.Name, "", "", err.Error())
			}
			return "", err
		}
		if target == "" {
			fmt.Printf("%s: %s; leaving as is\n", service.Name, hold)
			return "up to date — " + hold, nil
		}
		fmt.Printf("%s: newest tag matching %s is %s\n", service.Name, service.Tags, target)
	}

	if dryRun {
		// Full discovery (including the pull, which digest comparison needs),
		// no side effects beyond the local image cache.
		outcome, err := upgradeInFile(serviceFile, service.Name, target, d, true)
		switch {
		case err != nil:
			return "", err
		case outcome.Changed:
			oldTag, _ := tagAndDigest(outcome.OldRaw)
			newTag, _ := tagAndDigest(outcome.NewRaw)
			return fmt.Sprintf("WOULD UPGRADE  %s -> %s", oldTag, newTag), nil
		default:
			return "up to date — pinned digest is still current", nil
		}
	}

	before, err := os.ReadFile(serviceFile)
	if err != nil {
		return "", err
	}

	outcome, err := upgradeInFile(serviceFile, service.Name, target, d, false)
	if err != nil {
		notifyFailed(cfg, service.Name, "", "", err.Error())
		return "", err
	}
	if !outcome.Changed {
		return "up to date — pinned digest is still current", nil
	}
	oldRaw, newRaw := outcome.OldRaw, outcome.NewRaw
	oldTag, _ := tagAndDigest(oldRaw)
	newTag, _ := tagAndDigest(newRaw)

	fmt.Printf("Running docker compose up -d %s ...\n", service.Name)
	if err := sys.composeUp(rootFile, service.Name); err != nil {
		fmt.Fprintf(os.Stderr, "compose up %s failed: %v\nRolling back to %s ...\n", service.Name, err, oldRaw)
		if werr := os.WriteFile(serviceFile, before, 0o644); werr != nil {
			notifyFailed(cfg, service.Name, oldRaw, newRaw, "compose up failed AND rollback write failed — manual intervention needed")
			return "FAILED — compose up failed and rollback write failed", fmt.Errorf("compose up failed (%v) and rollback write failed: %w", err, werr)
		}
		if rerr := sys.composeUp(rootFile, service.Name); rerr != nil {
			notifyFailed(cfg, service.Name, oldRaw, newRaw, "compose up failed; rollback compose up ALSO failed — container may be down")
			return "FAILED — compose up failed; rollback also failed", fmt.Errorf("compose up failed (%v); rollback compose up also failed: %w", err, rerr)
		}
		notifyFailed(cfg, service.Name, oldRaw, newRaw, "compose up failed; rolled back to previous pin (retrying next run)")
		return fmt.Sprintf("FAILED — %s did not come up; rolled back to %s", newTag, oldTag), fmt.Errorf("compose up failed, rolled back: %w", err)
	}

	note := ""
	if cfg.OnChange != "" {
		fmt.Printf("Running on_change for %s: %s\n", service.Name, cfg.OnChange)
		_, oldDigest := tagAndDigest(oldRaw)
		_, newDigest := tagAndDigest(newRaw)
		env := []string{
			"PIN_SERVICE=" + service.Name,
			"PIN_OLD_IMAGE=" + oldRaw,
			"PIN_NEW_IMAGE=" + newRaw,
			"PIN_OLD_TAG=" + oldTag,
			"PIN_OLD_DIGEST=" + oldDigest,
			"PIN_NEW_TAG=" + newTag,
			"PIN_NEW_DIGEST=" + newDigest,
		}
		if err := sys.shell(filepath.Dir(rootFile), cfg.OnChange, env); err != nil {
			// Non-fatal: the upgrade is live. A failed commit/push just means
			// the change stays local until the next push carries it.
			fmt.Fprintf(os.Stderr, "Warning: on_change for %s failed: %v (change remains local)\n", service.Name, err)
			note = "note: on_change failed — change not pushed yet"
		}
	}
	notifyUpgraded(cfg, service.Name, oldRaw, newRaw, note)
	verdict = fmt.Sprintf("UPGRADED  %s -> %s", oldTag, newTag)
	if note != "" {
		verdict += " (not pushed yet)"
	}
	return verdict, nil
}

// tagAndDigest splits a raw image reference ("base:tag@sha256:...") into its
// tag and digest, either of which may be empty.
func tagAndDigest(raw string) (tag, digest string) {
	ref := raw
	if i := strings.Index(ref, "@"); i != -1 {
		digest = ref[i+1:]
		ref = ref[:i]
	}
	// The tag is after the last ":" that follows the last "/" (so a registry
	// host port never masquerades as a tag).
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		tag = ref[colon+1:]
	}
	return tag, digest
}

// maxDelayChecks bounds how many candidate publish dates one service may
// query per run when walking past too-fresh releases.
const maxDelayChecks = 10

// constrainedTarget picks the upgrade target for a service with a tags regex:
// the newest registry tag matching the regex (and not matching the optional
// exclude) that is newer than the current tag — and, with a delay configured,
// the newest such tag that has been published for at least that long. Returns
// "" when nothing qualifies, with hold explaining why in one human-readable
// clause. Unlike the unconstrained flow it never falls back to a moving tag,
// so the constraint can't be silently escaped.
func constrainedTarget(composeFile string, service schedule.Service, d dockerFuncs) (target, hold string, err error) {
	baseImage, currentTag, err := compose.ParseImage(composeFile, service.Name)
	if err != nil {
		return "", "", err
	}
	include, err := regexp.Compile(service.Tags)
	if err != nil {
		return "", "", err
	}
	var exclude *regexp.Regexp
	if service.Exclude != "" {
		if exclude, err = regexp.Compile(service.Exclude); err != nil {
			return "", "", err
		}
	}
	tags, err := d.listMatchingTags(baseImage, include, exclude, currentTag)
	if err != nil {
		return "", "", fmt.Errorf("listing tags for %s: %w", baseImage, err)
	}
	candidates := registry.MatchingCandidates(tags, include, exclude, currentTag)
	if len(candidates) == 0 {
		return "", fmt.Sprintf("no tag newer than %s matches %s", currentTag, service.Tags), nil
	}
	if service.Delay == "" {
		return candidates[0], "", nil
	}

	delay, err := schedule.ParseDelay(service.Delay)
	if err != nil {
		return "", "", err
	}
	if len(candidates) > maxDelayChecks {
		candidates = candidates[:maxDelayChecks]
	}
	for _, tag := range candidates {
		created, err := d.tagCreated(baseImage, tag)
		if err != nil {
			return "", "", fmt.Errorf("publish date for %s:%s: %w", baseImage, tag, err)
		}
		age := time.Since(created)
		if age >= delay {
			return tag, "", nil
		}
		fmt.Printf("%s: %s is only %s old (delay %s); skipping\n",
			service.Name, tag, age.Round(time.Hour), service.Delay)
	}
	return "", fmt.Sprintf("%d newer tag(s) match %s but none is %s old yet", len(candidates), service.Tags, service.Delay), nil
}

// notifyUpgraded announces one service's successful upgrade via ntfy; note
// (e.g. a failed push) is appended without raising the priority.
func notifyUpgraded(cfg *schedule.Config, name, oldRaw, newRaw, note string) {
	body := fmt.Sprintf("%s -> %s", oldRaw, newRaw)
	if note != "" {
		body += "\n" + note
	}
	sendNotification(cfg, fmt.Sprintf("docker pin@%s: %s upgraded", hostLabel(cfg), name), body, notify.PriorityDefault)
}

// notifyFailed reports one service's failed upgrade at high priority.
func notifyFailed(cfg *schedule.Config, name, oldRaw, newRaw, reason string) {
	body := reason
	if oldRaw != "" {
		body = fmt.Sprintf("%s -> %s\n%s", oldRaw, newRaw, reason)
	}
	sendNotification(cfg, fmt.Sprintf("docker pin@%s: %s FAILED", hostLabel(cfg), name), body, notify.PriorityHigh)
}

// hostLabel identifies this box in notifications, so several hosts can share
// one ntfy topic: pin.yaml's `hostname:` when set (the box name may differ
// from the OS hostname), otherwise the short OS hostname.
func hostLabel(cfg *schedule.Config) string {
	if cfg.Hostname != "" {
		return cfg.Hostname
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return strings.SplitN(h, ".", 2)[0]
}

// sendNotification publishes to the configured ntfy target, if any.
// Notification failures only warn: losing a message must not fail a run.
func sendNotification(cfg *schedule.Config, title, body string, priority int) {
	if cfg.Notify == nil || cfg.Notify.Ntfy == nil {
		return
	}
	token, err := cfg.Notify.Ntfy.Token()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		return
	}
	n := notify.Ntfy{URL: cfg.Notify.Ntfy.URL, Topic: cfg.Notify.Ntfy.Topic, Token: token}
	if err := n.Send(title, body, priority); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", err)
	}
}
