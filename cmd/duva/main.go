// duva checks whether a newer registry tag exists for each configured
// compose service and sends an ntfy notification the first time a given
// newer tag is seen — it never rewrites the compose file, pulls an image,
// or restarts a container. State (the last tag notified about, per service)
// persists to a small JSON file so the same tag is never reported twice.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/croncal"
	"github.com/Miista/homebrew-docker-pin/internal/notify"
	"github.com/Miista/homebrew-docker-pin/internal/registry"
	"github.com/Miista/homebrew-docker-pin/internal/schedule"
)

var version = "dev"

// regFuncs seams out registry access so run/serve can be tested without
// network calls, mirroring docker-pin's dockerFuncs pattern.
type regFuncs struct {
	listMatchingTags func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error)
	remoteDigest     func(baseImage, tag string) (string, error)
}

var realReg = regFuncs{listMatchingTags: registry.ListMatchingTags, remoteDigest: registry.RemoteDigest}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("duva", version)
	case "run":
		if err := runOnce(realReg, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		if err := serve(realReg, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: duva <run|serve|version>")
}

// configPath is where duva reads its config: a fixed path inside the
// container, mounted by the user (-v ./duva/config.yaml:/config.yaml:ro).
// Overridable via DUVA_CONFIG only so tests can point at fixtures.
func configPath() string {
	if p := os.Getenv("DUVA_CONFIG"); p != "" {
		return p
	}
	return "/config.yaml"
}

// loadConfig locates the compose file from the working directory and parses
// duva's config from the fixed container path.
func loadConfig() (cfg *schedule.Config, composeFile string, err error) {
	composeFile, err = compose.Locate()
	if err != nil {
		return nil, "", err
	}
	cfg, err = schedule.Load(configPath())
	if err != nil {
		return nil, "", err
	}
	return cfg, composeFile, nil
}

// runOnce checks every configured service once and reports what it found.
func runOnce(reg regFuncs, out io.Writer) error {
	cfg, composeFile, err := loadConfig()
	if err != nil {
		return err
	}

	statePath := stateFilePath()
	st, err := loadState(statePath)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
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

	changed := false
	for _, svc := range services {
		before := st[svc.Name]
		candidate, err := checkService(composeFile, svc, reg, st)
		if err != nil {
			fmt.Fprintf(out, "%s: error: %v\n", svc.Name, err)
			continue
		}
		if st[svc.Name] != before {
			changed = true // checkService recorded a moving-tag baseline
		}
		if candidate == "" {
			fmt.Fprintf(out, "%s: up to date\n", svc.Name)
			continue
		}
		if st[svc.Name] == candidate {
			fmt.Fprintf(out, "%s: %s available (already notified)\n", svc.Name, candidate)
			continue
		}
		fmt.Fprintf(out, "%s: %s available\n", svc.Name, candidate)
		notifyAvailable(cfg, svc.Name, candidate)
		st[svc.Name] = candidate
		changed = true
	}

	if changed {
		if err := saveState(statePath, st); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}
	}
	return nil
}

// checkService returns what's newly available for svc: for a service with a
// tags constraint, the newest matching registry tag newer than the one
// currently pinned; for an unconstrained service (moving tag such as
// "latest"), the remote manifest digest once it differs from st[svc.Name].
// An empty result means nothing new to report. For a moving-tag service,
// checkService may update st[svc.Name] itself even when it returns no
// candidate: the very first check has no baseline to compare the digest
// against, so it records one silently rather than notifying (otherwise
// every newly-watched service would fire once on day one regardless of
// whether anything actually changed).
func checkService(rootFile string, svc schedule.Service, reg regFuncs, st map[string]string) (string, error) {
	serviceFile, err := compose.ResolveServiceIn(rootFile, svc.Name)
	if err != nil {
		return "", err
	}
	baseImage, currentTag, err := compose.ParseImage(serviceFile, svc.Name)
	if err != nil {
		return "", err
	}

	if svc.Tags == "" {
		return checkMovingTag(baseImage, currentTag, reg, svc.Name, st)
	}

	include, exclude, err := compileTagFilters(svc)
	if err != nil {
		return "", err
	}
	tags, err := reg.listMatchingTags(baseImage, include, exclude, currentTag)
	if err != nil {
		return "", fmt.Errorf("listing tags for %s: %w", baseImage, err)
	}
	candidates := registry.MatchingCandidates(tags, include, exclude, currentTag)
	if len(candidates) == 0 {
		return "", nil
	}
	if svc.Delay == "" {
		return candidates[0], nil
	}

	delay, err := schedule.ParseDelay(svc.Delay)
	if err != nil {
		return "", err
	}
	if len(candidates) > maxDelayChecks {
		candidates = candidates[:maxDelayChecks]
	}
	for _, tag := range candidates {
		created, err := registry.TagCreated(baseImage, tag)
		if err != nil {
			return "", fmt.Errorf("publish date for %s:%s: %w", baseImage, tag, err)
		}
		if time.Since(created) >= delay {
			return tag, nil
		}
	}
	return "", nil
}

// checkMovingTag fetches the remote manifest digest for a moving tag (no
// pull) and reports it as the candidate whenever it differs from the
// recorded baseline in st. On the first check for a service (no recorded
// baseline yet) it records the current digest in st and reports nothing.
func checkMovingTag(baseImage, tag string, reg regFuncs, serviceName string, st map[string]string) (string, error) {
	digest, err := reg.remoteDigest(baseImage, tag)
	if err != nil {
		return "", fmt.Errorf("fetching remote digest for %s:%s: %w", baseImage, tag, err)
	}
	lastKnown := st[serviceName]
	if lastKnown == "" {
		st[serviceName] = digest
		return "", nil
	}
	if digest == lastKnown {
		return "", nil
	}
	return digest, nil
}

// maxDelayChecks bounds how many candidate publish dates one service may
// query per run when walking past too-fresh releases.
const maxDelayChecks = 10

// notifyAvailable reports a newly-seen newer tag via ntfy. Notification
// failures only warn — a lost notification must not fail the check.
func notifyAvailable(cfg *schedule.Config, service, tag string) {
	if cfg.Notify == nil || cfg.Notify.Ntfy == nil {
		return
	}
	token, err := cfg.Notify.Ntfy.Token()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		return
	}
	n := notify.Ntfy{URL: cfg.Notify.Ntfy.URL, Topic: cfg.Notify.Ntfy.Topic, Token: token}
	title := fmt.Sprintf("duva@%s: %s has an update", hostLabel(cfg), service)
	if err := n.Send(title, fmt.Sprintf("%s: newer tag %s is available", service, tag), notify.PriorityDefault); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", err)
	}
}

// hostLabel identifies this box in notifications, so several hosts can
// share one ntfy topic: config.yaml's `hostname:` when set, otherwise the
// short OS hostname.
func hostLabel(cfg *schedule.Config) string {
	if cfg.Hostname != "" {
		return cfg.Hostname
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return h
}

// serve loops forever, running the same check as `run` on cfg.Schedule
// (a 5-field cron expression), until SIGTERM/SIGINT.
func serve(reg regFuncs, out io.Writer) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if _, err := croncal.Next(cfg.Schedule, time.Now()); err != nil {
		return fmt.Errorf("schedule %q: %w", cfg.Schedule, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for {
		next, err := croncal.Next(cfg.Schedule, time.Now())
		if err != nil {
			return err
		}
		wait := time.Until(next)
		fmt.Fprintf(out, "duva: next check at %s (in %s)\n", next.Format(time.RFC3339), wait.Round(time.Second))

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			fmt.Fprintln(out, "duva: shutting down")
			return nil
		case <-timer.C:
		}

		if err := runOnce(reg, out); err != nil {
			fmt.Fprintf(os.Stderr, "duva: check failed: %v\n", err)
		}
	}
}
