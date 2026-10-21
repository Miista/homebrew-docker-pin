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
//	DUVA_UI_OIDC_ISSUER, _CLIENT_ID, _CLIENT_SECRET, _BASE_URL
//	                         sign in through an OIDC provider (all four
//	                         together); with none, a local account
//	DUVA_UI_AUTH_DISABLED    no sign-in at all, for a UI behind something else
//
// The sign-in gate keeps its state in /app/data/auth, so that directory must
// be a volume the container's user can write.
package webui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
	// The timezone database, embedded in the binary. Go reads TZ on its own
	// but resolves a name like Europe/Copenhagen against the host's
	// /usr/share/zoneinfo, which this image does not carry -- so TZ would be
	// silently ignored and every log line would stay UTC.
	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/ui"
	"github.com/miista/authy"
)

var Version = "dev"

// addr is where the UI serves. Fixed rather than configurable: inside a
// container the port is invisible to the operator, who chooses what to
// publish it as.
const addr = ":8080"

// Options is what a caller in the same process supplies.
type Options struct {
	// Local is the queue in this process, listed first. The rest come from
	// DUVA_QUEUES, which may then be empty: a UI over its own host alone is
	// still a UI over a queue.
	Local *ui.Upstream
	// Addr is where to listen. Empty is :8080.
	Addr string
}

// Run serves the page until ctx is done.
func Run(ctx context.Context, log zerolog.Logger, opts Options) error {
	listen := opts.Addr
	if listen == "" {
		listen = addr
	}
	queues, err := parseQueues(os.Getenv("DUVA_QUEUES"), tokenFromEnv)
	if err != nil {
		return fmt.Errorf("DUVA_QUEUES: %w", err)
	}
	if opts.Local != nil {
		queues = append([]ui.Upstream{*opts.Local}, queues...)
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

	// Sign-in is OIDC, always. A gate that is off, or a local account, is
	// a way into the button that replaces containers on an internet-facing
	// page, so neither is offered, and settings that ask for them are
	// refused rather than ignored.
	gate, err := newGate(ctx, os.Getenv)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	ap, err := approver(collector, log)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr: listen,
		Handler: newHandler(&ui.Server{
			Source:   collector,
			Approver: ap,
			Actions:  actionsFor(ap, collector),
			Actor:    signedIn,
			Version:  Version,
		}, gate),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		for _, d := range queues {
			if d.Local != nil {
				log.Info().Msgf("serving the queue on %s, in this process", d.Host)
				continue
			}
			if d.Token == "" {
				// Worth a line each: a queue that minted its own token
				// answers nothing without it, and the symptom is a host that
				// is simply always unreachable.
				log.Warn().Msgf("no token for %s — set %s to what that host's queue reads as DUVA_QUEUE_TOKEN",
					d.Host, tokenEnv(d.Host))
			}
			log.Info().Msgf("serving the queue on %s at %s", d.Host, d.URL)
		}
		log.Info().Msgf("listening on %s", listen)
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

// newHandler is the whole HTTP surface behind the sign-in gate. A nil gate
// (DUVA_UI_AUTH_DISABLED) leaves it unprotected. Only /healthz is public, so
// the container healthcheck keeps working without a session.
func newHandler(s *ui.Server, gate *authy.Gate) http.Handler {
	return gate.Wrap(s.Handler())
}

// approver returns what the Update button calls, or nil in read-only mode.
//
// Nil rather than a refusing implementation, because ui.Server leaves the
// endpoint unregistered when there is none: a UI meant only to report should
// not carry a route that triggers updates, even one that says no.
func approver(c *ui.Collector, log zerolog.Logger) (ui.Approver, error) {
	readOnly, err := settings.Bool(settings.ReadOnly, false)
	if err != nil {
		return nil, err
	}
	if !readOnly {
		return &loggedApprover{c: c, log: log}, nil
	}
	log.Info().Msg("read-only: the page will show the queue and nothing can be approved through it")
	return nil, nil
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

func (a *loggedApprover) Approve(key string, entry int64, actor string) error {
	host, service, _ := ui.SplitKey(key)
	if err := a.c.Approve(key, entry, actor); err != nil {
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

// actionsFor offers discard, block, check and rescan exactly when approving
// is offered: they carry the same authority over a host, so a read-only page
// has neither.
func actionsFor(ap ui.Approver, c *ui.Collector) ui.Actions {
	if ap == nil {
		return nil
	}
	return c
}
