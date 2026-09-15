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
// Configuration:
//
//	DUVA_QUEUE_HOST         what this queue calls itself, for a UI over several
//	DUVA_QUEUE_TOKEN        the bearer token a UI must present
//	DUVA_QUEUE_UPDATE_URL    where the updater is, e.g. http://actor:8080
//	DUVA_QUEUE_UPDATE_TOKEN  the token the updater requires
//	DUVA_QUEUE_UPDATE_TIMEOUT how long an apply may run before it is called failed
//	DUVA_QUEUE_COMPOSE_SUBDIR  where the project lives within /compose
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	// The timezone database, embedded in the binary. Go reads TZ on its
	// own, but resolves a name like Europe/Copenhagen against the host's
	// /usr/share/zoneinfo -- which a scratch image does not carry, so TZ
	// would be silently ignored and every timestamp would stay UTC:
	// the queue shows when an entry was first seen, and a log line with the wrong hour is worse than one with none.
	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/compose"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/update"
	// Aliased: this binary lives in queue/, so the bare name would be
	// ambiguous with the package it is the command for.
	q "github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/watchevent"
)

var version = "dev"

// addr is where the queue serves. Fixed rather than configurable: inside a
// container the port is invisible to the operator, who chooses what to publish
// it as.
const addr = ":8080"

// composeDir is the contract with whoever runs this.
var composeDir = "/compose"

// stateFile is where the queue is kept across a restart. A named volume
// mounted at /data, like the watcher's.
var stateFile = "/data/queue.json"

// defaultApplyTimeout bounds an apply.
//
// It is the only thing the queue has to believe about work it did not do:
// an updater that died mid-apply would otherwise hold its entry out of the queue
// forever, invisible to the person who could have noticed.
const defaultApplyTimeout = 30 * time.Minute

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("queue", version)
			return
		case "health":
			// For HEALTHCHECK. A subcommand rather than a port something
			// polls, because this image carries neither a shell nor curl to
			// poll it with.
			os.Exit(health())
		default:
			fmt.Fprintln(os.Stderr, "Usage: queue [health|version]")
			os.Exit(1)
		}
	}
	log := newLogger(os.Getenv("DUVA_QUEUE_LOG_LEVEL"))
	if err := run(log); err != nil {
		log.Error().Msgf("%v", err)
		os.Exit(1)
	}
}

// health is the exit code for the health subcommand: 0 healthy, 1 not.
//
// It asks the running process, over the loopback port it serves on, rather
// than re-deriving anything itself. A health check that re-reads the compose
// file in a second process would report on the file, not on the server: the
// server could be wedged, or dead, and the file would still parse.
//
// What it does not check is the update. A queue with no updater is a queue
// that queues everything and applies nothing, which is a legitimate way to
// run it -- and the only way it runs today.
func health() int {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://127.0.0.1" + addr + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "not serving: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "serving, but /healthz answered %s\n", resp.Status)
		return 1
	}
	return 0
}

func run(log zerolog.Logger) error {
	cfg := loadConfig()

	root, err := projectFile(cfg.ComposeSubdir)
	if err != nil {
		return err
	}
	// Before anything else: a queue that cannot read the project it is
	// deciding about should say so now, not when the first notice arrives
	// and is rejected for reasons nobody can see.
	if _, err := compose.ContainerIndex(root); err != nil {
		return fmt.Errorf("reading the compose project at %s: %w", root, err)
	}

	// Restored from /data, so a restart is not a decision about what is
	// waiting. Without a volume the path is still /data and load finds
	// nothing there, which is the same queue this had before.
	queue := q.Load(stateFile, func(err error) {
		log.Error().Msgf("the queue could not be kept across a restart: %v", err)
	})
	if n := queue.Len(); n > 0 {
		log.Info().Msgf("%d entr(ies) were still waiting from before the restart", n)
	}

	// The concrete one, so a notifier can be attached to it below: the
	// Applier interface deliberately says nothing about failures -- the
	// handler does not need to know -- but the thing being notified does.
	var live *applier
	var applying q.Applier
	if cfg.ActorURL != "" {
		live = newApplier(
			&update.Client{BaseURL: cfg.ActorURL, Token: cfg.ActorToken},
			cfg.ApplyTimeout,
			log,
		)
		applying = live
		log.Info().Msgf("applying through the actor at %s", cfg.ActorURL)
	} else {
		// Legitimate, not broken: a queue with no updater queues everything
		// and applies nothing, which is what someone running it to watch
		// rather than to act would want.
		log.Info().Msg("no DUVA_QUEUE_UPDATE_URL: every decision will be queued and nothing applied")
	}

	handler := &q.Handler{
		Lookup:  q.Lookup{Root: root},
		Queue:   queue,
		Applier: applying,
		Log:     log,
	}
	// Nil when no endpoint is configured, and then nobody is told. Left as a
	// nil Announce rather than a no-op function, so the handler's own guard
	// is what decides and there is one answer to "is anyone notified".
	if n := newNtfy(log); n != nil {
		handler.Announce = func(e q.Entry) { n.announce(cfg.Host, e, queued, "") }
		// A successful auto-apply says nothing: it is the policy working as
		// configured. A failed one may have left the container down and the
		// repository dirty, which wants a person.
		if live != nil {
			live.onFailure = func(e q.Entry, reason string) {
				n.announce(cfg.Host, e, failedApply, reason)
			}
		}
	}

	srv := &http.Server{
		Addr: addr,
		Handler: (&q.Server{
			Queue:   queue,
			Applier: applying,
			Host:    cfg.Host,
			Version: version,
			Token:   cfg.Token,
			Notify:  notifyHandler(handler, log),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info().Msgf("deciding for %s, serving on %s", cfg.Host, addr)
		if os.Getenv("DUVA_QUEUE_TOKEN") == "" {
			// Printed, because a token nobody can read is a queue nobody
			// can approve through. It is in the container's log rather than
			// anywhere durable on purpose: it lasts as long as this process
			// does, and the next restart mints another.
			log.Warn().Msgf("no DUVA_QUEUE_TOKEN was given, so one was minted: %s", cfg.Token)
			log.Warn().Msg("it changes on every restart — set DUVA_QUEUE_TOKEN to keep it stable")
		}
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

// notifyHandler wires the watcher's payload to the gate.
//
// The translation is the only part that knows which watcher is in use, which
// is why it is one function and not spread through the queue.
func notifyHandler(h *q.Handler, log zerolog.Logger) func([]byte) (int, string) {
	return func(body []byte) (int, string) {
		e, err := watchevent.Parse(body)
		if err != nil {
			log.Warn().Msgf("a notification could not be read: %v", err)
			return http.StatusBadRequest, err.Error()
		}
		n, why, ok := watchevent.Translate(e)
		if !ok {
			// Unusable is not a failure of the sender: saying what was
			// missing is more useful than a status code.
			return http.StatusOK, why
		}
		res := h.Handle(n)
		return res.Status, res.Message
	}
}

type config struct {
	Host          string
	Token         string
	ActorURL      string
	ActorToken    string
	ApplyTimeout  time.Duration
	ComposeSubdir string
}

func loadConfig() config {
	c := config{
		Host:          os.Getenv("DUVA_QUEUE_HOST"),
		Token:         tokenFromEnv(),
		ActorURL:      os.Getenv("DUVA_QUEUE_UPDATE_URL"),
		ActorToken:    os.Getenv("DUVA_QUEUE_UPDATE_TOKEN"),
		ComposeSubdir: os.Getenv("DUVA_QUEUE_COMPOSE_SUBDIR"),
		ApplyTimeout:  defaultApplyTimeout,
	}
	if c.Host == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			c.Host = h
		} else {
			c.Host = "unknown-host"
		}
	}
	if raw := os.Getenv("DUVA_QUEUE_UPDATE_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			// A timeout that does not parse is not one to fall back from:
			// the fallback would be silently different from what was asked
			// for, and nobody would know until an apply hung.
			fmt.Fprintf(os.Stderr, "Error: DUVA_QUEUE_UPDATE_TIMEOUT=%q is not a duration\n", raw)
			os.Exit(1)
		}
		c.ApplyTimeout = d
	}
	return c
}

// projectFile is the compose file to read, honouring the subdirectory.
func projectFile(sub string) (string, error) {
	dir := composeDir
	if sub != "" {
		if filepath.IsAbs(sub) {
			return "", fmt.Errorf("DUVA_QUEUE_COMPOSE_SUBDIR must be relative to %s, got %q", composeDir, sub)
		}
		joined := filepath.Join(composeDir, sub)
		// Join cleans ".." away rather than erroring, so a value trying to
		// escape would otherwise resolve quietly to somewhere else.
		rel, err := filepath.Rel(composeDir, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("DUVA_QUEUE_COMPOSE_SUBDIR %q escapes %s", sub, composeDir)
		}
		dir = joined
	}
	return compose.FindFile(dir)
}
