// duva watches the pinned services in a compose project and reports what has
// a newer version waiting.
//
// It watches only services that are digest-pinned: pin status is the opt-in.
// `docker pin <service>` starts duva watching it, `docker unpin <service>`
// stops. That is the sharper question than generic drift-watching — "has the
// registry moved past the exact digest I deliberately locked to" — and it is
// only meaningful where a versioning decision was actually made.
//
// Per-service rules live as labels on the service in the compose file, not in
// a separate config: config that governs a service should live with that
// service, so there is one file to check and nothing to drift when services
// are added, renamed or removed.
//
//	services:
//	  radarr:
//	    image: ghcr.io/example/radarr:latest@sha256:...
//	    labels:
//	      duva.include: '^\d+\.\d+\.\d+$'
//	      duva.exclude: '(alpha|beta|rc)'
//	      duva.delay: 7d
//
// Everything else (schedule, notification target, UI port) comes from env
// vars — see envConfig.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/croncal"
	"github.com/Miista/homebrew-docker-pin/internal/notify"
	"github.com/Miista/homebrew-docker-pin/internal/registry"
	"github.com/Miista/homebrew-docker-pin/internal/ui"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

var version = "dev"

// duva's container contract is fixed mount paths plus env vars — no
// working-directory tricks, no search logic:
//
//	/compose  the compose project DIRECTORY
//	/data     small writable volume for state
//
// /compose MUST be the directory, never the compose file alone: include:'d
// nested compose files resolve relative to it, and a single-file bind mount
// silently pins the old inode when the host file is replaced by rename (as
// editors and docker-pin do).
//
// The paths are overridable with DUVA_COMPOSE_DIR / DUVA_STATE_FILE, so the
// binary can be run outside a container -- against a scratch project while
// developing, or on a host that mounts things elsewhere. In the container the
// defaults are the contract and nothing needs setting.
//
// Package variables (not constants) so tests can point them at fixtures.
var (
	composeDir = envOr("DUVA_COMPOSE_DIR", "/compose")
	stateFile  = envOr("DUVA_STATE_FILE", "/data/duva.json")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var realRegistry = watch.Registry{
	ListMatchingTags: registry.ListMatchingTags,
	TagCreated:       registry.TagCreated,
	RemoteDigest:     registry.RemoteDigest,
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("duva", version)
	case "run":
		if err := runOnce(realRegistry, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		if err := serve(realRegistry, os.Stdout); err != nil {
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

type envConfig struct {
	Schedule  string
	Hostname  string
	NtfyURL   string
	NtfyTopic string
	NtfyToken string
	// UIAddr is the listen address for the approval queue, e.g. ":8080".
	// Empty disables the UI entirely — no listener at all.
	UIAddr string
}

func loadEnvConfig() envConfig {
	return envConfig{
		Schedule:  os.Getenv("DUVA_SCHEDULE"),
		Hostname:  os.Getenv("DUVA_HOSTNAME"),
		NtfyURL:   os.Getenv("DUVA_NTFY_URL"),
		NtfyTopic: os.Getenv("DUVA_NTFY_TOPIC"),
		NtfyToken: os.Getenv("DUVA_NTFY_TOKEN"),
		UIAddr:    os.Getenv("DUVA_UI_ADDR"),
	}
}

// hostLabel identifies this box in notifications and in the UI header, so
// several hosts can share one ntfy topic: DUVA_HOSTNAME when set, otherwise
// the short OS hostname.
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

// check runs detection once and folds the result into state: pending updates
// are reconciled, and anything newly available is notified about. It returns
// the findings so callers can report them.
//
// Everything needing approval is pending for now. Deciding what may be applied
// unattended arrives with the policy work; until then duva reports and does
// not act, which is what it has always done.
func check(cfg envConfig, reg watch.Registry, st *watch.State, now time.Time) ([]watch.Finding, error) {
	rootFile, err := compose.FindFile(composeDir)
	if err != nil {
		return nil, err
	}

	findings, err := watch.Project(rootFile, reg, st.Baseline)
	if err != nil {
		return nil, err
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Service < findings[j].Service })

	var available []watch.Finding
	for _, f := range findings {
		if f.Available() {
			available = append(available, f)
		}
	}

	st.Reconcile(available, findings, now.UTC().Format(time.RFC3339))

	// Notify once per candidate, not once per run.
	for _, f := range available {
		if st.Notified[f.Service] == f.Candidate {
			continue
		}
		notifyAvailable(cfg, f)
		st.Notified[f.Service] = f.Candidate
	}
	// Forget notifications for services with nothing outstanding, so the
	// same candidate reappearing later is announced again.
	for _, f := range findings {
		if !f.Available() && f.Status != watch.StatusError {
			delete(st.Notified, f.Service)
		}
	}

	return findings, nil
}

// runOnce performs a single check and prints what it found.
func runOnce(reg watch.Registry, out io.Writer) error {
	cfg := loadEnvConfig()
	st, err := watch.LoadState(stateFile)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	findings, err := check(cfg, reg, st, time.Now())
	if err != nil {
		return err
	}
	report(out, findings)

	if err := st.Save(stateFile); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}

func report(out io.Writer, findings []watch.Finding) {
	for _, f := range findings {
		switch f.Status {
		case watch.StatusAvailable:
			fmt.Fprintf(out, "%s: %s available\n", f.Service, f.Candidate)
		case watch.StatusSkipped:
			fmt.Fprintf(out, "%s: %s, skipping\n", f.Service, f.Reason)
		case watch.StatusError:
			fmt.Fprintf(out, "%s: error: %s\n", f.Service, f.Reason)
		default:
			fmt.Fprintf(out, "%s: up to date\n", f.Service)
		}
	}
}

// notifyAvailable reports one newly-seen update via ntfy. Notification
// failures only warn — a lost notification must not fail the check.
func notifyAvailable(cfg envConfig, f watch.Finding) {
	if cfg.NtfyURL == "" || cfg.NtfyTopic == "" {
		return
	}
	n := notify.Ntfy{URL: cfg.NtfyURL, Topic: cfg.NtfyTopic, Token: cfg.NtfyToken}
	title := fmt.Sprintf("duva@%s: %s has an update", hostLabel(cfg), f.Service)

	body := fmt.Sprintf("%s: newer tag %s is available", f.Service, f.Candidate)
	if f.Kind == watch.KindDigest {
		body = fmt.Sprintf("%s: %s:%s now points at a new image", f.Service, f.Image, f.CurrentTag)
	}
	if err := n.Send(title, body, notify.PriorityDefault); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", err)
	}
}

// store holds the state the scheduled checks write and the UI reads. The UI
// runs on its own goroutine, so access is guarded.
type store struct {
	mu        sync.RWMutex
	state     *watch.State
	lastCheck time.Time
}

func (s *store) Pending() []watch.Pending {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.PendingList()
}

func (s *store) LastCheck() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastCheck.IsZero() {
		return "not yet"
	}
	return s.lastCheck.UTC().Format(time.RFC3339)
}

// serve loops forever, running the same check as `run` on cfg.Schedule (a
// 5-field cron expression), until SIGTERM/SIGINT. With DUVA_UI_ADDR set it
// also serves the approval queue.
func serve(reg watch.Registry, out io.Writer) error {
	cfg := loadEnvConfig()
	if _, err := croncal.Next(cfg.Schedule, time.Now()); err != nil {
		return fmt.Errorf("schedule %q: %w", cfg.Schedule, err)
	}

	st, err := watch.LoadState(stateFile)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	s := &store{state: st}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.UIAddr != "" {
		srv := &http.Server{
			Addr: cfg.UIAddr,
			Handler: (&ui.Server{
				Source:  s,
				Host:    hostLabel(cfg),
				Version: version,
			}).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			fmt.Fprintf(out, "duva: approval queue on %s\n", cfg.UIAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "duva: ui: %v\n", err)
			}
		}()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(shutdown)
		}()
	}

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

		now := time.Now()
		s.mu.Lock()
		findings, err := check(cfg, reg, s.state, now)
		if err == nil {
			s.lastCheck = now
		}
		s.mu.Unlock()

		if err != nil {
			fmt.Fprintf(os.Stderr, "duva: check failed: %v\n", err)
			continue
		}
		report(out, findings)
		if err := s.saveState(); err != nil {
			fmt.Fprintf(os.Stderr, "duva: saving state: %v\n", err)
		}
	}
}

func (s *store) saveState() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Save(stateFile)
}
