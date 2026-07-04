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
	// shell runs `sh -c command` in dir, streaming output to the caller.
	shell func(dir, command string) error
	// composeUp runs `docker compose -f file up -d`, streaming output.
	composeUp func(file string) error
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
	shell: func(dir, command string) error {
		cmd := exec.Command("sh", "-c", command)
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	},
	composeUp: func(file string) error {
		cmd := exec.Command("docker", "compose", "-f", file, "up", "-d")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	},
}

func runSchedule(args []string, d dockerFuncs, sys sysFuncs) error {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: docker pin schedule <apply|status|remove|run>")
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
		return scheduleRun(d, sys)
	default:
		fmt.Fprintf(os.Stderr, "Unknown schedule command: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "Usage: docker pin schedule <apply|status|remove|run>")
		os.Exit(1)
		return nil
	}
}

// loadScheduleConfig locates the compose file from the working directory,
// then the pin.yaml next to it, and parses it.
func loadScheduleConfig() (cfg *schedule.Config, composeFile, pinFile string, err error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, "", "", err
	}
	composeFile, err = compose.FindFile(wd)
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

	fmt.Printf("Config:      %s\n", pinFile)
	fmt.Printf("Schedule:    %s  (OnCalendar=%s)\n", cfg.Schedule, onCal)
	if len(cfg.Services) == 0 {
		fmt.Println("Services:    all services in the compose file")
	} else {
		var parts []string
		for _, s := range cfg.Services {
			var opts []string
			if s.Tags != "" {
				opts = append(opts, "tags "+s.Tags)
			}
			if s.Exclude != "" {
				opts = append(opts, "exclude "+s.Exclude)
			}
			if s.Delay != "" {
				opts = append(opts, "delay "+s.Delay)
			}
			if len(opts) > 0 {
				parts = append(parts, fmt.Sprintf("%s (%s)", s.Name, strings.Join(opts, ", ")))
			} else {
				parts = append(parts, s.Name)
			}
		}
		fmt.Printf("Services:    %s\n", strings.Join(parts, ", "))
	}
	if cfg.OnChange != "" {
		fmt.Printf("On change:   %s\n", cfg.OnChange)
	}
	if cfg.Notify != nil && cfg.Notify.Ntfy != nil {
		fmt.Printf("Notify:      ntfy %s topic %s\n", cfg.Notify.Ntfy.URL, cfg.Notify.Ntfy.Topic)
	}

	if err := requireSystemd(sys); err != nil {
		fmt.Printf("Units:       not checked — %v\n", err)
		return nil
	}

	composeDir := filepath.Dir(composeFile)
	svcName, tmrName := schedule.UnitNames(composeDir)
	svcPath := filepath.Join(unitDir, svcName)
	tmrPath := filepath.Join(unitDir, tmrName)

	_, svcErr := os.Stat(svcPath)
	_, tmrErr := os.Stat(tmrPath)
	if svcErr != nil || tmrErr != nil {
		fmt.Printf("Units:       not installed (run `sudo docker pin schedule apply`)\n")
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
		fmt.Printf("Units:       installed, in sync (%s)\n", tmrName)
	} else {
		fmt.Printf("Units:       installed, but DRIFTED from pin.yaml — run `sudo docker pin schedule apply`\n")
	}

	if out, err := sys.analyze("calendar", onCal); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Next elapse") {
				fmt.Printf("Next run:    %s\n", strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
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
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	composeFile, err := compose.FindFile(wd)
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

func scheduleRun(d dockerFuncs, sys sysFuncs) error {
	cfg, composeFile, _, err := loadScheduleConfig()
	if err != nil {
		return err
	}
	if err := validateSchedule(cfg, composeFile); err != nil {
		return err
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

	before, err := os.ReadFile(composeFile)
	if err != nil {
		return err
	}

	var failed, upgraded []string
	for _, service := range services {
		target := ""
		if service.Tags != "" {
			target, err = constrainedTarget(composeFile, service, d)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error upgrading %s: %v\n", service.Name, err)
				failed = append(failed, service.Name)
				continue
			}
			if target == "" {
				fmt.Printf("%s: no newer tag matching %s; leaving as is\n", service.Name, service.Tags)
				continue
			}
		}
		oldRaw, _ := compose.RawImage(composeFile, service.Name)
		if err := upgradeInFile(composeFile, service.Name, target, d); err != nil {
			fmt.Fprintf(os.Stderr, "Error upgrading %s: %v\n", service.Name, err)
			failed = append(failed, service.Name)
			continue
		}
		if newRaw, _ := compose.RawImage(composeFile, service.Name); newRaw != oldRaw {
			upgraded = append(upgraded, fmt.Sprintf("%s: %s -> %s", service.Name, oldRaw, newRaw))
		}
	}

	after, err := os.ReadFile(composeFile)
	if err != nil {
		return err
	}

	if !bytes.Equal(before, after) {
		fmt.Printf("Compose file changed, running docker compose up -d ...\n")
		if err := sys.composeUp(composeFile); err != nil {
			notifyRun(cfg, upgraded, append(failed, "docker compose up -d failed"))
			return fmt.Errorf("docker compose up -d failed: %w", err)
		}
		if cfg.OnChange != "" {
			fmt.Printf("Running on_change: %s\n", cfg.OnChange)
			if err := sys.shell(filepath.Dir(composeFile), cfg.OnChange); err != nil {
				notifyRun(cfg, upgraded, append(failed, "on_change command failed"))
				return fmt.Errorf("on_change command failed: %w", err)
			}
		}
	} else {
		fmt.Println("All services already up to date; compose file unchanged.")
	}

	notifyRun(cfg, upgraded, failed)
	if len(failed) > 0 {
		return fmt.Errorf("failed to upgrade: %s", strings.Join(failed, ", "))
	}
	return nil
}

// maxDelayChecks bounds how many candidate publish dates one service may
// query per run when walking past too-fresh releases.
const maxDelayChecks = 10

// constrainedTarget picks the upgrade target for a service with a tags regex:
// the newest registry tag matching the regex (and not matching the optional
// exclude) that is newer than the current tag — and, with a delay configured,
// the newest such tag that has been published for at least that long. Returns
// "" when nothing qualifies. Unlike the unconstrained flow it never falls
// back to a moving tag, so the constraint can't be silently escaped.
func constrainedTarget(composeFile string, service schedule.Service, d dockerFuncs) (string, error) {
	baseImage, currentTag, err := compose.ParseImage(composeFile, service.Name)
	if err != nil {
		return "", err
	}
	include, err := regexp.Compile(service.Tags)
	if err != nil {
		return "", err
	}
	var exclude *regexp.Regexp
	if service.Exclude != "" {
		if exclude, err = regexp.Compile(service.Exclude); err != nil {
			return "", err
		}
	}
	tags, err := d.listTags(baseImage)
	if err != nil {
		return "", fmt.Errorf("listing tags for %s: %w", baseImage, err)
	}
	candidates := registry.MatchingCandidates(tags, include, exclude, currentTag)
	if len(candidates) == 0 {
		return "", nil
	}
	if service.Delay == "" {
		return candidates[0], nil
	}

	delay, err := schedule.ParseDelay(service.Delay)
	if err != nil {
		return "", err
	}
	if len(candidates) > maxDelayChecks {
		candidates = candidates[:maxDelayChecks]
	}
	for _, tag := range candidates {
		created, err := d.tagCreated(baseImage, tag)
		if err != nil {
			return "", fmt.Errorf("publish date for %s:%s: %w", baseImage, tag, err)
		}
		age := time.Since(created)
		if age >= delay {
			return tag, nil
		}
		fmt.Printf("%s: %s is only %s old (delay %s); skipping\n",
			service.Name, tag, age.Round(time.Hour), service.Delay)
	}
	return "", nil
}

// notifyRun reports the outcome of a schedule run via the configured ntfy
// target. Notification failures only warn: losing a message must not turn a
// successful upgrade run into a failed one.
func notifyRun(cfg *schedule.Config, upgraded, failed []string) {
	if cfg.Notify == nil || cfg.Notify.Ntfy == nil {
		return
	}
	if len(upgraded) == 0 && len(failed) == 0 {
		return
	}
	token, err := cfg.Notify.Ntfy.Token()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		return
	}
	n := notify.Ntfy{URL: cfg.Notify.Ntfy.URL, Topic: cfg.Notify.Ntfy.Topic, Token: token}

	title := fmt.Sprintf("docker pin: %d service(s) upgraded", len(upgraded))
	priority := notify.PriorityDefault
	var body []string
	body = append(body, upgraded...)
	if len(failed) > 0 {
		title = fmt.Sprintf("docker pin: %d upgraded, %d FAILED", len(upgraded), len(failed))
		priority = notify.PriorityHigh
		body = append(body, "failed: "+strings.Join(failed, ", "))
	}
	if err := n.Send(title, strings.Join(body, "\n"), priority); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", err)
	}
}
