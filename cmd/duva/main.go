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
//	      duva.include_tags: '^\d+\.\d+\.\d+$'
//	      duva.exclude_tags: '(alpha|beta|rc)'
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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	// The timezone database, embedded in the binary. Go reads TZ on its own,
	// but resolves a name like Europe/Copenhagen against the host's
	// /usr/share/zoneinfo -- which docker:cli does not carry, so TZ was
	// silently ignored and every timestamp stayed UTC. ~450KB to make the
	// setting mean what it says without depending on the base image.
	_ "time/tzdata"

	"github.com/Miista/homebrew-docker-pin/internal/agent"
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
// /compose is also where duva's git commands run: commit and push need a
// working repository, so whatever is mounted at /compose is assumed to be
// the git repository root as well as the compose project.
//
// That assumption breaks for a compose project that is a subdirectory of a
// larger repository (its own .git lives above the compose file, not beside
// it) -- mounting the compose directory alone then leaves duva with no
// repository, and mounting the compose file's own bind mount at /compose
// would put the compose file outside any mounted .git. DUVA_COMPOSE_SUBDIR
// covers this: mount the repository root at /compose, and set it to the
// path -- relative to /compose -- where the compose project actually lives.
// Unset, /compose is assumed to be the project root, as before.
//
// Package variables (not constants) so tests can point them at fixtures.
// Nothing else changes them: duva runs in a container, where these paths are
// the contract.
var (
	composeDir = "/compose"
	stateFile  = "/data/duva.json"
)

// projectDir is where the compose project actually lives: composeDir itself,
// or the subdirectory DUVA_COMPOSE_SUBDIR names within it. composeDir remains
// the git repository root regardless -- projectDir only affects where the
// compose file is looked for.
func projectDir() (string, error) {
	sub := os.Getenv("DUVA_COMPOSE_SUBDIR")
	if sub == "" {
		return composeDir, nil
	}
	if filepath.IsAbs(sub) {
		return "", fmt.Errorf("DUVA_COMPOSE_SUBDIR must be relative to %s, got %q", composeDir, sub)
	}
	joined := filepath.Join(composeDir, sub)
	// Join cleans ".." segments away rather than erroring, so a value that
	// tries to escape composeDir (e.g. "../etc") would otherwise resolve
	// silently to somewhere outside the mounted repository instead of
	// failing here.
	rel, err := filepath.Rel(composeDir, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("DUVA_COMPOSE_SUBDIR %q escapes %s", sub, composeDir)
	}
	return joined, nil
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

	// Mode is what this process is: see Mode.
	Mode Mode
	// AgentToken is the bearer token this agent requires, and the one a hub
	// presents. Empty means the agent is open to anything that can reach it.
	AgentToken string
	// Agents is the hub's list, as "host=url" entries. Configured rather
	// than discovered: at this scale a registration protocol would add an
	// inbound path by which something could claim to be an agent, to solve a
	// problem a two-line config already solves.
	Agents []agentAddr
}

// Mode is which half of duva this process runs.
//
// The default is ModeLocal -- what duva has always been, one process that
// both watches a host and serves the page about it. A single-host install
// stays exactly as it was, and nothing needs configuring to keep working.
type Mode string

const (
	// ModeLocal watches this host and serves its own page.
	ModeLocal Mode = "local"
	// ModeAgent watches this host and exposes JSON for a hub. No page: the
	// operator reads the hub's.
	ModeAgent Mode = "agent"
	// ModeHub serves the page over other hosts' agents. Watches nothing,
	// and needs neither the docker socket nor the compose files.
	ModeHub Mode = "hub"
)

// agentAddr is one entry in a hub's agent list.
type agentAddr struct {
	Host string
	URL  string
}

func loadEnvConfig() envConfig {
	agents, err := parseAgents(os.Getenv("DUVA_AGENTS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: DUVA_AGENTS: %v\n", err)
		os.Exit(1)
	}
	return envConfig{
		Schedule:  os.Getenv("DUVA_SCHEDULE"),
		Hostname:  os.Getenv("DUVA_HOSTNAME"),
		NtfyURL:   os.Getenv("DUVA_NTFY_URL"),
		NtfyTopic: os.Getenv("DUVA_NTFY_TOPIC"),
		NtfyToken: os.Getenv("DUVA_NTFY_TOKEN"),

		Push: boolEnv("DUVA_GIT_PUSH", false),

		Mode:       parseMode(os.Getenv("DUVA_MODE")),
		AgentToken: os.Getenv("DUVA_AGENT_TOKEN"),
		Agents:     agents,
	}
}

// parseMode reads DUVA_MODE. Unset is local -- the behaviour duva has always
// had -- and an unrecognised value is fatal rather than falling back to it: a
// typo silently meaning "keep doing what you did before" would leave an
// operator who meant to split a host wondering why the hub saw nothing.
func parseMode(raw string) Mode {
	switch raw {
	case "", string(ModeLocal):
		return ModeLocal
	case string(ModeAgent):
		return ModeAgent
	case string(ModeHub):
		return ModeHub
	default:
		fmt.Fprintf(os.Stderr, "Error: DUVA_MODE=%q is not local, agent or hub\n", raw)
		os.Exit(1)
		return ""
	}
}

// parseAgents reads a hub's agent list: comma-separated "host=url" entries.
//
//	DUVA_AGENTS=optiplex=http://192.0.2.10:10256,pi=http://192.0.2.12:10256
//
// The host is named explicitly rather than derived from the URL because it is
// what rows are labelled with and what applies route on; a hostname parsed
// out of an address would change meaning the day an agent moved.
func parseAgents(raw string) ([]agentAddr, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []agentAddr
	seen := map[string]bool{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host, url, ok := strings.Cut(entry, "=")
		host, url = strings.TrimSpace(host), strings.TrimSpace(url)
		if !ok || host == "" || url == "" {
			return nil, fmt.Errorf("%q is not host=url", entry)
		}
		// Two agents under one name would make a row ambiguous in exactly
		// the way the host label exists to prevent.
		if seen[host] {
			return nil, fmt.Errorf("%q appears twice", host)
		}
		seen[host] = true
		out = append(out, agentAddr{Host: host, URL: url})
	}
	return out, nil
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
//
// The returned function takes an extra, optional sink alongside the finding:
// every scheduled run logs to zerolog only, but the page's Start also wants
// each step as it happens, to answer a poller -- so a call from there passes
// one and a call from the scheduler passes nil.
func actor(cfg envConfig, log zerolog.Logger) func(watch.Finding, func(string, ...any)) Result {
	// Read once at startup: a template that does not parse should stop duva
	// here rather than at the moment it would have committed, with the
	// container already updated.
	tmpl, err := loadCommitTemplate()
	if err != nil {
		log.Error().Msgf("%v; commits will use the default subject", err)
		tmpl = defaultCommitTemplate
	}

	base := applyOptions{
		Host:           hostLabel(cfg),
		Push:           cfg.Push,
		CommitTemplate: tmpl,
	}
	return func(f watch.Finding, extra func(string, ...any)) Result {
		opts := base
		opts.Log = func(format string, args ...any) {
			log.Info().Msgf(format, args...)
			if extra != nil {
				extra(format, args...)
			}
		}
		return apply(f, realDocker, realGit, opts)
	}
}

// withoutExtra adapts an actor for checkWith, which never needs a per-call
// sink: every finding it applies came from a scheduled check, not a click.
func withoutExtra(act func(watch.Finding, func(string, ...any)) Result) func(watch.Finding) Result {
	return func(f watch.Finding) Result { return act(f, nil) }
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
	dir, err := projectDir()
	if err != nil {
		return nil, err
	}
	rootFile, err := compose.FindFile(dir)
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
		// Recorded only once it has actually gone out. Marking it sent
		// regardless turns "notify once" into "notify never" the moment ntfy
		// is unreachable: the candidate is remembered as announced, the next
		// run skips it, and the one notification for that update is lost.
		if notifyAvailable(cfg, log, f) {
			st.Notified[f.Service] = f.Candidate
		}
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

	findings, err := checkWith(cfg, log, reg, st, time.Now(), withoutExtra(actor(cfg, log)), realDocker, realGit)
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
// notifyAvailable announces a candidate, and reports whether the announcement
// can be considered delivered.
//
// False only when ntfy was asked and refused. No ntfy configured returns true:
// there is nothing to retry, and treating it as undelivered would make duva
// re-announce forever for someone who never wanted notifications.
func notifyAvailable(cfg envConfig, log zerolog.Logger, f watch.Finding) bool {
	if cfg.NtfyURL == "" || cfg.NtfyTopic == "" {
		return true
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
		log.Warn().Msgf("could not tell you about %s, will try again next run: %v", f.Service, err)
		return false
	}
	return true
}

// store holds the state the scheduled checks write and the UI reads. The UI
// runs on its own goroutine, so access is guarded.
type store struct {
	mu        sync.RWMutex
	state     *watch.State
	lastCheck time.Time
	// act applies an update, or is nil when duva only reports.
	act func(watch.Finding, func(string, ...any)) Result
	// log is where an update triggered from the page is recorded. Someone
	// pressing a button is an event worth a line: without one, the only
	// account of what happened is a redirect the operator may not have read,
	// and the logs show nothing between one scheduled check and the next.
	log zerolog.Logger

	// cfg and reg are what Refresh needs to run checkOnce on demand, the same
	// way the scheduler does. Carried here rather than threaded through the
	// UI layer, which has no business knowing what a check requires.
	cfg envConfig
	reg watch.Registry

	// jobs guards running: an update started from the page runs on its own
	// goroutine, separately from state.mu, so the poller asking about it
	// never waits behind the transaction it is asking about.
	jobs    sync.Mutex
	running map[string]*job

	// refreshing guards refresh: unlike jobs, there is only ever one -- a
	// check is one operation for the whole project, not one per service.
	refreshing sync.Mutex
	refresh    refreshJob
}

// refreshJob is a check-on-demand's progress, the same idea as job but
// without a step list: a check has no discrete stages worth narrating the
// way pull/write/recreate/commit/push are for an apply, so running/done and
// an eventual error is all there is to report.
type refreshJob struct {
	running bool
	done    bool
	err     string
}

// job is one update's progress, as Start's goroutine writes it and Progress
// reads it back. Guarded by store.jobs, like the map that holds it -- a
// service is only ever updated one at a time, so a lock per job would be one
// more thing to get right for no real concurrency gained.
type job struct {
	steps   []string
	done    bool
	message string
	failed  bool
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

// Start begins a queued update in the background, so the page and a
// scheduled run reach the container by the same path -- a click cannot do
// something an unattended run would not -- without the click itself waiting
// on the whole transaction.
//
// The queue row carries everything needed, so approving does not depend on a
// check having run since the page was loaded.
func (s *store) Start(service string) error {
	s.mu.Lock()
	f, soaking, err := s.queuedFinding(service)
	if err != nil {
		s.mu.Unlock()
		s.logf("%s: asked to update, but nothing is waiting for it", service)
		return err
	}
	s.mu.Unlock()

	if !s.beginJob(service) {
		return fmt.Errorf("%s is already being updated", service)
	}

	container := service
	if realDocker.ContainerName != nil {
		container = realDocker.ContainerName(service)
	}
	s.logf("updating %s to %s, asked for from the queue", container, f.Candidate)

	go s.runApply(service, container, f, soaking)
	return nil
}

// beginJob claims a service for a new job, refusing if one is already
// running under it: two clicks on the same row must not race two
// transactions against the same container and compose file.
func (s *store) beginJob(service string) bool {
	s.jobs.Lock()
	defer s.jobs.Unlock()
	if s.running == nil {
		s.running = map[string]*job{}
	}
	if j, ok := s.running[service]; ok && !j.done {
		return false
	}
	s.running[service] = &job{}
	return true
}

// Progress reports a service's most recently started job, for the page's
// poller.
func (s *store) Progress(service string) (ui.Progress, bool) {
	s.jobs.Lock()
	defer s.jobs.Unlock()
	j, ok := s.running[service]
	if !ok {
		return ui.Progress{}, false
	}
	return ui.Progress{
		Steps:   append([]string(nil), j.steps...),
		Done:    j.done,
		Message: j.message,
		Failed:  j.failed,
	}, true
}

// recordStep appends one line to a job's progress, for Start's Log sink.
func (s *store) recordStep(service, line string) {
	s.jobs.Lock()
	defer s.jobs.Unlock()
	if j, ok := s.running[service]; ok {
		j.steps = append(j.steps, line)
	}
}

// finishJob marks a job done with its final message, for Progress to report
// once the transaction has finished.
func (s *store) finishJob(service, message string, failed bool) {
	s.jobs.Lock()
	defer s.jobs.Unlock()
	if j, ok := s.running[service]; ok {
		j.done, j.message, j.failed = true, message, failed
	}
}

// runApply runs the transaction Start queued, recording its steps and
// updating state exactly as the old synchronous Apply did -- just on its own
// goroutine, so a poller asking about it never blocks behind it.
func (s *store) runApply(service, container string, f watch.Finding, soaking bool) {
	res := s.act(f, func(format string, args ...any) {
		s.recordStep(service, fmt.Sprintf(format, args...))
	})
	if res.Failed() {
		msg := fmt.Sprintf("%s failed: %v", res.FailedAt, res.Err)
		s.logf("%s failed for %s: %v", res.FailedAt, container, res.Err)
		s.finishJob(service, msg, true)
		return
	}

	s.mu.Lock()
	// It reached the container, so it is no longer waiting -- for a decision
	// or for time, whichever it was.
	delete(s.state.Pending, service)
	delete(s.state.Soaking, service)
	delete(s.state.Notified, service)
	if f.Kind == watch.KindDigest {
		s.state.Baseline[service] = f.Candidate
	}
	saveErr := s.state.Save(stateFile)
	s.mu.Unlock()
	if saveErr != nil {
		msg := fmt.Sprintf("applied, but saving state failed: %v", saveErr)
		s.finishJob(service, msg, true)
		return
	}

	// The page shows this beside the service name it was clicked for, so it
	// reads as a fragment there; the log names the container itself.
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
	s.logf("%s is %s", container, msg)
	s.finishJob(service, msg, res.FailedAt != "")
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

	// A hub watches nothing, so it has no schedule to validate, no state to
	// load and no docker socket to hold. It is served entirely from what the
	// agents report.
	if cfg.Mode == ModeHub {
		return serveHub(cfg, log)
	}

	if _, err := croncal.Next(cfg.Schedule, time.Now()); err != nil {
		return fmt.Errorf("schedule %q: %w", cfg.Schedule, err)
	}

	st, err := watch.LoadState(stateFile)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	s := &store{state: st, act: actor(cfg, log), log: log, cfg: cfg, reg: reg}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	{
		// The same three interfaces either way -- the store does not know
		// whether it is answering a page or a hub, which is what makes an
		// agent behave identically to the duva it was split out of.
		var handler http.Handler
		var banner string
		if cfg.Mode == ModeAgent {
			handler = (&agent.Server{
				Source:    s,
				Host:      hostLabel(cfg),
				Version:   version,
				Applier:   s,
				Refresher: s,
				Token:     cfg.AgentToken,
			}).Handler()
			banner = "serving as an agent on " + uiAddr
			if cfg.AgentToken == "" {
				// Worth a line rather than silence: an open agent is a
				// root shell on this host for anything that can reach the
				// port, which is a choice someone should have made on
				// purpose.
				log.Warn().Msg("no DUVA_AGENT_TOKEN: this agent will accept any caller that can reach it")
			}
		} else {
			handler = (&ui.Server{
				Source:    s,
				Host:      hostLabel(cfg),
				Version:   version,
				Applier:   s,
				Refresher: s,
			}).Handler()
			banner = "the approval queue is on " + uiAddr
		}
		srv := &http.Server{
			Addr:              uiAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Info().Msg(banner)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error().Msgf("stopped serving: %v", err)
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
//
// Returns the failure rather than only logging it, so a caller that can show
// it to someone -- Refresh, on behalf of a page asking for a check right now
// -- is able to.
//
// s.mu is held for the whole call, not just the state mutation at the end --
// including every registry round trip checkWith makes, one per watched
// service, which is where a check's real time goes (seconds, for a handful
// of services). That is coarser than it needs to be: an apply's own state
// write (see runApply) also needs s.mu briefly, so a refresh in flight
// serializes behind or in front of an apply rather than running alongside
// it. Neither corrupts anything -- it is lock contention, not a race -- but
// a refresh started while an apply is finishing (or the reverse) will sit
// waiting for the whole other operation before it can even begin its own
// work, which reads as "stuck" longer than the actual check takes. Left as
// is deliberately: narrowing the lock to just the mutation, the way runApply
// already does, is the fix if this becomes a real annoyance rather than a
// theoretical one.
func checkOnce(cfg envConfig, reg watch.Registry, s *store, log zerolog.Logger) error {
	now := time.Now()
	s.mu.Lock()
	findings, err := checkWith(cfg, log, reg, s.state, now, withoutExtra(s.act), realDocker, realGit)
	if err == nil {
		s.lastCheck = now
	}
	s.mu.Unlock()

	if err != nil {
		log.Error().Msgf("the check failed, nothing was changed: %v", err)
		return err
	}
	report(log, findings)
	if err := s.saveState(); err != nil {
		log.Error().Msgf("could not save what was found, so it may be reported again: %v", err)
		return err
	}
	return nil
}

// Refresh starts a check on demand in the background, for the page's
// "refresh now" button: waiting for a daily schedule is otherwise the only
// way to ask duva to look again. Mirrors Start/Progress for apply -- a click
// is answered immediately rather than blocking on however long the registry
// takes to answer.
func (s *store) Refresh() error {
	s.refreshing.Lock()
	if s.refresh.running {
		s.refreshing.Unlock()
		return fmt.Errorf("a check is already running")
	}
	s.refresh = refreshJob{running: true}
	s.refreshing.Unlock()

	go func() {
		err := checkOnce(s.cfg, s.reg, s, s.log)
		s.refreshing.Lock()
		s.refresh.running = false
		s.refresh.done = true
		if err != nil {
			s.refresh.err = err.Error()
		}
		s.refreshing.Unlock()
	}()
	return nil
}

// RefreshProgress reports whether the most recently started check is still
// running, for the page's poller.
func (s *store) RefreshProgress() (running bool, done bool, errMsg string) {
	s.refreshing.Lock()
	defer s.refreshing.Unlock()
	return s.refresh.running, s.refresh.done, s.refresh.err
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
		log.Warn().Msgf("could not tell you that %s was updated: %v", f.Service, err)
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
