package main

import (
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/compose"
	q "github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
)

// defaultReconcileInterval is how often the queue re-checks what it is
// waiting on against the compose file.
//
// Cheap enough to run often: no registry, no docker socket, one file read per
// entry. Not so often that a person watching logs sees noise -- an entry
// satisfied by hand is not urgent to clear, since it is already done.
const defaultReconcileInterval = 10 * time.Minute

// stillWanted answers Pending.Reconcile's question for one entry: does the
// compose file still disagree with what this entry is waiting to apply?
//
// Reads the file fresh every call, not Lookup: Lookup is keyed by container
// name, for matching a watcher's notice, and an Entry already knows its own
// file and service name -- the thing this needs to re-read is narrower than
// what Lookup answers.
func stillWanted(e q.Entry) (bool, error) {
	raw, err := compose.RawImage(e.File, e.Service)
	if err != nil {
		return false, fmt.Errorf("reading %s's image: %w", e.Service, err)
	}

	// A digest move, or a version change whose digest the watcher already
	// resolved: satisfied the moment the file pins that digest, whatever the
	// tag says -- the digest is the record of what actually ran, and that is
	// the thing a manual fix changes first.
	if e.Digest != "" {
		return compose.DigestOf(raw) != e.Digest, nil
	}

	// A version change with no digest yet (the updater resolves it by
	// pulling the tag): the tag itself is the only thing to compare, and the
	// entry is satisfied once the file already follows the tag it wanted to
	// move to.
	return compose.TagOf(raw) != e.To, nil
}

// reconciler periodically drops queue entries the compose file no longer
// agrees with -- see Pending.Reconcile's comment for why this exists.
type reconciler struct {
	queue    *q.Pending
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
	removed := r.queue.Reconcile(stillWanted, func(service string, err error) {
		r.log.Warn().Msgf("%s: could not reconcile against the compose file: %v", service, err)
	})
	for _, service := range removed {
		r.log.Info().Msgf("%s: already matches the compose file, dropped from the queue", service)
	}
}
