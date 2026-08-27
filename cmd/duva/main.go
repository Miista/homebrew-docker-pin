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
	"strconv"
)

var version = "dev"

// uiAddr is where the approval queue is served. Fixed rather than
// configurable: inside a container the port is invisible to the operator,
// who chooses what -- if anything -- to publish it as. A setting here would
// only be a second place to look.
const uiAddr = ":8080"

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
// Package variables (not constants) so tests can point them at fixtures.
// Nothing else changes them: duva runs in a container, where these paths are
// the contract.
var (
	composeDir = "/compose"
	stateFile  = "/data/duva.json"
)

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
	// Push publishes duva's commits. Off by default: pushing needs a key in a
	// container that already holds the docker socket, for value a human's
	// next push delivers anyway.
	Push bool
}

func loadEnvConfig() envConfig {
	return envConfig{
		Schedule:  os.Getenv("DUVA_SCHEDULE"),
		Hostname:  os.Getenv("DUVA_HOSTNAME"),
		NtfyURL:   os.Getenv("DUVA_NTFY_URL"),
		NtfyTopic: os.Getenv("DUVA_NTFY_TOPIC"),
		NtfyToken: os.Getenv("DUVA_NTFY_TOKEN"),

		Push: boolEnv("DUVA_GIT_PUSH", false),
	}
}

// boolEnv reads a boolean environment variable, treating anything unset as
// the given default and anything unrecognised as a hard error: a typo
// silently meaning "no" is the kind of thing discovered months later, when
// something has not been happening and nobody knows why.
func boolEnv(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s=%q is not a boolean\n", key, raw)
		os.Exit(1)
	}
	return v
}

// actor returns the function that applies an update.
//
// Acting is not optional: it is what duva is for, and what it may act on is
// already decided per service by duva.auto -- which defaults to none, so a
// service is only ever updated because someone said so. A global switch on
// top would be a second brake on the same pedal.
func actor(cfg envConfig) func(watch.Finding) Result {
	opts := applyOptions{
		Host: hostLabel(cfg),
		Push: cfg.Push,
	}
	return func(f watch.Finding) Result {
		return apply(f, realDocker, realGit, opts)
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
	return checkWith(cfg, reg, st, now, nil, Docker{}, Git{})
}

// checkWith is check with the acting half injectable, so tests can drive it
// without a docker daemon. act is nil when duva only reports.
func checkWith(cfg envConfig, reg watch.Registry, st *watch.State, now time.Time,
	act func(watch.Finding) Result, d Docker, g Git) ([]watch.Finding, error) {
	rootFile, err := compose.FindFile(composeDir)
	if err != nil {
		return nil, err
	}

	findings, err := watch.Project(rootFile, reg, st.Baseline)
	if err != nil {
		return nil, err
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Service < findings[j].Service })

	// Apply what policy allows, before deciding what is still outstanding: a
	// service that has just been updated is no longer waiting for anything.
	if act != nil {
		for i, f := range findings {
			if !f.AutoApplies() {
				continue
			}
			res := act(f)
			applyResult(cfg, &findings[i], res, st)
		}
	}

	// Only what needs a human goes in the queue. An update policy allows duva
	// to apply is not "waiting" for anything.
	var needApproval []watch.Finding
	for _, f := range findings {
		if f.NeedsApproval() {
			needApproval = append(needApproval, f)
		}
	}

	st.Reconcile(needApproval, findings, now.UTC().Format(time.RFC3339))

	// Notify once per candidate, not once per run. Anything applied above has
	// already reported its own outcome.
	for _, f := range findings {
		if !f.Available() || st.Notified[f.Service] == f.Candidate {
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

	findings, err := checkWith(cfg, reg, st, time.Now(), actor(cfg), realDocker, realGit)
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
			verb := "needs approval"
			if f.AutoApplies() {
				verb = "would apply"
			}
			fmt.Fprintf(out, "%s: %s available — %s (%s)\n", f.Service, f.Candidate, verb, f.Why)
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
	what := "needs approval"
	if f.AutoApplies() {
		what = "will be applied"
	}
	title := fmt.Sprintf("duva@%s: %s %s", hostLabel(cfg), f.Service, what)

	body := fmt.Sprintf("%s: %s -> %s\n%s", f.Service, f.CurrentTag, f.Candidate, f.Why)
	if f.Kind == watch.KindDigest {
		body = fmt.Sprintf("%s: %s:%s now points at a new image\n%s",
			f.Service, f.Image, f.CurrentTag, f.Why)
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
	// act applies an update, or is nil when duva only reports.
	act func(watch.Finding) Result
}

func (s *store) Pending() []watch.Pending {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.PendingList()
}

// Apply performs a queued update, so the page and a scheduled run reach the
// container by the same path -- a click cannot do something an unattended run
// would not.
//
// The queue row carries everything needed, so approving does not depend on a
// check having run since the page was loaded.
func (s *store) Apply(service string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.state.Pending[service]
	if !ok {
		return "", fmt.Errorf("%s is not waiting for approval", service)
	}
	f := watch.Finding{
		Service:       p.Service,
		File:          p.File,
		Image:         p.Image,
		CurrentTag:    p.CurrentTag,
		CurrentDigest: p.CurrentDigest,
		Kind:          p.Kind,
		Candidate:     p.Candidate,
		Bump:          p.Bump,
		Status:        watch.StatusAvailable,
	}

	res := s.act(f)
	if res.Failed() {
		return "", fmt.Errorf("%s failed: %v", res.FailedAt, res.Err)
	}

	// It reached the container, so it is no longer waiting.
	delete(s.state.Pending, service)
	delete(s.state.Notified, service)
	if f.Kind == watch.KindDigest {
		s.state.Baseline[service] = f.Candidate
	}
	if err := s.state.Save(stateFile); err != nil {
		return "", fmt.Errorf("applied, but saving state failed: %w", err)
	}

	msg := fmt.Sprintf("updated to %s", res.Outcome.Tag)
	if res.Note != "" {
		msg += " (" + res.Note + ")"
	}
	if res.FailedAt != "" {
		msg += fmt.Sprintf(" — but %s failed: %v", res.FailedAt, res.Err)
	}
	return msg, nil
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
// 5-field cron expression), until SIGTERM/SIGINT, serving the approval queue
// alongside it.
func serve(reg watch.Registry, out io.Writer) error {
	cfg := loadEnvConfig()
	if _, err := croncal.Next(cfg.Schedule, time.Now()); err != nil {
		return fmt.Errorf("schedule %q: %w", cfg.Schedule, err)
	}

	st, err := watch.LoadState(stateFile)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	s := &store{state: st, act: actor(cfg)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	{
		srv := &http.Server{
			Addr: uiAddr,
			Handler: (&ui.Server{
				Source:  s,
				Host:    hostLabel(cfg),
				Version: version,
				Applier: s,
			}).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			fmt.Fprintf(out, "duva: approval queue on %s\n", uiAddr)
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

	// Check once at startup rather than waiting for the first tick. A daily
	// schedule would otherwise leave duva idle for up to a day, showing a
	// queue from some earlier run under a footer saying it had not looked yet.
	if _, err := checkWith(cfg, reg, s.state, time.Now(), s.act, realDocker, realGit); err != nil {
		fmt.Fprintf(os.Stderr, "duva: %v\n", err)
	} else {
		s.mu.Lock()
		s.lastCheck = time.Now()
		s.mu.Unlock()
		if err := s.saveState(); err != nil {
			fmt.Fprintf(os.Stderr, "duva: %v\n", err)
		}
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
		findings, err := checkWith(cfg, reg, s.state, now, actor(cfg), realDocker, realGit)
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

// applyResult folds an applied update back into the run: the finding stops
// being "available" when the container took it, the moving-tag baseline
// advances only then, and the outcome is announced.
//
// The baseline is deliberately not advanced on detection alone -- only when
// the update is applied -- so a move that could not be applied is still
// waiting on the next run rather than being silently forgotten.
func applyResult(cfg envConfig, f *watch.Finding, res Result, st *watch.State) {
	switch {
	case res.Applied:
		if f.Kind == watch.KindDigest {
			st.Baseline[f.Service] = f.Candidate
		}
		f.Status = watch.StatusUpToDate
		f.Reason = "applied"
		delete(st.Notified, f.Service)
		notifyApplied(cfg, *f, res)

	case res.Err != nil:
		f.Status = watch.StatusError
		f.Reason = fmt.Sprintf("%s failed: %v", res.FailedAt, res.Err)
		notifyFailed(cfg, *f, res)

	default:
		// Nothing to do: the registry offered what the file already pins.
		f.Status = watch.StatusUpToDate
		f.Reason = "already at this digest"
	}
}

// notifyApplied announces an update duva made itself. Notification failures
// only warn: a lost message must not make a successful update look failed.
func notifyApplied(cfg envConfig, f watch.Finding, res Result) {
	if cfg.NtfyURL == "" || cfg.NtfyTopic == "" {
		return
	}
	n := notify.Ntfy{URL: cfg.NtfyURL, Topic: cfg.NtfyTopic, Token: cfg.NtfyToken}
	title := fmt.Sprintf("duva@%s: %s updated", hostLabel(cfg), f.Service)
	body := fmt.Sprintf("%s -> %s", res.Outcome.OldRaw, res.Outcome.NewRaw)
	if res.Note != "" {
		body += "\n" + res.Note
	}
	if res.FailedAt != "" {
		// Applied, but the record-keeping did not finish.
		body += fmt.Sprintf("\nnote: %s failed: %v", res.FailedAt, res.Err)
	}
	if err := n.Send(title, body, notify.PriorityDefault); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", err)
	}
}

// notifyFailed announces an update that did not reach the container, at high
// priority: something needs a human.
func notifyFailed(cfg envConfig, f watch.Finding, res Result) {
	if cfg.NtfyURL == "" || cfg.NtfyTopic == "" {
		return
	}
	n := notify.Ntfy{URL: cfg.NtfyURL, Topic: cfg.NtfyTopic, Token: cfg.NtfyToken}
	title := fmt.Sprintf("duva@%s: %s FAILED to update", hostLabel(cfg), f.Service)
	body := fmt.Sprintf("%s -> %s\n%s failed: %v", f.CurrentTag, f.Candidate, res.FailedAt, res.Err)
	if res.Reverted {
		body += "\nthe compose file was put back; the container is unchanged"
	}
	if err := n.Send(title, body, notify.PriorityHigh); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: notification failed: %v\n", err)
	}
}
