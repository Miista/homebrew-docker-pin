package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/internal/agent"
	"github.com/Miista/homebrew-docker-pin/internal/hub"
	"github.com/Miista/homebrew-docker-pin/internal/ui"
)

// hubPoll is how often the hub re-asks its agents.
//
// Short, because it is a cheap read of state the agents already have -- not a
// registry check, which is what the agents' own schedules govern. The hub
// does no work on its own behalf; it is a view, and a view should not be
// minutes out of date when the thing it views is a click away from changing.
const hubPoll = 30 * time.Second

// serveHub runs duva as a hub: the page, over other hosts' agents.
//
// It holds no state, watches nothing, and needs neither the docker socket nor
// the compose files. Everything it shows came from an agent, and everything
// it does routes back to one.
func serveHub(cfg envConfig, log zerolog.Logger) error {
	if len(cfg.Agents) == 0 {
		// Refusing rather than serving an empty page: a hub with no agents
		// is always a misconfiguration, and an empty queue looks exactly
		// like "nothing needs updating" -- the one thing a queue must never
		// say when it does not know.
		return fmt.Errorf("DUVA_MODE=hub needs DUVA_AGENTS (host=url,...)")
	}

	var agents []hub.Agent
	for _, a := range cfg.Agents {
		agents = append(agents, hub.Agent{
			Host: a.Host,
			Client: &agent.Client{
				BaseURL: a.URL,
				// One token for all agents. Per-agent tokens would be
				// better and are a change to the config format, not to
				// this design; nothing here assumes they are the same.
				Token: cfg.AgentToken,
			},
		})
		log.Info().Msgf("agent %s at %s", a.Host, a.URL)
	}
	h := hub.New(agents)
	// Said on every collection, including the ones a page load triggers --
	// not only on the hub's own timer. A queue missing a host looks like
	// good news, so the reason it is missing has to be loud.
	h.OnUnreachable = func(host, err string) {
		log.Warn().Msgf("agent %s is unreachable: %s", host, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Once before serving, so the first page load is not empty.
	h.Collect()

	srv := &http.Server{
		Addr: uiAddr,
		Handler: (&ui.Server{
			Source:    h,
			Host:      hostLabel(cfg),
			Version:   version,
			Applier:   h,
			Refresher: h,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info().Msgf("the approval queue is on %s, over %d agents", uiAddr, len(agents))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error().Msgf("stopped serving: %v", err)
		}
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	ticker := time.NewTicker(hubPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("shutting down")
			return nil
		case <-ticker.C:
			h.Collect()
		}
	}
}
