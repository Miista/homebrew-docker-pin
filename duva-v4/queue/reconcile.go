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
// agrees with -- see Pending.Reconcile's comment for why this exists.
type reconciler struct {
	queue    *q.Pending
	root     string
	interval time.Duration
	log      zerolog.Logger
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
