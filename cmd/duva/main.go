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
	"github.com/rs/zerolog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"
	// The timezone database, embedded in the binary. Go reads TZ on its own,
	// but resolves a name like Europe/Copenhagen against the host's
	// /usr/share/zoneinfo -- which docker:cli does not carry, so TZ was
	// silently ignored and every timestamp stayed UTC. ~450KB to make the
	// setting mean what it says without depending on the base image.
	_ "time/tzdata"

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
	log := newLogger(os.Getenv("DUVA_LOG_LEVEL"))

	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("duva", version)
	case "run":
		if err := runOnce(realRegistry, log); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		if err := serve(realRegistry, log); err != nil {
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
func actor(cfg envConfig, log zerolog.Logger) func(watch.Finding) Result {
	opts := applyOptions{
		Host: hostLabel(cfg),
		Push: cfg.Push,
		Log: func(format string, args ...any) {
			log.Info().Msgf(format, args...)
		},
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
// This one only looks: act is nil, so nothing is applied whatever policy
// allows. For callers that want the findings without the consequences.
func check(cfg envConfig, log zerolog.Logger, reg watch.Registry, st *watch.State, now time.Time) ([]watch.Finding, error) {
	return checkWith(cfg, log, reg, st, now, nil, Docker{}, Git{})
}

// checkWith is check with the acting half injectable, so tests can drive it
// without a docker daemon. act is nil when duva only reports.
func checkWith(cfg envConfig, log zerolog.Logger, reg watch.Registry, st *watch.State,
	now time.Time, act func(watch.Finding) Result, d Docker, g Git) ([]watch.Finding, error) {
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
			applyResult(cfg, log, &findings[i], res, st)
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
	// What duva.delay is holding back. Recorded separately from the approval
	// queue: a soaking update has been decided and is waiting for time, not
	// for a decision.
	st.ReconcileSoaking(findings)

	// Notify once per candidate, not once per run. Anything applied above has
	// already reported its own outcome.
	for _, f := range findings {
		if !f.Available() || st.Notified[f.Service] == f.Candidate {
			continue
		}
		notifyAvailable(cfg, log, f)
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
func runOnce(reg watch.Registry, log zerolog.Logger) error {
	cfg := loadEnvConfig()
	st, err := watch.LoadState(stateFile)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	findings, err := checkWith(cfg, log, reg, st, time.Now(), actor(cfg, log), realDocker, realGit)
	if err != nil {
		return err
	}
	report(log, findings)

	if err := st.Save(stateFile); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}

// report says what the check found, in sentences that mean something to
// someone who has not read duva's source.
//
// A line names the service, what duva saw, and what follows from it. "app:
// not pinned, skipping" said none of those: it assumed the reader knew that
// pinned means a digest in the image line, that duva watches only pinned
// services, and that skipping meant this one would be left alone.
func report(log zerolog.Logger, findings []watch.Finding) {
	for _, f := range findings {
		switch {
		case f.Status == watch.StatusAvailable:
			// Still available after acting means policy did not allow duva to
			// apply it -- anything it could apply, it already has. Warn
			// rather than Info: it is waiting on a person.
			log.Warn().Msgf("%s: %s is available, waiting for approval (%s)",
				f.Service, f.Candidate, f.Why)

		case f.Status == watch.StatusSkipped:
			log.Debug().Msgf("%s: not watching it — %s", f.Service, skipReason(f))

		case f.Status == watch.StatusError:
			log.Error().Msgf("%s: could not check it — %s", f.Service, f.Reason)

		case f.Reason == "applied":
			log.Info().Msgf("%s: updated to %s (%s)", f.Service, f.Candidate, f.Why)

		case f.Soaking != nil:
			log.Info().Msgf("%s: %s is available but still soaking, %s to go",
				f.Service, f.Soaking.Tag, watch.HumanDuration(f.Soaking.Delay-f.Soaking.Age))

		default:
			log.Debug().Msgf("%s: nothing newer", f.Service)
		}
	}
}

// skipReason turns why duva passed over a service into something that says
// what to do about it, where there is anything to do.
func skipReason(f watch.Finding) string {
	switch f.Reason {
	case "not pinned":
		return fmt.Sprintf("%s has no digest, and being pinned is how a service opts in "+
			"(`docker pin %s` adds one)", f.Image, f.Service)
	case "built locally (build:)":
		return "it is built here rather than pulled, so there is no upstream to watch"
	default:
		return f.Reason
	}
}

// notifyAvailable reports one newly-seen update via ntfy. Notification
// failures only warn — a lost notification must not fail the check.
func notifyAvailable(cfg envConfig, log zerolog.Logger, f watch.Finding) {
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
		log.Warn().Msgf("could not send the notification, but the check itself was fine: %v", err)
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
	// log is where an update triggered from the page is recorded. Someone
	// pressing a button is an event worth a line: without one, the only
	// account of what happened is a redirect the operator may not have read,
	// and the logs show nothing between one scheduled check and the next.
	log zerolog.Logger
}

// logf records what duva did when asked from the page.
func (s *store) logf(format string, args ...any) {
	s.log.Info().Msgf(format, args...)
}

func (s *store) Pending() []watch.Pending {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.PendingList()
}

func (s *store) Soaking() []watch.Soaking {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.SoakingList()
}

// queuedFinding is the update waiting under a service's name, from the
// approval queue or from what duva.delay is holding back.
//
// The two are separate deliberately, and this reports which it found: taking
// a soaking update is overriding the soak, which is a thing an operator
// chooses and not something a caller should be able to do without noticing.
func (s *store) queuedFinding(service string) (watch.Finding, bool, error) {
	if p, ok := s.state.Pending[service]; ok {
		return watch.Finding{
			Service:       p.Service,
			File:          p.File,
			Image:         p.Image,
			CurrentTag:    p.CurrentTag,
			CurrentDigest: p.CurrentDigest,
			Kind:          p.Kind,
			Candidate:     p.Candidate,
			Bump:          p.Bump,
			Status:        watch.StatusAvailable,
		}, false, nil
	}
	if p, ok := s.state.Soaking[service]; ok {
		return watch.Finding{
			Service:    p.Service,
			File:       p.File,
			Image:      p.Image,
			CurrentTag: p.CurrentTag,
			Kind:       watch.KindTag,
			Candidate:  p.Candidate,
			Bump:       p.Bump,
			Status:     watch.StatusAvailable,
		}, true, nil
	}
	return watch.Finding{}, false, fmt.Errorf(
		"%s has no update waiting: nothing for approval, nothing soaking", service)
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

	f, soaking, err := s.queuedFinding(service)
	if err != nil {
		s.logf("%s: asked to update, but nothing is waiting for it", service)
		return "", err
	}
	s.logf("%s: updating to %s, asked for from the queue", service, f.Candidate)

	res := s.act(f)
	if res.Failed() {
		s.logf("%s: %s failed: %v", service, res.FailedAt, res.Err)
		return "", fmt.Errorf("%s failed: %v", res.FailedAt, res.Err)
	}

	// It reached the container, so it is no longer waiting -- for a decision
	// or for time, whichever it was.
	delete(s.state.Pending, service)
	delete(s.state.Soaking, service)
	delete(s.state.Notified, service)
	if f.Kind == watch.KindDigest {
		s.state.Baseline[service] = f.Candidate
	}
	if err := s.state.Save(stateFile); err != nil {
		return "", fmt.Errorf("applied, but saving state failed: %w", err)
	}

	msg := fmt.Sprintf("updated to %s", res.Outcome.Tag)
	if soaking {
		// Worth saying: the operator did something duva would not have done
		// on its own yet, and the record should show that they chose to.
		msg += ", ahead of its soak"
	}
	if res.Note != "" {
		msg += " (" + res.Note + ")"
	}
	if res.FailedAt != "" {
		msg += fmt.Sprintf(" — but %s failed: %v", res.FailedAt, res.Err)
	}
	s.logf("%s: %s", service, msg)
	return msg, nil
}

func (s *store) LastCheck() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastCheck.IsZero() {
		return "not yet"
	}
	return since(s.lastCheck, time.Now())
}

// since renders a time as an age. "21:08:27Z" makes the reader do arithmetic
// to answer the only question they had -- is this recent -- so the answer is
// given directly. The exact time is still available: the page carries it as a
// tooltip.
func since(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		// Clock skew, or a state file written by a host running ahead.
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "1 minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "1 hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// LastCheckExact is the same moment in the host's timezone, for the tooltip.
// Local honours TZ, which is why the tzdata import above is needed.
func (s *store) LastCheckExact() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastCheck.IsZero() {
		return ""
	}
	return s.lastCheck.Local().Format("2006-01-02 15:04:05 MST")
}

// serve loops forever, running the same check as `run` on cfg.Schedule (a
// 5-field cron expression), until SIGTERM/SIGINT, serving the approval queue
// alongside it.
func serve(reg watch.Registry, log zerolog.Logger) error {
	cfg := loadEnvConfig()
	if _, err := croncal.Next(cfg.Schedule, time.Now()); err != nil {
		return fmt.Errorf("schedule %q: %w", cfg.Schedule, err)
	}

	st, err := watch.LoadState(stateFile)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	s := &store{state: st, act: actor(cfg, log), log: log}

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
			log.Info().Msgf("the approval queue is on %s", uiAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error().Msgf("the approval queue stopped serving: %v", err)
			}
		}()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(shutdown)
		}()
	}

	// Once at startup, rather than waiting for the first tick: a daily
	// schedule would otherwise leave duva idle for up to a day, showing a
	// queue from some earlier run under a footer saying it had not looked yet.
	// It is also what makes restarting duva a way to ask for a check now.
	checkOnce(cfg, reg, s, log)

	for {
		next, err := croncal.Next(cfg.Schedule, time.Now())
		if err != nil {
			return err
		}
		wait := time.Until(next)
		// Local, with the zone named: an operator reading this wants to know
		// when it fires in their own time. TZ selects the zone, which needs
		// the embedded database -- see the tzdata import.
		log.Info().Msgf("next check at %s, in %s",
			next.Local().Format("2006-01-02 15:04:05 MST"), wait.Round(time.Second))

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Info().Msg("shutting down")
			return nil
		case <-timer.C:
		}

		checkOnce(cfg, reg, s, log)
	}
}

// checkOnce runs one check and reports it, for both the startup check and
// every scheduled one. They were separate blocks that drifted: the startup
// one recorded its findings but never printed them, so duva looked like it
// had found nothing.
func checkOnce(cfg envConfig, reg watch.Registry, s *store, log zerolog.Logger) {
	now := time.Now()
	s.mu.Lock()
	findings, err := checkWith(cfg, log, reg, s.state, now, s.act, realDocker, realGit)
	if err == nil {
		s.lastCheck = now
	}
	s.mu.Unlock()

	if err != nil {
		log.Error().Msgf("the check failed, nothing was changed: %v", err)
		return
	}
	report(log, findings)
	if err := s.saveState(); err != nil {
		log.Error().Msgf("could not save what was found, so it may be reported again: %v", err)
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
func applyResult(cfg envConfig, log zerolog.Logger, f *watch.Finding, res Result, st *watch.State) {
	switch {
	case res.Applied:
		if f.Kind == watch.KindDigest {
			st.Baseline[f.Service] = f.Candidate
		}
		f.Status = watch.StatusUpToDate
		f.Reason = "applied"
		delete(st.Notified, f.Service)
		notifyApplied(cfg, log, *f, res)

	case res.Note != "" && res.Outcome.NewRaw == "":
		// Nothing was attempted -- the repository was busy. The finding stays
		// available so the next run tries again, and it is a warning rather
		// than an error: someone was committing, which is not a fault.
		fmt.Fprintf(os.Stderr, "duva: %s: %s\n", f.Service, res.Note)

	case res.Err != nil:
		f.Status = watch.StatusError
		f.Reason = fmt.Sprintf("%s failed: %v", res.FailedAt, res.Err)
		notifyFailed(cfg, log, *f, res)

	default:
		// Nothing to do: the registry offered what the file already pins.
		f.Status = watch.StatusUpToDate
		f.Reason = "already at this digest"
	}
}

// notifyApplied announces an update duva made itself. Notification failures
// only warn: a lost message must not make a successful update look failed.
func notifyApplied(cfg envConfig, log zerolog.Logger, f watch.Finding, res Result) {
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
		log.Warn().Msgf("could not send the notification, but the check itself was fine: %v", err)
	}
}

// notifyFailed announces an update that did not reach the container, at high
// priority: something needs a human.
func notifyFailed(cfg envConfig, log zerolog.Logger, f watch.Finding, res Result) {
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
		log.Warn().Msgf("could not send the notification, but the check itself was fine: %v", err)
	}
}
