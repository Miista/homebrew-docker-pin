package decide

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Applier is whatever carries out a decision.
//
// An interface, not the actor client, so the decider can be tested without
// one and so a caller can wrap it -- with a lock, with logging, with nothing.
// The decider does not know what applying means; see internal/actor.
type Applier interface {
	// Start begins applying an entry. An error means it never began.
	Start(e Entry) error
	// Stream writes an in-flight apply's progress to w, for as long as it
	// runs. It writes nothing when the service has nothing in progress,
	// which is the whole of its error handling.
	Stream(service string, w http.ResponseWriter, r *http.Request)
}

// Server is the decider's HTTP surface.
//
// Two audiences, and they are the reason the endpoints are split the way they
// are: a detector on this host telling it something happened, and a UI --
// possibly on another host -- reading the queue and approving from it.
type Server struct {
	// Queue is what is waiting.
	Queue *Pending
	// Notify handles a detector's webhook: it translates, decides, and
	// either queues or applies. Supplied rather than built here because the
	// translation belongs to whichever detector is in use.
	Notify func(body []byte) (status int, message string)
	// Applier carries out an approval.
	Applier Applier
	// Host is what this decider calls itself, for a UI showing several.
	Host string
	// Version is the build.
	Version string
	// Token guards the endpoints a UI uses. Empty leaves them open.
	Token string
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Open: the thing most likely to call it is a container runtime, which
	// carries no credential.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	// Open, because a detector reaches this over the host's own network by
	// container name and nothing else has a path to it. It is still exposed
	// on the published port, which is a judgement about a LAN rather than a
	// principle -- see the duva-v4 design note. What it can do is bounded:
	// an unknown container is refused, and a known one still has to clear
	// the policy on it.
	mux.HandleFunc("/v1/notify", s.notify)

	mux.HandleFunc("/v1/snapshot", s.guard(s.snapshot))
	mux.HandleFunc("/v1/apply/", s.guard(s.apply))
	mux.HandleFunc("/v1/stream/", s.guard(s.stream))
	return mux
}

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

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	status, message := s.Notify(body)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// Snapshot is what a UI renders.
type Snapshot struct {
	Host    string  `json:"host"`
	Version string  `json:"version"`
	Pending []Entry `json:"pending"`
	// CanApply is whether an update approved now would actually be attempted.
	//
	// Not merely "an actor is configured". An actor that is there and would
	// refuse the work -- this one refuses on an uncommitted repository,
	// because applying commits -- is the same to a person clicking the
	// button: they have already decided by the time they find out. Reported
	// so a UI can grey it beforehand.
	CanApply bool `json:"can_apply"`
	// WhyNot is what to say when CanApply is false, in the actor's own words.
	//
	// Relayed, not composed: why an actor would refuse is its business, and
	// a decider that wrote this sentence itself would be a decider that had
	// learned what applying involves.
	WhyNot string `json:"why_not,omitempty"`
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	canApply, whyNot := s.actorReadiness()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Snapshot{
		Host:     s.Host,
		Version:  s.Version,
		Pending:  s.Queue.List(),
		CanApply: canApply,
		WhyNot:   whyNot,
	})
}

// apply approves a queued entry.
//
// A refusal is 200 with a message rather than a 4xx: "nothing queued under
// that name" is an answer about the queue's state, which a UI renders to a
// person. An HTTP error would make its client report a transport failure for
// something that is not one.
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

	// No actor is a configuration, not a fault: a decider without one queues
	// every decision and applies nothing, which is how this runs before an
	// actor is trusted with the socket. Said plainly, and before the queue is
	// touched -- an entry must not leave the queue for an apply that cannot
	// happen.
	if s.Applier == nil {
		writeMessage(w, "no actor is configured, so nothing can be applied from here")
		return
	}

	e, ok := s.Queue.Get(service)
	if !ok {
		writeMessage(w, "nothing is queued for "+service)
		return
	}
	if err := s.Applier.Start(e); err != nil {
		writeMessage(w, err.Error())
		return
	}
	// Approved is no longer waiting. It leaves the queue now rather than when
	// the work finishes: the queue answers "what needs a decision", and this
	// one has had its decision. If the apply fails, the detector will say so
	// again and it comes back -- which is the protocol working rather than
	// state being lost.
	s.Queue.Remove(service)
	writeMessage(w, "")
}

// stream relays an in-flight apply's progress.
//
// The decider is a pipe here, not a participant: what the actor says is
// copied through without being interpreted, so the actor keeps owning the
// shape of its own progress.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	service := strings.TrimPrefix(r.URL.Path, "/v1/stream/")
	if service == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}
	if s.Applier == nil {
		http.Error(w, "no actor is configured, so nothing is ever in flight",
			http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.Applier.Stream(service, w, r)
}

func writeMessage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// readBody reads a webhook body, bounded.
//
// Bounded because this endpoint is open: a detector's notification is a few
// hundred bytes, and anything that is not one should not be allowed to make
// the decider allocate for it.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 64<<10))
}

// ReadinessReporter is an Applier that can say whether it would take work.
//
// Separate from Applier because not every one can answer -- and an Applier
// that cannot must stay usable rather than be excluded by the interface. The
// decider reports what it is told and adds nothing: why an actor is not ready
// is the actor's business, and the decider knowing would mean learning what
// applying involves.
type ReadinessReporter interface {
	Ready() (ready bool, reason string)
}

// actorReadiness asks the applier, when it can answer.
//
// No actor at all is not-ready with the reason already used elsewhere, so a
// page has one thing to render rather than two shapes of the same fact.
func (s *Server) actorReadiness() (bool, string) {
	if s.Applier == nil {
		return false, "no actor is configured, so nothing can be applied from here"
	}
	if rr, ok := s.Applier.(ReadinessReporter); ok {
		return rr.Ready()
	}
	return true, ""
}
