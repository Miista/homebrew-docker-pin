package agent

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/ui"
)

// Server exposes one host's duva over JSON, for a hub to collect from.
//
// It is the same three interfaces the page is built on -- Source, Applier,
// Refresher -- with an HTTP encoding instead of an HTML one. Nothing here
// decides anything: an agent that refuses an update refuses it in the same
// place and for the same reason a single-process duva would, because it is
// the same code underneath.
type Server struct {
	// Source supplies the queue. Required.
	Source ui.Source
	// Host is what this agent calls itself, reported in every snapshot.
	Host string
	// Version is the agent's build.
	Version string

	// Applier enables /apply and /progress. Nil leaves them unregistered
	// rather than refusing -- an agent meant only to report should not have
	// an endpoint that applies, the same way the page does not get a button.
	Applier ui.Applier
	// Refresher enables /refresh and /refresh-progress, on the same terms.
	Refresher ui.Refresher

	// Token, when set, is required as a bearer token on every endpoint
	// except /healthz.
	//
	// An agent's /apply is the authority to pull an arbitrary image and
	// recreate a container as root on this host. Binding it to a LAN keeps
	// strangers out of it; it does not keep out everything already on that
	// LAN, which for a home network is most things. So the token is how the
	// hub proves it is the hub, and an agent without one is only safe on a
	// network where every host is trusted to root every other.
	Token string
}

// Handler returns the agent's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: a health check that needed a credential could not be
	// used by the thing most likely to need it -- the container runtime,
	// which has none. It reveals only that a duva is listening.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/v1/snapshot", s.guard(s.snapshot))
	if s.Applier != nil {
		mux.HandleFunc("/v1/apply/", s.guard(s.apply))
		mux.HandleFunc("/v1/progress/", s.guard(s.progress))
	}
	if s.Refresher != nil {
		mux.HandleFunc("/v1/refresh", s.guard(s.refresh))
		mux.HandleFunc("/v1/refresh-progress", s.guard(s.refreshProgress))
	}
	return mux
}

// guard rejects a request without the configured bearer token.
//
// Compared in constant time via subtle? No: the token is not a secret derived
// from user input being checked against a stored hash, it is a shared string
// on a LAN, and a timing oracle over a network that already carries the
// token in plaintext on every legitimate request is not the weak part. What
// matters is that it is checked at all, on every endpoint that does anything.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != s.Token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, Snapshot{
		Host:           s.Host,
		Version:        s.Version,
		Pending:        s.Source.Pending(),
		Soaking:        s.Source.Soaking(),
		LastCheck:      s.Source.LastCheck(),
		LastCheckExact: s.Source.LastCheckExact(),
	})
}

// apply starts an update. The service is the path suffix rather than a query
// parameter or a body: it identifies the thing being acted on, and a POST to
// /v1/apply with the name elsewhere reads like one endpoint doing many jobs.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	service := strings.TrimPrefix(r.URL.Path, "/v1/apply/")
	if service == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}
	// A refusal is 200 with an error in the body, not a 4xx: "nothing queued
	// under that name" and "one is already running" are answers about the
	// queue's state, which the hub renders to the operator. An HTTP error
	// status would make the hub's client report a transport failure for
	// something that is not one.
	if err := s.Applier.Start(service); err != nil {
		writeJSON(w, StartResponse{Error: err.Error()})
		return
	}
	writeJSON(w, StartResponse{})
}

func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	service := strings.TrimPrefix(r.URL.Path, "/v1/progress/")
	if service == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}
	p, ok := s.Applier.Progress(service)
	writeJSON(w, ProgressResponse{Known: ok, Progress: p})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.Refresher.Refresh(); err != nil {
		writeJSON(w, RefreshResponse{Error: err.Error()})
		return
	}
	writeJSON(w, RefreshResponse{})
}

func (s *Server) refreshProgress(w http.ResponseWriter, r *http.Request) {
	running, done, errMsg := s.Refresher.RefreshProgress()
	writeJSON(w, RefreshProgressResponse{Running: running, Done: done, Error: errMsg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
