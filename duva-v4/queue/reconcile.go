package main

import (
	"fmt"
	"time"

	"github.com/rs/zerolog"

	q "github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
)

// defaultReconcileInterval is how often the queue re-checks what it is
// waiting on against the compose file.
//
// Cheap enough to run often: no registry, no docker socket, one file read per
// entry. Not so often that a person watching logs sees noise -- an entry
// satisfied by hand is not urgent to clear, since it is already done.
const defaultReconcileInterval = 10 * time.Minute

// recheck answers Pending.Reconcile's question for one entry: does it still
// apply to the service as the compose file declares it now? See
// q.RecheckEntry for what that means.
//
// root is the project's own compose file, read fresh every call so a service
// moved to a renamed or restructured include: is still found. e.File is not
// trusted for this: it is where the service was declared when the entry was
// queued, and nothing keeps it in step with the tree afterward -- a file
// rename orphans every entry pointing at the old path, silently and
// permanently, since nothing else ever re-reads File once an entry is
// queued. A stale File already did this twice in practice (duva-v4/v7.2 and
// v7.5's deploys both found entries from before a rename or an old path
// layout, unreconcilable and warning on every tick).
func recheck(root string, e q.Entry) (q.Recheck, error) {
	svc, found, err := q.Lookup{Root: root}.ByService(e.Service)
	if err != nil {
		return q.Recheck{}, fmt.Errorf("reading the compose project: %w", err)
	}
	if !found {
		// Renamed, removed, or lost its container_name. Any of those is a
		// real question for a person, not evidence the update already
		// happened, so this must not read as "no longer applies" (which
		// Reconcile would then drop). An error keeps it queued instead.
		return q.Recheck{}, fmt.Errorf("no service %q (with a container_name) in the compose project any more", e.Service)
	}
	return q.RecheckEntry(e, svc), nil
}

// reconciler periodically drops queue entries the compose file no longer
// agrees with -- see Pending.Reconcile's comment for why this exists -- and
// releases the ones whose soak has run out.
//
// One loop for both, because both are the queue re-asking a question on a
// clock, and releasing right after reconciling means nothing is released that
// the compose file has already moved past. The interval is therefore also how
// late a release can be, which against a soak measured in days is noise.
type reconciler struct {
	queue    *q.Pending
	root     string
	interval time.Duration
	log      zerolog.Logger

	// applier is what a released entry is handed to. Nil releases nothing:
	// a queue with no updater never set a soak, since there was nothing to
	// apply unattended.
	applier q.Applier
	// applicable is asked before releasing, for the same reason the handler
	// asks it: a container stopped while its update soaked must not be
	// started by applying it. Nil means every service is.
	applicable func(service string) (bool, string)
	// now is the clock, injected so a test can pin it.
	now func() time.Time
}

// run reconciles once per tick until ctx is done.
//
// Once at the start too: a queue that restarts with something stale in
// /data.queue.json (already fixed while it was down) should not wait a full
// interval to say so.
func (r *reconciler) run(done <-chan struct{}) {
	r.tick()
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.tick()
		case <-done:
			return
		}
	}
}

func (r *reconciler) tick() {
	r.reconcile()
	r.release()
}

func (r *reconciler) reconcile() {
	dropped := r.queue.Reconcile(
		func(e q.Entry) (q.Recheck, error) { return recheck(r.root, e) },
		func(service string, err error) {
			r.log.Warn().Msgf("%s: could not reconcile against the compose file: %v", service, err)
		})
	for _, d := range dropped {
		r.log.Info().Msgf("%s: %s -> %s no longer applies (%s), dropped from the queue",
			d.Entry.Service, d.Entry.From, d.Entry.To, d.Why)
	}
}

// release hands every entry whose soak has run out to the applier.
//
// The soak is cleared first, by Release: from then on the entry is an ordinary
// auto-apply, and what happens to it if the apply is deferred or fails is the
// applier's existing answer rather than a second one here.
func (r *reconciler) release() {
	if r.applier == nil {
		return
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	for _, due := range r.queue.Due(now) {
		if r.applicable != nil {
			if ok, why := r.applicable(due.Service); !ok {
				// Left soaking, so it is released the first tick after the
				// service is back -- the same as a notice for it would be.
				r.log.Debug().Msgf("%s: soak is over, but not released: %s", due.Service, why)
				continue
			}
		}
		e, ok := r.queue.Release(due.Service, now)
		if !ok {
			continue
		}
		if err := r.applier.StartUnattended(e); err != nil {
			// Released either way: it now waits where any auto-apply that
			// could not start waits, for a person or the next notice.
			r.log.Warn().Msgf("%s: soak is over, but could not start applying: %v", e.Service, err)
			continue
		}
		r.log.Info().Msgf("%s: soak is over, applying %s -> %s", e.Service, e.From, e.To)
	}
}
