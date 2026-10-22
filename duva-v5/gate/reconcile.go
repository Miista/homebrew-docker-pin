package gate

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/compose"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/jobs"
	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
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
	return recheckWith(q.Lookup{Root: root}, e, false)
}

// recheckWith is recheck through a given Lookup, so policy is read from
// wherever the queue reads it.
//
// refile also fits a kept entry to a policy changed since it was decided (see
// q.Refile): only where something here applies, since a release time with
// nothing to release it would take the entry from a person for good.
func recheckWith(lookup q.Lookup, e q.Entry, refile bool) (q.Recheck, error) {
	svc, found, err := lookup.ByService(e.Service)
	if err != nil {
		return q.Recheck{}, fmt.Errorf("reading the compose project: %w", err)
	}
	if !found {
		// Renamed, removed, or lost its container_name. Not by itself
		// evidence the update happened, so not "no longer applies": the
		// queue drops it only once the service's container is gone too.
		// v4 kept such an entry for a person to discard; with the container
		// gone the service is decommissioned, and if it comes back it is
		// watched again from scratch, so there is nothing left to ask.
		return q.Recheck{}, fmt.Errorf("%w: no service %q (with a container_name) in the compose project any more",
			q.ErrNotDeclared, e.Service)
	}
	r := q.RecheckEntry(e, svc)
	if refile && r.Keep {
		// Compared with the policy the entry was last decided under, which
		// RecheckEntry has already replaced with the current one.
		was := r.Entry
		was.Auto = e.Auto
		r.Entry, r.Refiled = q.Refile(was, svc, time.Now())
	}
	return r, nil
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
	// labels reads policy from the container; nil reads the compose file.
	labels func(container string) (map[string]string, bool, error)
	// containers lists this host's containers, for the service sync. Nil
	// leaves the services table to grow as entries arrive.
	containers func() ([]q.Container, error)
	// seen and missing are the last sync's containers, and those of them
	// that had just gone missing; see graced.
	seen    []q.Container
	missing map[string]q.Container
	// poke asks for a tick now: a container started or stopped, and what is
	// waiting may have changed with it. Nil never asks.
	poke <-chan struct{}
	// now is the clock, injected so a test can pin it.
	now func() time.Time
	// st is where each job records its run. Nil records nothing.
	st *store.Store
	// last is when the last tick started.
	lastMu sync.Mutex
	last   time.Time
}

// next is when the next tick is due.
func (r *reconciler) next() time.Time {
	r.lastMu.Lock()
	defer r.lastMu.Unlock()
	if r.last.IsZero() {
		return time.Time{}
	}
	return r.last.Add(r.interval)
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
		case <-r.poke:
			r.log.Debug().Msg("a container changed; reconciling now")
			r.tick()
		case <-done:
			return
		}
	}
}

// tick is the three jobs, in the order each depends on the last.
//
//   - sync brings the world outside duva into its state: which services the
//     compose file declares, the image and tag each follows, and which have
//     gone (from the file and their container both).
//   - reconcile fits what is waiting to that state, dropping what no longer
//     applies.
//   - release applies the delayed releases that are due, of what is left.
func (r *reconciler) tick() {
	r.lastMu.Lock()
	r.last = time.Now()
	r.lastMu.Unlock()
	r.job(jobs.Sync, r.syncServices)
	r.job(jobs.Reconcile, r.reconcile)
	r.job(jobs.Release, r.release)
}

// job runs one of the tick's jobs and records the run: its summary, and what
// it logged while it ran, which is its output. A tick is one goroutine, so
// swapping the logger for the length of a job is safe.
func (r *reconciler) job(name string, fn func() (summary string, failed bool)) {
	started := time.Now()
	out := &jobs.Log{}
	saved := r.log
	r.log = saved.Hook(out)
	summary, failed := fn()
	r.log = saved
	jobs.Record(r.st, jobs.Run{Job: name, Started: started, Took: time.Since(started),
		Summary: summary, Output: out.Lines(), Failed: failed})
}

func (r *reconciler) reconcile() (string, bool) {
	// An entry being applied is the apply's until it ends: midway its compose
	// file is already rewritten, so a recheck would find the service "already
	// matching" and drop it -- recording a completed update as dropped, and
	// taking its row off the page before it reports how it went.
	busy := map[string]bool{}
	if ar, ok := r.applier.(q.ApplyingReporter); ok {
		for _, s := range ar.Applying() {
			busy[s] = true
		}
	}
	unread, waiting := 0, 0
	dropped, refiled := r.queue.Reconcile(
		func(e q.Entry) (q.Recheck, error) {
			if busy[e.Service] {
				return q.Recheck{Keep: true, Entry: e}, nil
			}
			return recheckWith(q.Lookup{Root: r.root, Labels: r.labels}, e, r.applier != nil)
		},
		func(service string, err error) {
			if errors.Is(err, q.ErrNoContainer) {
				// Kept as it is: nothing runs, so nothing can be applied,
				// and the next tick after its container is back asks again.
				waiting++
				r.log.Debug().Msgf("%s: kept, waiting on its container", service)
				return
			}
			unread++
			r.log.Warn().Msgf("%s: could not reconcile against the compose file: %v", service, err)
		})
	for _, d := range dropped {
		r.log.Info().Msgf("%s: %s -> %s no longer applies (%s), dropped from the queue",
			d.Entry.Service, d.Entry.From, d.Entry.To, d.Why)
	}
	for _, m := range refiled {
		r.log.Info().Msgf("%s: %s -> %s %s", m.Entry.Service, m.Entry.From, m.Entry.To, m.Refiled)
	}
	summary := fmt.Sprintf("%d dropped", len(dropped))
	if len(refiled) > 0 {
		summary += fmt.Sprintf(", %d refiled", len(refiled))
	}
	if waiting > 0 {
		summary += fmt.Sprintf(", %d waiting on a container", waiting)
	}
	if unread > 0 {
		summary += fmt.Sprintf(", %d could not be checked", unread)
	}
	return summary, unread > 0
}

// release hands every entry whose soak has run out to the applier.
//
// The soak is cleared first, by Release: from then on the entry is an ordinary
// auto-apply, and what happens to it if the apply is deferred or fails is the
// applier's existing answer rather than a second one here.
func (r *reconciler) release() (string, bool) {
	if r.applier == nil {
		return "nothing applies here", false
	}
	released, failed := 0, 0
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	for _, due := range r.queue.Due(now) {
		if r.applicable != nil {
			if ok, why := r.applicable(due.Service); !ok {
				// Left soaking, so it is released the first tick after the
				// service is back -- the same as a notice for it would be.
				r.log.Debug().Msgf("%s: delay is over, but not released: %s", due.Service, why)
				continue
			}
		}
		e, ok := r.queue.Release(due, now)
		if !ok {
			continue
		}
		if err := r.applier.StartUnattended(e); err != nil {
			// Released either way: it now waits where any auto-apply that
			// could not start waits, for a person or the next notice.
			r.log.Warn().Msgf("%s: delay is over, but could not start applying: %v", e.Service, err)
			failed++
			continue
		}
		released++
		r.log.Info().Msgf("%s: delay is over, applying %s -> %s", e.Service, e.From, e.To)
	}
	summary := fmt.Sprintf("%d released", released)
	if failed > 0 {
		summary += fmt.Sprintf(", %d could not start", failed)
	}
	return summary, failed > 0
}

// syncServices brings the services table up to date with the compose file
// and the containers: which services exist, what image and tag each follows,
// and which have gone. See q.Pending.SyncServices.
func (r *reconciler) syncServices() (string, bool) {
	if r.containers == nil {
		return "nothing to sync from", false
	}
	declared, err := declaredServices(r.root)
	if err != nil {
		// Not a reason to mark anything removed: a file that cannot be read
		// says nothing about what is declared in it.
		r.log.Warn().Msgf("could not read the compose project: %v", err)
		return "could not read the compose project", true
	}
	cs, err := r.containers()
	if err != nil {
		r.log.Warn().Msgf("could not list this host's containers: %v", err)
		return "could not list this host's containers", true
	}
	cs = r.graced(cs)
	added, removed, err := r.queue.SyncServices(declared, cs, time.Now())
	if err != nil {
		r.log.Error().Msgf("could not record this host's services: %v", err)
		return "could not record this host's services", true
	}
	for _, c := range added {
		r.log.Debug().Msgf("%s: a service duva knows", c)
	}
	for _, c := range removed {
		r.log.Info().Msgf("%s: gone from the compose file and its container gone; the service is disabled, its history kept", c)
	}
	return fmt.Sprintf("%d services; %d added, %d removed", len(declared), len(added), len(removed)), false
}

// declaredServices is every service in the compose project with a
// container_name, with the image and tag its file declares.
//
// A service whose image cannot be parsed is left out rather than failing
// the rest: it is still in the file, and a service left out is only removed
// once its container is gone too.
func declaredServices(root string) ([]q.Declared, error) {
	index, err := compose.ContainerIndex(root)
	if err != nil {
		return nil, err
	}
	out := make([]q.Declared, 0, len(index))
	for container, ref := range index {
		raw, err := compose.RawImage(ref.File, ref.Service)
		if err != nil || compose.HasUnexpandedVariable(raw) {
			continue
		}
		base, tag, err := compose.ParseImage(ref.File, ref.Service)
		if err != nil {
			continue
		}
		out = append(out, q.Declared{Container: container, Service: ref.Service, Image: base, Tag: tag})
	}
	return out, nil
}

// graced keeps a container that has just gone missing for one more sync.
//
// Gone means missing twice running. A recreate -- duva's own apply, or a
// compose up -- removes the old container before creating the new one, and
// a sync prompted by the removal lands in between: without this every apply
// would record the service as removed and then back again.
func (r *reconciler) graced(cs []q.Container) []q.Container {
	present := map[string]bool{}
	for _, c := range cs {
		present[c.Name] = true
	}
	missing := map[string]q.Container{}
	out := cs
	for _, c := range r.seen {
		if present[c.Name] {
			continue
		}
		if _, already := r.missing[c.Name]; already {
			continue // missing twice: let it go
		}
		missing[c.Name] = c
		out = append(out, c)
	}
	r.missing = missing
	r.seen = out
	return out
}
