package queue

import (
	"sort"
	"sync"
	"time"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// Entry is one decision waiting to happen.
//
// Both kinds of waiting are the same entry. An update held back for a person
// to approve and one held back until a release is old enough differ only in
// what releases them — a click or a clock — and nothing else about them is
// different, so there is one queue rather than a queue and a soak list.
type Entry struct {
	// Service, File and Image identify what would change.
	Service string `json:"service"`
	File    string `json:"file"`
	Image   string `json:"image"`
	// From and To are the change, as the verdict described it.
	From string `json:"from"`
	To   string `json:"to"`
	// Tag is the tag to follow afterwards; Digest what to end up on, when
	// known.
	Tag    string `json:"tag"`
	Digest string `json:"digest,omitempty"`
	// Kind is how big the change is. Empty for a digest move, which has no
	// version pair.
	Kind version.Kind `json:"kind,omitempty"`
	// Why is the reason it is waiting, fit to render in a row.
	Why string `json:"why"`
	// Auto is the policy that produced this, so a queue shows the rule and
	// not only the verdict. A service is easiest to misconfigure in the
	// direction of applying more than intended, which is invisible if the
	// policy is only ever mentioned in the explanation of what it rejected.
	Auto Auto `json:"auto"`
	// FirstSeen is when this candidate first arrived, RFC 3339.
	FirstSeen string `json:"first_seen"`
	// ReleaseAt, when set, is when a clock rather than a person releases
	// this. Zero means it waits for someone.
	ReleaseAt string `json:"release_at,omitempty"`
}

// Pending is what is waiting, keyed by service.
//
// Keyed by service, not by candidate, and that is the whole of supersession:
// a newer candidate for a service replaces the pending one rather than
// queueing beside it, so it is impossible to approve something that has
// already been overtaken. A stale row invites approving something that is
// gone.
type Pending struct {
	mu      sync.RWMutex
	entries map[string]Entry
	// store is where this survives a restart. The zero value writes nowhere,
	// so a queue built with NewPending is exactly what it was before.
	store store
	// onSaveError is told when a write failed. Nil discards, which is what a
	// queue with no store wants.
	onSaveError func(error)
}

// NewPending returns an empty queue that keeps nothing across a restart.
func NewPending() *Pending {
	return &Pending{entries: map[string]Entry{}}
}

// Load returns a queue restored from path, and writes every change back to it.
//
// A queue is what is waiting on a person, and a restart is not a decision
// about any of it: without this a deploy dropped every pending approval, and
// re-announced each one the next time the watcher re-sent it.
//
// A file that cannot be parsed is reported and started from empty rather than
// refused. The queue rebuilds from the next check -- noisily, since everything
// in it looks new again -- and a queue that starts is worth more than one that
// is right about what it has already said.
func Load(path string, onError func(error)) *Pending {
	q := &Pending{entries: map[string]Entry{}, store: store{path: path}, onSaveError: onError}
	entries, err := q.store.load()
	if err != nil {
		if onError != nil {
			onError(err)
		}
		return q
	}
	q.entries = entries
	return q
}

// save writes the queue out. Called with the lock held.
//
// A failed write is reported and otherwise ignored: the entry is in the queue
// either way, and refusing to queue because a disk is full would turn a
// recoverable problem into a lost decision.
func (q *Pending) save() {
	if err := q.store.save(q.entries); err != nil && q.onSaveError != nil {
		q.onSaveError(err)
	}
}

// Put adds or replaces the entry for a service, reporting whether anything
// changed.
//
// Unchanged means the same candidate arriving again. That is not the schedule:
// a completed check advances that service's cutoff, so a tag is reported once.
// It is the delivery. The watcher's outbox drops a finding only when the POST
// succeeded, so one that was received while the response was lost -- a
// timeout, a restart mid-request -- is held and re-sent on the next run.
// DUVA_SINCE re-reports deliberately, which is the other way to see it.
//
// Reporting that nothing changed is how a caller avoids logging or notifying
// twice about one candidate.
func (q *Pending) Put(e Entry, now time.Time) (changed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	prev, existed := q.entries[e.Service]
	if existed && prev.To == e.To && prev.From == e.From {
		return false
	}
	// A candidate only supersedes a pending one if it is actually newer.
	//
	// Not last-write-wins, which is what this was: a watcher reporting four
	// releases at once sends them in whatever order it listed them, and
	// keeping the last arrival left the queue offering 4.39.24 when 4.39.27
	// was among them. Every service with more than one release outstanding
	// showed its oldest update.
	//
	// Only where both are versions. A digest move has no version pair to
	// compare, and there the newer arrival is by definition the current one.
	if existed && prev.Kind != "" && e.Kind != "" &&
		version.CompareVersions(e.To, prev.To) < 0 {
		return false
	}
	// FirstSeen is when *this* candidate arrived, so a superseding one gets
	// its own. Carrying the old one forward would make a row that has been
	// waiting five minutes look like it had been waiting a week.
	if e.FirstSeen == "" {
		e.FirstSeen = now.UTC().Format(time.RFC3339)
	}
	q.entries[e.Service] = e
	q.save()
	return true
}

// Get returns the entry for a service.
func (q *Pending) Get(service string) (Entry, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	e, ok := q.entries[service]
	return e, ok
}

// Remove drops a service's entry, reporting whether there was one.
func (q *Pending) Remove(service string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.entries[service]
	delete(q.entries, service)
	if ok {
		q.save()
	}
	return ok
}

// Reconcile drops entries the world no longer agrees with.
//
// An entry leaves the queue two ways today: an apply completes, or a newer
// candidate for the same service supersedes it. Neither covers a service
// changed by anything else -- a person running `docker pin upgrade` by hand,
// a hand-edited compose file, a manual `docker pull` + `compose up`. The
// queue has no way to learn about those on its own, and an entry already
// satisfied by reality sits offering an update that already happened.
//
// still is asked once per entry, with the queue's lock held for the whole
// call: it is expected to read a compose file, not to block on anything
// slower. Called on a schedule (see the queue binary's reconciler), not on
// every read, because "is this still true" costs a file read per entry and a
// person approving something stale is rare enough not to need it checked
// synchronously.
//
// still's error is logged by the caller and treated as "still wanted": a
// compose file that briefly fails to read is not evidence an entry is
// satisfied, and removing one on a spurious read error would be worse than
// leaving a stale one an extra cycle.
func (q *Pending) Reconcile(still func(e Entry) (bool, error), onError func(service string, err error)) (removed []string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for service, e := range q.entries {
		ok, err := still(e)
		if err != nil {
			if onError != nil {
				onError(service, err)
			}
			continue
		}
		if ok {
			continue
		}
		delete(q.entries, service)
		removed = append(removed, service)
	}
	if len(removed) > 0 {
		sort.Strings(removed)
		q.save()
	}
	return removed
}

// List returns everything waiting, ordered by service so a row keeps its
// place between reloads.
func (q *Pending) List() []Entry {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out := make([]Entry, 0, len(q.entries))
	for _, e := range q.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// Len is how many entries are waiting.
func (q *Pending) Len() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.entries)
}

// Due returns the entries a clock has released by now.
//
// Only ones with a ReleaseAt: an entry waiting for a person is never due, and
// asking this question of it would be asking when someone will make up their
// mind.
func (q *Pending) Due(now time.Time) []Entry {
	q.mu.RLock()
	defer q.mu.RUnlock()
	var out []Entry
	for _, e := range q.entries {
		if e.ReleaseAt == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, e.ReleaseAt)
		if err != nil {
			// An unparseable release time is not a reason to release
			// something early. It waits, and stays visible, which is the
			// safe direction for a field nobody can read.
			continue
		}
		if !now.Before(at) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// EntryFrom builds a queue entry from a verdict about a service.
//
// Pure, and separate from Put, so the shape of a row can be tested without a
// queue and a clock.
func EntryFrom(v Verdict, svc Service, digest string) Entry {
	return Entry{
		Service: svc.Name,
		File:    svc.File,
		Image:   svc.Image,
		From:    v.From,
		To:      v.To,
		Tag:     tagAfter(v, svc),
		Digest:  digest,
		Kind:    v.Kind,
		Why:     v.Why,
		Auto:    svc.Auto,
	}
}

// tagAfter is the tag the service follows once this is applied.
//
// The tag is an instruction, not a description of what is pinned, so a digest
// move leaves it alone and only a version change sets it.
func tagAfter(v Verdict, svc Service) string {
	if v.Kind == "" {
		return svc.Tag // a digest move: the tag does not change
	}
	return v.To
}
