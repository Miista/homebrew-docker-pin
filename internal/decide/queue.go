package decide

import (
	"sort"
	"sync"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/version"
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
}

// NewPending returns an empty queue.
func NewPending() *Pending {
	return &Pending{entries: map[string]Entry{}}
}

// Put adds or replaces the entry for a service, reporting whether anything
// changed.
//
// Unchanged means the same candidate arriving again, which is the common case
// under a protocol that re-notifies: the detector says the same thing every
// run until something is done about it. Reporting that nothing changed is how
// a caller avoids logging or notifying twice about one candidate.
func (q *Pending) Put(e Entry, now time.Time) (changed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	prev, existed := q.entries[e.Service]
	if existed && prev.To == e.To && prev.From == e.From {
		return false
	}
	// FirstSeen is when *this* candidate arrived, so a superseding one gets
	// its own. Carrying the old one forward would make a row that has been
	// waiting five minutes look like it had been waiting a week.
	if e.FirstSeen == "" {
		e.FirstSeen = now.UTC().Format(time.RFC3339)
	}
	q.entries[e.Service] = e
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
	return ok
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
