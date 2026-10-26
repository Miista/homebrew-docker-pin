// queue is the gate.
//
// It receives a watcher's webhook, works out what this host declares it is
// running, decides whether that is an update worth making and whether policy
// allows making it unattended, and either queues it for a person or hands it
// to whatever applies.
//
// It talks to a registry never, and to the docker daemon never. Everything it
// knows about this host comes from the compose file mounted at /compose, and
// everything it does about it goes to an updater it reaches by name.
//
//	/compose   the compose project directory, read-only
//
// Configuration, through internal/settings (DUVA_SETTING_<KEY>):
//
//	host               what this queue calls itself, for a page over several
//	api_token          the bearer token a peer's page must present
//	update_timeout     how long an apply may run before it is called failed
//	reconcile_interval how often a queued entry is re-checked against the
//	                   compose file, to catch one satisfied by something other
//	                   than this queue's own apply (default 10m); also how
//	                   often a delayed entry is checked for release
package gate

import (
	"context"
	"fmt"
	"net/http"
	"time"
	// The timezone database, embedded in the binary. Go reads TZ on its
	// own, but resolves a name like Europe/Copenhagen against the host's
	// /usr/share/zoneinfo -- which a scratch image does not carry, so TZ
	// would be silently ignored and every timestamp would stay UTC:
	// the queue shows when an entry was first seen, and a log line with the wrong hour is worse than one with none.
	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/compose"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/update"
	// Aliased: this binary lives in queue/, so the bare name would be
	// ambiguous with the package it is the command for.
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/jobs"
	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
)

var Version = "dev"

// addr is where the queue serves. Fixed rather than configurable: inside a
// container the port is invisible to the operator, who chooses what to publish
// it as.
const addr = ":8080"

// composeDir is the contract with whoever runs this.
var composeDir = "/compose"

// stateFile is where the queue is kept across a restart. A named volume
// mounted at /data, like the watcher's.
var stateFile = "/data/duva.db"

// defaultApplyTimeout bounds an apply.
//
// It is the only thing the queue has to believe about work it did not do:
// an updater that died mid-apply would otherwise hold its entry out of the queue
// forever, invisible to the person who could have noticed.
const defaultApplyTimeout = 30 * time.Minute

// notify wires a notifier to the two things worth telling someone about.
//
// A function rather than four lines inside run, so a test can exercise the
// same wiring the binary uses: without that, deleting either of these lines
// breaks nothing that is checked, and the notification silently stops.
//
// A nil notifier is nobody to tell, which is the default -- and it is left as
// a nil Announce rather than a no-op function, so the handler's own guard is
// what decides and there is one answer to "is anyone notified".
func notify(n *ntfy, host string, h *q.Handler, a *applier) {
	if n == nil {
		return
	}
	h.Announce = func(e q.Entry) { n.announce(host, e, queued, "") }
	// A successful auto-apply says nothing: it is the policy working as
	// configured. A failed one may have left the container down and the
	// repository dirty, which wants a person.
	if a != nil {
		a.onFailure = func(e q.Entry, reason string) {
			n.announce(host, e, failedApply, reason)
		}
	}
}

// Options is what a caller in the same process supplies instead of the
// environment.
type Options struct {
	// Updater applies what is decided. Nil applies nothing: every decision
	// waits for a person, which is how a queue is tested without one.
	Updater update.Updater
	// Root is the compose file. Empty means the one mounted at /compose.
	Root string
	// Containers lists this host's containers, for the service sync. Nil
	// skips it.
	Containers func() ([]q.Container, error)
	// Check asks the watcher to look now, at one service or every one for
	// "", and at least back far back, for the queue API's "Check…". Resolve
	// is what a tag points at now, for blocking a version change. Nil leaves
	// each action unavailable.
	Check   func(service string, back time.Duration, rec *q.CheckRecorder) error
	Resolve func(image, tag string) (string, error)
	// LastChecked is when the watcher last looked for new tags, for the
	// page. Nil: not reported.
	LastChecked func() time.Time
	// NextCheck is when the watcher's scheduled check runs next, for the
	// page's Jobs list. Nil: not reported.
	NextCheck func() time.Time
	// Store is where the queue is kept. Nil opens /data/duva.db.
	Store *store.Store
	// Labels reads a service's policy from its container. Nil reads it from
	// the compose file, which is all a queue without the socket can do.
	Labels func(container string) (map[string]string, bool, error)
}

// Gate is the queue, assembled: what decides, what is waiting, and what
// serves both to a UI.
type Gate struct {
	log   zerolog.Logger
	cfg   config
	root  string
	queue *q.Pending
	// handler decides about one notice; Handle is how a watcher in the same
	// process reaches it.
	handler *q.Handler
	// API is the queue's HTTP surface, for a UI. Exposed so a UI in the same
	// process can serve it without a network in between.
	API        *q.Server
	applying   q.Applier
	live       *applier
	labels     func(container string) (map[string]string, bool, error)
	containers func() ([]q.Container, error)
	// poke holds at most one request for a reconcile; see Poke.
	poke chan struct{}
	// st is where the queue is kept, and where its jobs record their runs.
	st *store.Store
	// rec is the reconciler, once Run has started it.
	rec *reconciler
}

// New assembles a queue: it reads the compose project, restores what was
// waiting, and connects whatever applies.
func New(log zerolog.Logger, opts Options) (*Gate, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	token, err := tokenFromEnv()
	if err != nil {
		return nil, err
	}
	cfg.Token = token

	root := opts.Root
	if root == "" {
		var err error
		if root, err = projectFile(); err != nil {
			return nil, err
		}
	}
	// Before anything else: a queue that cannot read the project it is
	// deciding about should say so now, not when the first notice arrives
	// and is rejected for reasons nobody can see.
	if _, err := compose.ContainerIndex(root); err != nil {
		return nil, fmt.Errorf("reading the compose project at %s: %w", root, err)
	}

	// Restored from /data, so a restart is not a decision about what is
	// waiting.
	st := opts.Store
	if st == nil {
		var err error
		if st, err = store.Open(stateFile); err != nil {
			return nil, err
		}
	}
	queue := q.Over(st, func(err error) {
		log.Error().Msgf("the queue could not record a change: %v", err)
	})
	if n := queue.Len(); n > 0 {
		log.Info().Msgf("%d entr(ies) were still waiting from before the restart", n)
	}

	updater := opts.Updater

	// The concrete one, so a notifier can be attached to it below: the
	// Applier interface deliberately says nothing about failures -- the
	// handler does not need to know -- but the thing being notified does.
	var live *applier
	var applying q.Applier
	if updater != nil {
		live = newApplier(updater, cfg.ApplyTimeout, log, queue)
		applying = live
	} else {
		// Legitimate, not broken: a queue with no updater queues everything
		// and applies nothing, which is what someone running it to watch
		// rather than to act would want.
		log.Info().Msg("no updater: every decision will be queued and nothing applied")
	}

	handler := &q.Handler{
		Lookup:  q.Lookup{Root: root, Labels: opts.Labels},
		Queue:   queue,
		Applier: applying,
		Log:     log,
	}
	// Whether a service should be acted on at all is the updater's to answer:
	// it is the stage that holds the docker socket, and everything the queue
	// knows about this host comes from the compose file -- which says nothing
	// about whether a container is running.
	//
	// Nil without an updater, so a watch-only queue keeps queueing everything.
	// There is nothing to act on, so nothing to refuse on behalf of.
	if live != nil {
		handler.Applicable = live.Applicable
	}
	notifier, err := newNtfy(log)
	if err != nil {
		return nil, err
	}
	notify(notifier, cfg.Host, handler, live)

	g := &Gate{
		log:     log,
		cfg:     cfg,
		root:    root,
		queue:   queue,
		handler: handler,
		API: &q.Server{
			Queue:   queue,
			Applier: applying,
			Host:    cfg.Host,
			Version: Version,
			Token:   cfg.Token,
			Check:   opts.Check,
			Resolve: opts.Resolve,

			LastChecked: opts.LastChecked,
		},
		applying:   applying,
		live:       live,
		labels:     opts.Labels,
		containers: opts.Containers,
		poke:       make(chan struct{}, 1),
		st:         st,
	}
	g.API.Tick = g.Poke
	g.API.Jobs = func() []jobs.Status {
		var nextCheck time.Time
		if opts.NextCheck != nil {
			nextCheck = opts.NextCheck()
		}
		return jobs.Statuses(st, nextCheck, g.NextTick())
	}
	return g, nil
}

// Host is what this queue calls itself.
func (g *Gate) Host() string { return g.cfg.Host }

// Poke asks for the queue to be reconciled now rather than on its next tick:
// a container was created, started, stopped or removed, so what is waiting --
// and whether a soak held for a stopped container can go -- may have changed.
//
// Coalesced. A request already pending absorbs this one, so a compose up that
// starts ten containers costs one reconcile, not ten, and a caller never
// blocks.
func (g *Gate) Poke() {
	select {
	case g.poke <- struct{}{}:
	default:
	}
}

// NextTick is when the sync, reconcile and release run next: the last tick
// plus the interval. Zero before the first. A container event or a person can
// bring it sooner.
func (g *Gate) NextTick() time.Time {
	if g.rec == nil {
		return time.Time{}
	}
	return g.rec.next()
}

// IsBlocked reports whether a release is on the blocklist, and why.
func (g *Gate) IsBlocked(image, digest string) (string, bool) {
	return g.queue.IsBlocked(image, digest)
}

// Handle decides about one notice: what a watcher in the same process calls
// instead of POSTing the webhook.
func (g *Gate) Handle(n q.Notice) q.Result { return g.handler.Handle(n) }

// Run serves the queue's API and reconciles on its interval, until ctx is
// done.
func (g *Gate) Run(ctx context.Context) error {
	log, cfg := g.log, g.cfg
	srv := &http.Server{
		Addr:              addr,
		Handler:           g.API.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	reconcileDone := make(chan struct{})
	rec := &reconciler{queue: g.queue, root: g.root, interval: cfg.ReconcileInterval, log: log,
		applier: g.applying, labels: g.labels, poke: g.poke, containers: g.containers, st: g.st,
		announce: func(e q.Entry) {
			if g.handler.Announce != nil {
				g.handler.Announce(e)
			}
		}}
	g.rec = rec
	if g.live != nil {
		rec.applicable = g.live.Applicable
	}
	go rec.run(reconcileDone)
	defer close(reconcileDone)

	go func() {
		log.Info().Msgf("deciding for %s, serving on %s", cfg.Host, addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error().Msgf("stopped serving: %v", err)
		}
	}()

	<-ctx.Done()
	log.Info().Msg("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

type config struct {
	Host              string
	Token             string
	ApplyTimeout      time.Duration
	ReconcileInterval time.Duration
}

func loadConfig() (config, error) {
	c := config{Host: settings.HostName()}
	// A duration that does not parse is not one to fall back from: the
	// fallback would be silently different from what was asked for, and
	// nobody would know until an apply hung.
	var err error
	if c.ApplyTimeout, err = settings.Duration(settings.UpdateTimeout, defaultApplyTimeout); err != nil {
		return c, err
	}
	if c.ReconcileInterval, err = settings.Duration(settings.ReconcileInterval, defaultReconcileInterval); err != nil {
		return c, err
	}
	return c, nil
}

// projectFile is the compose file to read, which must be at the root of what
// is mounted.
//
// No subdirectory, and no walking. This stage reads compose files and touches
// no git, so it has no reason to see the repository around them -- the project
// is what is mounted, and if it is not there that is a misconfiguration to say
// out loud. FindFile would climb out of the mount looking for one and answer
// with whatever it found, which is a wrong answer wearing the shape of a right
// one.
func projectFile() (string, error) {
	return compose.FileIn(composeDir)
}
