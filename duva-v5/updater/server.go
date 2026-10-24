package updater

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/update"
)

// linger is how long a finished stream stays readable.
//
// Long enough for a caller that connects after the work ended to still learn
// how it went; short enough that nothing accumulates. It is not a retention
// policy -- what happened is in the container log and the git history.
const linger = 60 * time.Second

// Server is the updater: it takes work, one apply per service at a time, and
// lets it be watched. The queue in the same process reaches it through
// Local; nothing reaches it over a network.
type Server struct {
	// Apply carries out one request, writing its progress to step. It
	// returns the terminal status and a reason for a failure.
	//
	// A function rather than a method so the transaction can be swapped for
	// a fake in tests without faking the daemon and a git repository.
	Apply func(r update.Request, step func(string, ...any)) (update.Status, string)
	// Ready reports whether work offered now would be attempted, and why not.
	//
	// Injected like Apply, and for the same reason: what makes this updater
	// ready is a git working tree, which a test should not have to build to
	// exercise it. Nil means always ready -- an updater that cannot
	// tell should not claim to be broken.
	Ready func() update.Readiness
	// Applicable reports whether this updater would act on one service.
	//
	// Injected for the same reason as Ready: the answer here is a docker
	// daemon, which a test should not have to run. Nil means every service is
	// applicable -- an updater that cannot tell must not refuse work.
	Applicable func(service string) update.Applicable
	// IsSelf reports whether a service is this updater's own container. Nil
	// means none is: the updater is then never asked to replace itself.
	IsSelf func(service string) bool
	// HandOff starts the helper that replaces this updater, for a request
	// naming it. See selfupdate.go.
	HandOff func(r update.Request) (Handoff, error)
	// Version is the build.
	Version string
	// Log records what happened, for an operator reading the container's
	// output rather than watching a stream. The zero value discards.
	Log zerolog.Logger

	mu sync.Mutex
	// running is the work in flight, keyed by service. One at a time per
	// service; the queue already serialises across them, but an updater that
	// relied on its caller for that would be one that could be broken by a
	// different caller.
	running map[string]*stream
	// handedOff names the helper replacing this updater, while one is. Held
	// in memory only: the process that holds it is the one being replaced,
	// and its successor starts without it.
	handedOff string
}

// work is what an accepted request runs: the transaction, or an answer that
// needs none.
type work func(update.Request, func(string, ...any)) (update.Status, string)

// answer is work that ends at once with a status, for a request that was taken
// but cannot be carried out now.
func answer(status update.Status, reason string) work {
	return func(update.Request, func(string, ...any)) (update.Status, string) {
		return status, reason
	}
}

// Start begins applying a request and says where to watch it. It is what
// the HTTP handler does after reading the request, and what a caller in the
// same process uses instead -- one path, so the two cannot answer the same
// request differently.
//
// An error means nothing was started: the service already has an apply in
// flight.
func (s *Server) Start(req update.Request) (update.Accepted, error) {
	// Asked before the lock: it is a question for the daemon, and holding the
	// lock across it would stall every stream and readiness check behind it.
	self := s.IsSelf != nil && s.IsSelf(req.Service)

	s.mu.Lock()
	if s.handedOff != "" {
		// Deferred, not refused: the helper is about to stop this process,
		// and work started here now would be killed partway. An unattended
		// apply is offered again later and reaches the replacement.
		helper := s.handedOff
		s.mu.Unlock()
		return s.begin(req, answer(update.Deferred,
			"this updater is being replaced by "+helper+"; will try again once it is back"))
	}
	if self {
		// Only from idle. The helper stops this container, and with it
		// whatever this container is in the middle of applying -- which,
		// between a stop and a create, is a service left removed.
		if busy := s.busyLocked(); busy != "" {
			s.mu.Unlock()
			return s.begin(req, answer(update.Deferred,
				"this updater is applying "+busy+", and replacing it now would interrupt that; will try again"))
		}
		// Claimed before the helper exists, so a request arriving while it
		// starts is deferred rather than racing it.
		s.handedOff = "the helper that is starting"
		s.mu.Unlock()
		return s.handOff(req)
	}
	s.mu.Unlock()

	return s.begin(req, s.Apply)
}

// busyLocked names a service whose work is still running, or "". The caller
// holds s.mu.
func (s *Server) busyLocked() string {
	for service, st := range s.running {
		if !st.finished() {
			return service
		}
	}
	return ""
}

// begin takes a request: one stream per service, and the work run against it.
func (s *Server) begin(req update.Request, do work) (update.Accepted, error) {
	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]*stream{}
	}
	if prev, busy := s.running[req.Service]; busy {
		// A finished stream still in its linger window is not busy: the work
		// is over, and refusing the next request because its log has not
		// been collected yet would make the window a rate limit.
		if !prev.finished() {
			s.mu.Unlock()
			return update.Accepted{}, fmt.Errorf("%s is already being updated", req.Service)
		}
	}
	st := newStream()
	s.running[req.Service] = st
	s.mu.Unlock()

	go s.run(req, st, do)

	return update.Accepted{Stream: "/v1/stream/" + req.Service}, nil
}

// handOff gives a request to update this updater to a helper, and answers with
// the helper's stream.
//
// The queue follows whatever stream it is given, so it watches the helper do
// the work and reads the helper's terminal line: completed means the helper did
// its job, which is what completed means from any updater.
func (s *Server) handOff(req update.Request) (update.Accepted, error) {
	h, err := s.HandOff(req)
	if err != nil {
		s.setHandedOff("")
		return s.begin(req, answer(update.Failed,
			"could not start the helper that replaces this updater: "+err.Error()))
	}
	s.setHandedOff(h.Name)
	s.logf("%s: handed to %s, which replaces this updater", req.Service, h.Name)

	// Not ready for exactly as long as the helper lives. If it replaces this
	// container, this process never sees the wait return. If it exits first
	// -- a failed pull, nobody watching -- this updater is still the one
	// running, and takes work again.
	go func() {
		if err := h.Wait(); err != nil {
			// Guessing would reopen the window this closes: staying not ready
			// is visible on the page, and a restart clears it.
			s.Log.Error().Msgf("lost track of %s (%v); not taking work until this updater is restarted", h.Name, err)
			return
		}
		s.setHandedOff("")
		s.logf("%s exited and this updater is still running: taking work again", h.Name)
	}()

	return update.Accepted{Stream: h.Stream}, nil
}

func (s *Server) setHandedOff(helper string) {
	s.mu.Lock()
	s.handedOff = helper
	s.mu.Unlock()
}

func (s *Server) run(req update.Request, st *stream, do work) {
	defer func() {
		// A panic in the transaction must still end the stream, or the
		// queue waits out its whole timeout on work that stopped.
		if p := recover(); p != nil {
			st.write("%s", update.Terminal(update.Failed, fmt.Sprintf("the actor panicked: %v", p)))
			s.logf("%s: panicked: %v", req.Service, p)
		}
		st.finish()
		// Kept for a short while after finishing, rather than deleted at
		// once. Work can end faster than the queue connects -- an updater
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
	status, reason := do(req, st.write)

	// The terminal line is the last thing on the stream, always: a queue
	// reading it is how the work is known to have ended, and an updater that
	// finished without saying so is indistinguishable from one that died.
	st.write("%s", update.Terminal(status, reason))
	switch {
	case status == update.Deferred:
		s.logf("%s: deferred: %s", req.Service, reason)
	case status != update.Completed:
		s.logf("%s: failed: %s", req.Service, reason)
	case reason != "":
		// Completed *with* a reason means the container is running the new
		// image and something after it did not work -- a commit that a hook
		// rejected, a push that did not land. The reason was dropped here
		// once, which is how bazarr's first real apply left a staged,
		// uncommitted change with no trace anywhere but a stream nobody kept.
		s.logf("%s: completed, but: %s", req.Service, reason)
	default:
		s.logf("%s: completed", req.Service)
	}
}

// serveStream relays one stream to one watcher, from its start, until it ends
// or the watcher leaves.
func serveStream(w http.ResponseWriter, r *http.Request, st *stream) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

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

// Readiness is whether work offered now would be attempted: the /v1/ready
// answer, for a caller in the same process.
func (s *Server) Readiness() update.Readiness {
	s.mu.Lock()
	helper := s.handedOff
	s.mu.Unlock()

	out := update.Readiness{Ready: true}
	switch {
	case helper != "":
		// Ahead of the repository's answer, which is clean -- and so "ready"
		// -- for most of a self-update.
		out = update.Readiness{Ready: false, Reason: "updating itself: " + helper + " is replacing this updater"}
	case s.Ready != nil:
		out = s.Ready()
	}
	return out
}
