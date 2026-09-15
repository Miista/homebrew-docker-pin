package decide

import (
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// Handler turns a notice into what happens next.
//
// The gate's whole job, assembled: look up what this host declares, decide,
// and either queue it for a person or hand it to whatever applies. Everything
// it does beyond that is reporting.
type Handler struct {
	// Lookup reads the compose project.
	Lookup Lookup
	// Queue is where decisions wait.
	Queue *Pending
	// Applier carries out what policy allows unattended. Nil means nothing
	// is ever applied without being approved -- which is a legitimate way to
	// run a decider, not a broken one.
	Applier Applier
	// Announce tells someone a candidate is waiting. Nil means nobody is
	// told, which is how a decider runs with no notifier configured.
	//
	// It is called only where enqueue reports the entry was new, so a
	// protocol that re-notifies every run does not notify a person every
	// run. Errors are the announcer's to log: failing to tell someone is
	// not failing to queue, and must not change what the detector is told.
	Announce func(Entry)
	// Now is the clock, injected so a caller can pin it.
	Now func() time.Time
	// Log records what was decided. The zero value discards, so a handler
	// built without one is quiet rather than broken.
	Log zerolog.Logger
}

// Result is what a handler did about one notice.
type Result struct {
	// Outcome is the verdict's, or Reject when the notice could not be
	// matched to anything.
	Outcome Outcome
	// Service is what it was about, empty when nothing matched.
	Service string
	// Message says what happened, in one clause.
	Message string
	// Status is the HTTP status a webhook caller should see.
	Status int
}

// Handle decides about one notice and acts on the decision.
func (h *Handler) Handle(n Notice) Result {
	svc, found, err := h.Lookup.Service(n.Container)
	if err != nil {
		// A compose file that cannot be read is not a reason to say nothing
		// happened. The detector is told, and so is the log.
		h.logf("could not look up %s: %v", n.Container, err)
		return Result{Outcome: Reject, Message: err.Error(), Status: http.StatusInternalServerError}
	}

	v := Decide(n, svc, found)

	switch v.Outcome {
	case Reject:
		// Loud rather than dropped: a notice naming a container nothing
		// declares means the detector and the compose file disagree about
		// what runs here, and a silently ignored one looks exactly like
		// nothing having happened.
		h.logf("rejected: %s", v.Why)
		return Result{Outcome: Reject, Message: v.Why, Status: http.StatusUnprocessableEntity}

	case Ignore:
		return Result{Outcome: Ignore, Service: svc.Name, Message: v.Why, Status: http.StatusOK}

	case Apply:
		e := EntryFrom(v, svc, digestFor(v, n))
		if h.Applier == nil {
			// No actor configured: queue it rather than lose it. A decider
			// that decided to apply and then dropped the decision would be
			// worse than one that never decided.
			h.enqueue(e)
			h.logf("%s: %s -> %s would apply (%s), but nothing is configured to apply it", svc.Name, v.From, v.To, v.Why)
			return Result{Outcome: Queue, Service: svc.Name, Message: "queued: nothing is configured to apply it", Status: http.StatusOK}
		}
		if err := h.Applier.Start(e); err != nil {
			// Failing to start is not failing to decide. It stays queued, so
			// the next notice tries again and a person can see it waiting.
			h.enqueue(e)
			h.logf("%s: could not start applying: %v", svc.Name, err)
			return Result{Outcome: Queue, Service: svc.Name, Message: err.Error(), Status: http.StatusOK}
		}
		// Applying is not "applied". The actor says when it is done, and the
		// entry stays out of the queue because it is no longer waiting on
		// anyone.
		h.logf("%s: applying %s -> %s (%s)", svc.Name, v.From, v.To, v.Why)
		return Result{Outcome: Apply, Service: svc.Name, Message: v.Why, Status: http.StatusOK}

	default: // Queue
		e := EntryFrom(v, svc, digestFor(v, n))
		if h.enqueue(e) {
			h.logf("%s: %s -> %s is available, waiting for approval (%s)", svc.Name, v.From, v.To, v.Why)
		}
		return Result{Outcome: Queue, Service: svc.Name, Message: v.Why, Status: http.StatusOK}
	}
}

// enqueue records an entry, reporting whether it was new.
//
// New is also when anyone is told. Put already distinguishes a fresh candidate
// from the same one arriving again -- which it does for exactly this reason --
// so the announcement rides on that rather than on a second notion of newness
// that could disagree with the queue.
func (h *Handler) enqueue(e Entry) bool {
	added := h.Queue.Put(e, h.now())
	if added && h.Announce != nil {
		h.Announce(e)
	}
	return added
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// logf records at warn: everything the handler says is either a decision
// waiting on someone or a reason it could not make one, and both are things
// an operator reading `docker logs` is looking for.
func (h *Handler) logf(format string, args ...any) {
	h.Log.Warn().Msgf(format, args...)
}

// digestFor is the digest to end up on, when it is known.
//
// A digest move knows it: the detector said what the tag now points at. A
// version change does not -- the digest for a tag nobody has pulled is the
// actor's to resolve, and guessing it here would be the decider doing the
// actor's job with less information.
func digestFor(v Verdict, n Notice) string {
	if v.Kind == "" {
		return n.Digest
	}
	return ""
}
