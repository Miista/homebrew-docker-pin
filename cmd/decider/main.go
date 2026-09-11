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

	"github.com/Miista/homebrew-docker-pin/internal/actor"
	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/decide"
	"github.com/Miista/homebrew-docker-pin/internal/diun"
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
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
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
			logf,
		)
		logf("actor at %s", cfg.ActorURL)
	} else {
		// Legitimate, not broken: a decider with no actor queues everything
		// and applies nothing, which is what someone running it to watch
		// rather than to act would want.
		logf("no DECIDER_ACTOR_URL: everything will be queued and nothing applied")
	}

	handler := &decide.Handler{
		Lookup:  decide.Lookup{Root: root},
		Queue:   queue,
		Applier: applier,
		Log:     logf,
	}

	srv := &http.Server{
		Addr: addr,
		Handler: (&decide.Server{
			Queue:   queue,
			Applier: applier,
			Host:    cfg.Host,
			Version: version,
			Token:   cfg.Token,
			Notify:  notifyHandler(handler),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logf("deciding for %s, on %s", cfg.Host, addr)
		if cfg.Token == "" {
			logf("no DECIDER_TOKEN: anything that can reach this can approve an update")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("stopped serving: %v", err)
		}
	}()

	<-ctx.Done()
	logf("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// notifyHandler wires the detector's payload to the gate.
//
// The translation is the only part that knows which detector is in use, which
// is why it is one function and not spread through the decider.
func notifyHandler(h *decide.Handler) func([]byte) (int, string) {
	return func(body []byte) (int, string) {
		p, err := diun.Parse(body)
		if err != nil {
			logf("unreadable notification: %v", err)
			return http.StatusBadRequest, err.Error()
		}
		n, why, ok := diun.Translate(p)
		if !ok {
			// Not a signal is not a failure: a first sighting is the
			// detector recording a baseline, and saying so is more useful
			// than an error.
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
		Token:         os.Getenv("DECIDER_TOKEN"),
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

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05")+" "+format+"\n", args...)
}
