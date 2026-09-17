package queue

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
	// run a queue, not a broken one.
	Applier Applier
	// Announce tells someone a candidate is waiting. Nil means nobody is
	// told, which is how a queue runs with no notifier configured.
	//
	// It is called only where enqueue reports the entry was new. The watcher
	// does not re-report on a schedule, but its outbox drops a finding only
	// when the POST succeeded -- so one received while the response was lost
	// is re-sent next run, and would otherwise notify twice about one queued
	// update. Errors are the announcer's to log: failing to tell someone is
	// not failing to queue, and must not change what the watcher is told.
	Announce func(Entry)
	// Applicable reports whether a service should be acted on at all,
	// independently of what the change is. Nil means every service is.
	//
	// The case it exists for is a container somebody stopped: the compose file
	// still declares it, so a finding still arrives and still sizes correctly,
	// but there is nothing to update -- and applying would recreate the
	// container, which starts it. A finding for such a service is neither
	// queued nor announced, so nothing appears on the page and nobody is
	// notified about a service that is not running.
	//
	// It is asked on every notice rather than remembered, so a service that
	// comes back is offered what accumulated while it was down: the first
	// notice after it starts enqueues normally and announces then.
	Applicable func(service string) (bool, string)
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
		// happened. The watcher is told, and so is the log.
		h.logf("could not look up %s: %v", n.Container, err)
		return Result{Outcome: Reject, Message: err.Error(), Status: http.StatusInternalServerError}
	}

	// Asked before the verdict is acted on, but after the lookup: a notice
	// naming nothing must still be rejected loudly, which is a fact about the
	// compose file rather than about a container.
	if h.Applicable != nil && found {
		if ok, why := h.Applicable(svc.Name); !ok {
			// Ignore, not Reject: nothing is wrong. The service exists, the
			// finding is real, and it is simply not actionable right now --
			// the same shape of answer as "already on it".
			h.logf("%s: not acted on — %s", svc.Name, why)
			return Result{Outcome: Ignore, Service: svc.Name, Message: why, Status: http.StatusOK}
		}
	}

	v := Decide(n, svc, found)

	switch v.Outcome {
	case Reject:
		// Loud rather than dropped: a notice naming a container nothing
		// declares means the watcher and the compose file disagree about
		// what runs here, and a silently ignored one looks exactly like
		// nothing having happened.
		h.logf("rejected: %s", v.Why)
		return Result{Outcome: Reject, Message: v.Why, Status: http.StatusUnprocessableEntity}

	case Ignore:
		return Result{Outcome: Ignore, Service: svc.Name, Message: v.Why, Status: http.StatusOK}

	case Apply:
		e := EntryFrom(v, svc, digestFor(v, n))
		if h.Applier == nil {
			// No updater configured: queue it rather than lose it. A queue
			// that decided to apply and then dropped the decision would be
			// worse than one that never decided.
			h.enqueue(e)
			h.logf("%s: %s -> %s would apply (%s), but nothing is configured to apply it", svc.Name, v.From, v.To, v.Why)
			return Result{Outcome: Queue, Service: svc.Name, Message: "queued: nothing is configured to apply it", Status: http.StatusOK}
		}
		if err := h.Applier.Start(e); err != nil {
			// Failing to start is not failing to queue. It stays queued, so
			// the next notice tries again and a person can see it waiting.
			h.enqueue(e)
			h.logf("%s: could not start applying: %v", svc.Name, err)
			return Result{Outcome: Queue, Service: svc.Name, Message: err.Error(), Status: http.StatusOK}
		}
		// Applying is not "applied". The updater says when it is done, and the
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
// from the same one arriving again, so the announcement rides on that rather
// than on a second notion of newness that could disagree with the queue.
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
// A digest move knows it: the watcher said what the tag now points at. A
// version change does not -- the digest for a tag nobody has pulled is the
// updater's to resolve, and guessing it here would be the queue doing the
// updater's job with less information.
func digestFor(v Verdict, n Notice) string {
	if v.Kind == "" {
		return n.Digest
	}
	return ""
}
