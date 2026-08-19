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
	"strings"
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

// duva's container contract is fixed mount paths plus env vars — no
// working-directory tricks, no search logic:
//
//	/compose  the compose project DIRECTORY, mounted read-only
//	/data     small writable volume for the dedup state
//
// /compose MUST be the directory, never the compose file alone: include:'d
// nested compose files resolve relative to it, and a single-file bind mount
// silently pins the old inode when the host file is replaced by rename (as
// editors and docker-pin do).
//
// composeDir is a package variable only so tests can point it at a fixture.
var composeDir = "/compose"

// duva only watches services that are pinned (image: has @sha256:...) —
// pin status is the opt-in/opt-out: `docker pin <service>` starts duva
// watching it, `docker unpin <service>` stops it. Unpinned services are
// skipped with a log line, never notified about, never state-tracked.
//
// Per-service rules (tags/exclude/delay) live as labels on the service in
// the compose file, not in a separate config file — config that governs a
// service should live with that service, so there's one file to check and
// nothing to drift when services are added/renamed/removed:
//
//	services:
//	  radarr:
//	    image: ghcr.io/linuxserver/radarr:latest@sha256:...
//	    labels:
//	      duva.include: '^\d+\.\d+\.\d+$'
//	      duva.exclude: '(alpha|beta|rc)'
//	      duva.delay: 7d
//
// Everything else (schedule, notify, hostname) comes from env vars — see
// loadEnvConfig.
type envConfig struct {
	Schedule  string
	Hostname  string
	NtfyURL   string
	NtfyTopic string
	NtfyToken string
}

func loadEnvConfig() envConfig {
	return envConfig{
		Schedule:  os.Getenv("DUVA_SCHEDULE"),
		Hostname:  os.Getenv("DUVA_HOSTNAME"),
		NtfyURL:   os.Getenv("DUVA_NTFY_URL"),
		NtfyTopic: os.Getenv("DUVA_NTFY_TOPIC"),
		NtfyToken: os.Getenv("DUVA_NTFY_TOKEN"),
	}
}

// serviceRules is a pinned service's include/exclude/delay, read from its
// compose labels.
type serviceRules struct {
	Name    string
	Include string
	Exclude string
	Delay   string
}

func loadServiceRules(composeFile, name string) (serviceRules, error) {
	labels, err := compose.Labels(composeFile, name)
	if err != nil {
		return serviceRules{}, err
	}
	return serviceRules{
		Name:    name,
		Include: labels["duva.include"],
		Exclude: labels["duva.exclude"],
		Delay:   labels["duva.delay"],
	}, nil
}

// runOnce checks every pinned service once and reports what it found.
func runOnce(reg regFuncs, out io.Writer) error {
	cfg := loadEnvConfig()
	composeFile, err := compose.FindFile(composeDir)
	if err != nil {
		return err
	}

	statePath := stateFile
	st, err := loadState(statePath)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	names, err := compose.ListServices(composeFile)
	if err != nil {
		return err
	}

	changed := false
	for _, name := range names {
		serviceFile, err := compose.ResolveServiceIn(composeFile, name)
		if err != nil {
			fmt.Fprintf(out, "%s: error: %v\n", name, err)
			continue
		}
		raw, err := compose.RawImage(serviceFile, name)
		if err != nil {
			fmt.Fprintf(out, "%s: error: %v\n", name, err)
			continue
		}
		if !strings.Contains(raw, "@sha256:") {
			fmt.Fprintf(out, "%s: not pinned, skipping\n", name)
			continue
		}

		svc, err := loadServiceRules(composeFile, name)
		if err != nil {
			fmt.Fprintf(out, "%s: error: %v\n", name, err)
			continue
		}

		before := st[name]
		candidate, err := checkService(composeFile, svc, reg, st)
		if err != nil {
			fmt.Fprintf(out, "%s: error: %v\n", name, err)
			continue
		}
		if st[name] != before {
			changed = true // checkService recorded a moving-tag baseline
		}
		if candidate == "" {
			fmt.Fprintf(out, "%s: up to date\n", name)
			continue
		}
		if st[name] == candidate {
			fmt.Fprintf(out, "%s: %s available (already notified)\n", name, candidate)
			continue
		}
		fmt.Fprintf(out, "%s: %s available\n", name, candidate)
		notifyAvailable(cfg, name, candidate)
		st[name] = candidate
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
func checkService(rootFile string, svc serviceRules, reg regFuncs, st map[string]string) (string, error) {
	serviceFile, err := compose.ResolveServiceIn(rootFile, svc.Name)
	if err != nil {
		return "", err
	}
	baseImage, currentTag, err := compose.ParseImage(serviceFile, svc.Name)
	if err != nil {
		return "", err
	}

	if svc.Include == "" {
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
func notifyAvailable(cfg envConfig, service, tag string) {
	if cfg.NtfyURL == "" || cfg.NtfyTopic == "" {
		return
	}
	n := notify.Ntfy{URL: cfg.NtfyURL, Topic: cfg.NtfyTopic, Token: cfg.NtfyToken}
	title := fmt.Sprintf("duva@%s: %s has an update", hostLabel(cfg), service)
	if err := n.Send(title, fmt.Sprintf("%s: newer tag %s is available", service, tag), notify.PriorityDefault); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", err)
	}
}

// hostLabel identifies this box in notifications, so several hosts can
// share one ntfy topic: DUVA_HOSTNAME when set, otherwise the short OS
// hostname.
func hostLabel(cfg envConfig) string {
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
	cfg := loadEnvConfig()
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
