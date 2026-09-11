// decider is the gate.
//
// It receives a detector's webhook, works out what this host declares it is
// running, decides whether that is an update worth making and whether policy
// allows making it unattended, and either queues it for a person or hands it
// to whatever applies.
//
// It talks to a registry never, and to the docker daemon never. Everything it
// knows about this host comes from the compose file mounted at /compose, and
// everything it does about it goes to an actor it reaches by name.
//
//	/compose   the compose project directory, read-only
//
// Configuration:
//
//	DECIDER_HOST         what this decider calls itself, for a UI over several
//	DECIDER_TOKEN        the bearer token a UI must present
//	DECIDER_ACTOR_URL    where the actor is, e.g. http://actor:8080
//	DECIDER_ACTOR_TOKEN  the token the actor requires
//	DECIDER_APPLY_TIMEOUT how long an apply may run before it is called failed
//	DECIDER_COMPOSE_SUBDIR  where the project lives within /compose
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
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/actor"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/decide"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/detectevent"
)

var version = "dev"

// addr is where the decider serves. Fixed rather than configurable: inside a
// container the port is invisible to the operator, who chooses what to publish
// it as.
const addr = ":8080"

// composeDir is the contract with whoever runs this.
var composeDir = "/compose"

// defaultApplyTimeout bounds an apply.
//
// It is the only thing the decider has to believe about work it did not do:
// an actor that died mid-apply would otherwise hold its entry out of the queue
// forever, invisible to the person who could have noticed.
const defaultApplyTimeout = 30 * time.Minute

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("decider", version)
			return
		default:
			fmt.Fprintln(os.Stderr, "Usage: decider [version]")
			os.Exit(1)
		}
	}
	log := newLogger(os.Getenv("DECIDER_LOG_LEVEL"))
	if err := run(log); err != nil {
		log.Error().Msgf("%v", err)
		os.Exit(1)
	}
}

func run(log zerolog.Logger) error {
	cfg := loadConfig()

	root, err := projectFile(cfg.ComposeSubdir)
	if err != nil {
		return err
	}
	// Before anything else: a decider that cannot read the project it is
	// deciding about should say so now, not when the first notice arrives
	// and is rejected for reasons nobody can see.
	if _, err := compose.ContainerIndex(root); err != nil {
		return fmt.Errorf("reading the compose project at %s: %w", root, err)
	}

	queue := decide.NewPending()

	var applier decide.Applier
	if cfg.ActorURL != "" {
		applier = newApplier(
			&actor.Client{BaseURL: cfg.ActorURL, Token: cfg.ActorToken},
			cfg.ApplyTimeout,
			log,
		)
		log.Info().Msgf("applying through the actor at %s", cfg.ActorURL)
	} else {
		// Legitimate, not broken: a decider with no actor queues everything
		// and applies nothing, which is what someone running it to watch
		// rather than to act would want.
		log.Info().Msg("no DECIDER_ACTOR_URL: every decision will be queued and nothing applied")
	}

	handler := &decide.Handler{
		Lookup:  decide.Lookup{Root: root},
		Queue:   queue,
		Applier: applier,
		Log:     log,
	}

	srv := &http.Server{
		Addr: addr,
		Handler: (&decide.Server{
			Queue:   queue,
			Applier: applier,
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
		if os.Getenv("DECIDER_TOKEN") == "" {
			// Printed, because a token nobody can read is a decider nobody
			// can approve through. It is in the container's log rather than
			// anywhere durable on purpose: it lasts as long as this process
			// does, and the next restart mints another.
			log.Warn().Msgf("no DECIDER_TOKEN was given, so one was minted: %s", cfg.Token)
			log.Warn().Msg("it changes on every restart — set DECIDER_TOKEN to keep it stable")
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

// notifyHandler wires the detector's payload to the gate.
//
// The translation is the only part that knows which detector is in use, which
// is why it is one function and not spread through the decider.
func notifyHandler(h *decide.Handler, log zerolog.Logger) func([]byte) (int, string) {
	return func(body []byte) (int, string) {
		e, err := detectevent.Parse(body)
		if err != nil {
			log.Warn().Msgf("a notification could not be read: %v", err)
			return http.StatusBadRequest, err.Error()
		}
		n, why, ok := detectevent.Translate(e)
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
		Host:          os.Getenv("DECIDER_HOST"),
		Token:         tokenFromEnv(),
		ActorURL:      os.Getenv("DECIDER_ACTOR_URL"),
		ActorToken:    os.Getenv("DECIDER_ACTOR_TOKEN"),
		ComposeSubdir: os.Getenv("DECIDER_COMPOSE_SUBDIR"),
		ApplyTimeout:  defaultApplyTimeout,
	}
	if c.Host == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			c.Host = h
		} else {
			c.Host = "unknown-host"
		}
	}
	if raw := os.Getenv("DECIDER_APPLY_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			// A timeout that does not parse is not one to fall back from:
			// the fallback would be silently different from what was asked
			// for, and nobody would know until an apply hung.
			fmt.Fprintf(os.Stderr, "Error: DECIDER_APPLY_TIMEOUT=%q is not a duration\n", raw)
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
			return "", fmt.Errorf("DECIDER_COMPOSE_SUBDIR must be relative to %s, got %q", composeDir, sub)
		}
		joined := filepath.Join(composeDir, sub)
		// Join cleans ".." away rather than erroring, so a value trying to
		// escape would otherwise resolve quietly to somewhere else.
		rel, err := filepath.Rel(composeDir, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("DECIDER_COMPOSE_SUBDIR %q escapes %s", sub, composeDir)
		}
		dir = joined
	}
	return compose.FindFile(dir)
}
