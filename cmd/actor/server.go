package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/internal/actor"
)

// linger is how long a finished stream stays readable.
//
// Long enough for a caller that connects after the work ended to still learn
// how it went; short enough that nothing accumulates. It is not a retention
// policy -- what happened is in the container log and the git history.
const linger = 60 * time.Second

// Server is the actor's HTTP surface: take work, and let it be watched.
//
// Internal only. The decider calls it over the host's own network, and
// nothing outside has a path to it — which is why the token is about saying
// who is asking rather than hiding what is said.
type Server struct {
	// Apply carries out one request, writing its progress to step. It
	// returns the terminal status and a reason for a failure.
	//
	// A function rather than a method so the transaction can be swapped for
	// a fake in tests without faking the daemon and a git repository.
	Apply func(r actor.Request, step func(string, ...any)) (actor.Status, string)
	// Token, when set, is required on /v1/apply.
	Token string
	// Version is the build.
	Version string
	// Log records what happened, for an operator reading the container's
	// output rather than watching a stream. The zero value discards.
	Log zerolog.Logger

	mu sync.Mutex
	// running is the work in flight, keyed by service. One at a time per
	// service; the decider already serialises across them, but an actor that
	// relied on its caller for that would be one that could be broken by a
	// different caller.
	running map[string]*stream
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	s.running = map[string]*stream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/v1/apply", s.guard(s.apply))
	// Deliberately unguarded: it returns nothing when no work is running, so
	// there is no session to guess at and nothing to leak. Guarding it would
	// mean a token for reading a log that is about to be shown to whoever
	// asked for the update anyway.
	mux.HandleFunc("/v1/stream/", s.stream)
	return mux
}

func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != s.Token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req actor.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "unreadable request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Service == "" || req.File == "" {
		http.Error(w, "a request needs a service and a file", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if prev, busy := s.running[req.Service]; busy {
		// A finished stream still in its linger window is not busy: the work
		// is over, and refusing the next request because its log has not
		// been collected yet would make the window a rate limit.
		if !prev.finished() {
			s.mu.Unlock()
			http.Error(w, req.Service+" is already being updated", http.StatusConflict)
			return
		}
	}
	st := newStream()
	s.running[req.Service] = st
	s.mu.Unlock()

	go s.run(req, st)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(actor.Accepted{Stream: "/v1/stream/" + req.Service})
}

func (s *Server) run(req actor.Request, st *stream) {
	defer func() {
		// A panic in the transaction must still end the stream, or the
		// decider waits out its whole timeout on work that stopped.
		if p := recover(); p != nil {
			st.write(actor.Terminal(actor.Failed, fmt.Sprintf("the actor panicked: %v", p)))
			s.logf("%s: panicked: %v", req.Service, p)
		}
		st.finish()
		// Kept for a short while after finishing, rather than deleted at
		// once. Work can end faster than the decider connects -- an actor
		// that refused before pulling anything finishes in microseconds --
		// and a stream discarded that quickly means the caller watches
		// nothing and waits out its whole timeout on work that is already
		// done. A watcher arriving inside the window gets the backlog and
		// the terminal line, which is the whole of what it needed.
		go func() {
			time.Sleep(linger)
			s.mu.Lock()
			if s.running[req.Service] == st {
				delete(s.running, req.Service)
			}
			s.mu.Unlock()
		}()
	}()

	s.logf("%s: applying %s -> %s", req.Service, req.From, req.To)
	status, reason := s.Apply(req, st.write)

	// The terminal line is the last thing on the stream, always: a decider
	// reading it is how the work is known to have ended, and an actor that
	// finished without saying so is indistinguishable from one that died.
	st.write(actor.Terminal(status, reason))
	if status == actor.Completed {
		s.logf("%s: completed", req.Service)
	} else {
		s.logf("%s: failed: %s", req.Service, reason)
	}
}

// stream relays work in flight.
//
// Empty when nothing is running, which is the whole of its error handling:
// either work is happening and its log is what the watcher wanted, or there
// is nothing to say.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	service := strings.TrimPrefix(r.URL.Path, "/v1/stream/")
	if service == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	st, ok := s.running[service]
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		return
	}

	ch, backlog := st.subscribe()
	defer st.unsubscribe(ch)

	// What has already been said, so a watcher arriving mid-apply sees how
	// it got there rather than joining a conversation in progress.
	for _, line := range backlog {
		fmt.Fprintln(w, line)
	}
	flush(w)

	for {
		select {
		case line, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintln(w, line)
			flush(w)
		case <-r.Context().Done():
			// The watcher left. The work carries on without them.
			return
		}
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logf(format string, args ...any) {
	s.Log.Info().Msgf(format, args...)
}
