// ui serves the approval queue over one or more queues.
//
// It holds no docker socket, reads no compose file and talks to no registry.
// Everything it shows it asked a queue for, and everything it does it asks a
// queue to do -- which is the point of it being its own process: the
// network-facing half needs none of the privilege the work needs.
//
// Configuration:
//
//	DUVA_QUEUES              the queues to serve, "host=url,host=url"
//	DUVA_QUEUE_TOKEN_<HOST>  what that host's queue reads as DUVA_QUEUE_TOKEN
//	DUVA_READ_ONLY           serve the page without the Update button
//	DUVA_LOG_LEVEL           how much to say
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	// The timezone database, embedded in the binary. Go reads TZ on its own
	// but resolves a name like Europe/Copenhagen against the host's
	// /usr/share/zoneinfo, which this image does not carry -- so TZ would be
	// silently ignored and every log line would stay UTC.
	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/ui"
)

var version = "dev"

// addr is where the UI serves. Fixed rather than configurable: inside a
// container the port is invisible to the operator, who chooses what to
// publish it as.
const addr = ":8080"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("duva-ui", version)
			return
		case "health":
			// For HEALTHCHECK. A subcommand rather than a port something
			// polls, because this image carries neither a shell nor curl.
			//
			// It reports whether this process is serving, not whether the
			// queues are reachable. An unreachable queue is the UI's
			// subject matter -- it renders as unreachable, which is the whole
			// point of the page -- and marking the UI unhealthy for it would
			// take down the one thing that can tell you about it.
			os.Exit(health())
		default:
			fmt.Fprintln(os.Stderr, "Usage: duva-ui [health|version]")
			os.Exit(1)
		}
	}
	log := newLogger(os.Getenv("DUVA_LOG_LEVEL"))
	if err := run(log); err != nil {
		log.Error().Msgf("%v", err)
		os.Exit(1)
	}
}

// health is the exit code for the health subcommand: 0 healthy, 1 not.
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
	queues, err := parseQueues(os.Getenv("DUVA_QUEUES"), tokenFromEnv)
	if err != nil {
		return fmt.Errorf("DUVA_QUEUES: %w", err)
	}
	// Refusing rather than serving an empty page: with no queues every
	// reload would say "nothing waiting for approval", which is the single
	// most misleading thing this program can display. It is also the exact
	// sentence it shows when everything is genuinely fine.
	if len(queues) == 0 {
		return fmt.Errorf("no queues: set DUVA_QUEUES to host=url[,host=url]")
	}

	collector := ui.NewCollector(queues)
	// On the collection rather than on a timer: someone loading the page is
	// exactly when a queue being down is worth saying out loud.
	collector.OnUnreachable = func(host, err string) {
		log.Warn().Msgf("could not reach the queue on %s, so its queue is not shown: %s", host, err)
	}

	srv := &http.Server{
		Addr: addr,
		Handler: (&ui.Server{
			Source:   collector,
			Approver: approver(collector, log),
			Version:  version,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		for _, d := range queues {
			if d.Token == "" {
				// Worth a line each: a queue that minted its own token
				// answers nothing without it, and the symptom is a host that
				// is simply always unreachable.
				log.Warn().Msgf("no token for %s — set %s to what that host's queue reads as DUVA_QUEUE_TOKEN",
					d.Host, tokenEnv(d.Host))
			}
			log.Info().Msgf("serving the queue on %s at %s", d.Host, d.URL)
		}
		log.Info().Msgf("listening on %s", addr)
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

// approver returns what the Update button calls, or nil in read-only mode.
//
// Nil rather than a refusing implementation, because ui.Server leaves the
// endpoint unregistered when there is none: a UI meant only to report should
// not carry a route that triggers updates, even one that says no.
func approver(c *ui.Collector, log zerolog.Logger) ui.Approver {
	if !boolEnv("DUVA_READ_ONLY", false) {
		return &loggedApprover{c: c, log: log}
	}
	log.Info().Msg("read-only: the page will show the queue and nothing can be approved through it")
	return nil
}

// loggedApprover records who was approved before asking the queue to do it.
//
// The page shows the result, but the page is a browser tab somebody closes.
// An approval replaces a container on another host, which is worth a line in
// a log that outlives the click.
type loggedApprover struct {
	c   *ui.Collector
	log zerolog.Logger
}

func (a *loggedApprover) Approve(key string) error {
	host, service, _ := ui.SplitKey(key)
	if err := a.c.Approve(key); err != nil {
		a.log.Error().Msgf("approving %s on %s failed: %v", service, host, err)
		return err
	}
	a.log.Info().Msgf("approved %s on %s", service, host)
	return nil
}

// boolEnv reads a boolean environment variable, treating anything unset as
// the given default and anything unrecognised as a hard error: a typo
// silently meaning "no" would leave the Update button live on a UI someone
// believed was read-only.
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
