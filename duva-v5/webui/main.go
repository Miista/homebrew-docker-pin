// webui serves the approval page over this host's queue and its peers'.
//
// It holds no docker socket, reads no compose file and talks to no registry.
// Everything it shows it asked a queue for, and everything it does it asks a
// queue to do.
//
// Configuration, through internal/settings (DUVA_SETTING_<KEY>):
//
//	peers              the other hosts, "host=url,host=url"
//	peer.<host>.token  what that host reads as its api_token
//	read_only          serve the page without the Update button
//	ui.oidc.issuer, .client_id, .client_secret, .base_url
//	                   sign in through an OIDC provider; all four required
//
// The sign-in gate keeps its state in /data/auth.
package webui

import (
	"context"
	"fmt"
	"net/http"
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
	// Local is the queue in this process, listed first. The rest are the
	// peers, which may then be none: a page over its own host alone is still
	// a page over a queue.
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
	queues, err := parseAgents(settings.Value(settings.Agents), agentToken)
	if err != nil {
		return fmt.Errorf("%s: %w", settings.EnvVar(settings.Agents), err)
	}
	if opts.Local != nil {
		queues = append([]ui.Upstream{*opts.Local}, queues...)
	}
	// Refusing rather than serving an empty page: with no queues every
	// reload would say "nothing waiting for approval", which is the single
	// most misleading thing this program can display. It is also the exact
	// sentence it shows when everything is genuinely fine.
	if len(queues) == 0 {
		return fmt.Errorf("no queues: set %s to host=url[,host=url]", settings.EnvVar(settings.Agents))
	}

	collector := ui.NewCollector(queues)
	// On the collection rather than on a timer: someone loading the page is
	// exactly when a queue being down is worth saying out loud.
	collector.OnUnreachable = func(host, err string) {
		log.Warn().Msgf("could not reach %s, so its queue is not shown: %s", host, err)
	}

	// Sign-in is OIDC, always. A gate that is off, or a local account, is
	// a way into the button that replaces containers on an internet-facing
	// page, so neither is offered, and settings that ask for them are
	// refused rather than ignored.
	gate, err := newGate(ctx, settings.Value)
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
				// Worth a line each: a peer answers nothing without it, and
				// the symptom is a host that is simply always unreachable.
				log.Warn().Msgf("no token for %s — set %s to that host's %s",
					d.Host, settings.EnvVar(settings.AgentToken(d.Host)), settings.EnvVar(settings.APIToken))
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

// newHandler is the whole HTTP surface behind the sign-in gate. Only /healthz is public, so
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

// actionsFor offers discard, block, check and rescan exactly when approving
// is offered: they carry the same authority over a host, so a read-only page
// has neither.
func actionsFor(ap ui.Approver, c *ui.Collector) ui.Actions {
	if ap == nil {
		return nil
	}
	return c
}
