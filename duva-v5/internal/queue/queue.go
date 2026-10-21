package queue

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// Entry is one decision waiting to happen.
//
// Both kinds of waiting are the same entry. An update held back for a person
// to approve and one held back until a release is old enough differ only in
// what releases them — a click or a clock — and nothing else about them is
// different, so there is one queue rather than a queue and a soak list.
type Entry struct {
	// ID is the entry's row. Zero until it has been stored.
	ID int64 `json:"id,omitempty"`
	// Service, File and Image identify what would change; Container is the
	// container_name, which is how duva identifies a service.
	Service   string `json:"service"`
	Container string `json:"container,omitempty"`
	File      string `json:"file"`
	Image     string `json:"image"`
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
	// Published is when the release appeared upstream, RFC 3339, when known.
	Published string `json:"published,omitempty"`
	// ReleaseAt, when set, is when a clock rather than a person releases
	// this. Zero means it waits for someone.
	ReleaseAt string `json:"release_at,omitempty"`
}

// Pending is what is waiting: the queue's rows in the store.
//
// A row per release, not per service. A release waiting for a person still
// supersedes older ones waiting for a person -- approving the newest makes
// them moot, and a stale row invites approving something already overtaken --
// but a soaking release does not: each soaks on its own clock, so an older
// one that comes due while a newer one is still soaking is applied, rather
// than the wait restarting with every release. Supersession otherwise happens
// when something is applied: everything older for that service goes then.
//
// What a person sees is the newest open entry per service (Get, List); the
// rest is there for the clock, and for history.
type Pending struct {
	// mu serialises the read-decide-write of each operation. The store has
	// one connection, but a check followed by an insert is two statements,
	// and two arrivals of the same release must not both insert.
	mu sync.Mutex
	st *store.Store
	// onError is told when the store failed. Nil discards. A queue that
	// cannot write still answers from what it could read, which is better
	// than a queue that stops.
	onError func(error)
}

// NewPending returns an empty queue in a database of its own, in memory.
func NewPending() *Pending {
	st, err := store.Open(":memory:")
	if err != nil {
		panic("an in-memory store would not open: " + err.Error())
	}
	return &Pending{st: st}
}

// Over returns the queue kept in st. onError is told about writes that fail.
func Over(st *store.Store, onError func(error)) *Pending {
	return &Pending{st: st, onError: onError}
}

func (q *Pending) fail(err error) {
	if err != nil && q.onError != nil {
		q.onError(err)
	}
}

// columns is every entry column, in the order scan reads them.
const columns = `e.id, s.name, s.container, e.file, i.repo, e.from_ref, e.to_ref, e.tag,
	COALESCE(e.digest, ''), COALESCE(e.kind, ''), e.auto, e.why, e.found_at,
	COALESCE(e.published_at, ''), COALESCE(e.release_at, '')`

const fromEntries = ` FROM queue_entries e
	JOIN services s ON s.id = e.service_id
	JOIN images i ON i.id = s.image_id `

// open is the statuses still waiting on something.
const open = `e.status IN ('waiting', 'applying')`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Entry, error) {
	var e Entry
	var kind, auto string
	err := row.Scan(&e.ID, &e.Service, &e.Container, &e.File, &e.Image, &e.From, &e.To, &e.Tag,
		&e.Digest, &kind, &auto, &e.Why, &e.FirstSeen, &e.Published, &e.ReleaseAt)
	e.Kind, e.Auto = version.Kind(kind), Auto(auto)
	return e, err
}

func (q *Pending) query(where string, args ...any) ([]Entry, error) {
	rows, err := q.st.DB().Query(`SELECT `+columns+fromEntries+`WHERE `+where+` ORDER BY e.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// openFor is every open entry for a service, by its compose name.
func (q *Pending) openFor(service string) ([]Entry, error) {
	return q.query(open+` AND s.name = ?`, service)
}

// newer reports whether a is a later release than b. Where both are versions
// they are compared as versions; otherwise -- a digest move has none -- the
// later arrival is the newer, since it is what the tag points at now.
func newer(a, b Entry) bool {
	if a.Kind != "" && b.Kind != "" {
		if c := version.CompareVersions(a.To, b.To); c != 0 {
			return c > 0
		}
	}
	return a.ID > b.ID
}

func newest(entries []Entry) (Entry, bool) {
	if len(entries) == 0 {
		return Entry{}, false
	}
	best := entries[0]
	for _, e := range entries[1:] {
		if newer(e, best) {
			best = e
		}
	}
	return best, true
}

// Put records a candidate, and reports whether it is news: a release not
// seen before that is now the newest for its service.
//
// The same release arriving again is not news and changes nothing. That is
// not the schedule -- a checked service's cutoff moves, so a tag is reported
// once -- but redelivery, and announcing it twice would be noise.
func (q *Pending) Put(e Entry, now time.Time) (news bool) {
	_, news = q.put(e, now)
	return news
}

// PutEntry is Put, returning the entry as stored -- with its ID, which is
// what applying it later needs.
func (q *Pending) PutEntry(e Entry, now time.Time) (Entry, bool) {
	return q.put(e, now)
}

func (q *Pending) put(e Entry, now time.Time) (Entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	current, err := q.openFor(e.Service)
	if err != nil {
		q.fail(err)
		return e, false
	}
	for _, c := range current {
		if c.From == e.From && c.To == e.To {
			return c, false
		}
	}
	best, haveBest := newest(current)
	e.ID = 1 << 62 // compares as the latest arrival until it has a real ID
	isNewest := !haveBest || newer(e, best)
	// A release waiting for a person that is not the newest is moot: the
	// newer one is what a person would approve. One that soaks is kept,
	// because its clock may run out first.
	if !isNewest && e.ReleaseAt == "" {
		return best, false
	}
	if e.FirstSeen == "" {
		e.FirstSeen = now.UTC().Format(time.RFC3339)
	}

	err = q.st.Tx(func(tx *sql.Tx) error {
		serviceID, err := entryService(tx, e)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO queue_entries
			(service_id, file, from_ref, to_ref, tag, digest, kind, auto, why,
			 published_at, found_at, release_at, status)
			VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), 'waiting')`,
			serviceID, e.File, e.From, e.To, e.Tag, e.Digest, string(e.Kind), string(e.Auto), e.Why,
			e.Published, e.FirstSeen, e.ReleaseAt)
		if err != nil {
			return err
		}
		if e.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		kind := "found"
		if e.ReleaseAt != "" {
			kind = "soaking"
		}
		if err := event(tx, e.ID, serviceID, now, kind, e.Why); err != nil {
			return err
		}
		// A new release for a person supersedes older ones for a person.
		// Soaking ones stay: their clocks are their own.
		if e.ReleaseAt == "" {
			for _, c := range current {
				if c.ReleaseAt == "" && newer(e, c) {
					if err := setStatus(tx, c.ID, serviceID, now, "superseded", "overtaken by "+e.To); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		q.fail(fmt.Errorf("recording %s -> %s for %s: %w", e.From, e.To, e.Service, err))
		return e, false
	}
	return e, isNewest
}

// ensureService finds or creates a service, enabling it if it was disabled.
//
// Created on demand as well as by the sync, so the first finding for a
// service needs nothing to have run first. A service that comes back is the
// same row, so the same history.
func ensureService(tx *sql.Tx, container, name, image string) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO images (repo) VALUES (?) ON CONFLICT (repo) DO NOTHING`, image); err != nil {
		return 0, err
	}
	var imageID int64
	if err := tx.QueryRow(`SELECT id FROM images WHERE repo = ?`, image).Scan(&imageID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO services (container, name, image_id) VALUES (?, ?, ?)
		ON CONFLICT (container) DO UPDATE SET name = excluded.name, image_id = excluded.image_id, removed_at = NULL`,
		container, name, imageID); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM services WHERE container = ?`, container).Scan(&id)
	return id, err
}

// entryService is ensureService for the service an entry is about.
func entryService(tx *sql.Tx, e Entry) (int64, error) {
	container := e.Container
	if container == "" {
		container = e.Service
	}
	return ensureService(tx, container, e.Service, e.Image)
}

// event appends to the history.
func event(tx *sql.Tx, entryID, serviceID int64, at time.Time, kind, detail string) error {
	_, err := tx.Exec(`INSERT INTO queue_events (entry_id, service_id, at, kind, actor, detail)
		VALUES (?, ?, ?, ?, 'duva', ?)`, entryID, serviceID, at.UTC().Format(time.RFC3339), kind, detail)
	return err
}

// setStatus moves an entry to a status, and says so in the history.
func setStatus(tx *sql.Tx, entryID, serviceID int64, at time.Time, status, detail string) error {
	if _, err := tx.Exec(`UPDATE queue_entries SET status = ? WHERE id = ?`, status, entryID); err != nil {
		return err
	}
	return event(tx, entryID, serviceID, at, status, detail)
}

func serviceOf(tx *sql.Tx, entryID int64) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT service_id FROM queue_entries WHERE id = ?`, entryID).Scan(&id)
	return id, err
}

// Get returns the newest open entry for a service: what a person would see,
// and approve.
func (q *Pending) Get(service string) (Entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	current, err := q.openFor(service)
	if err != nil {
		q.fail(err)
		return Entry{}, false
	}
	return newest(current)
}

// ByID returns an entry by its row, if it is still open.
func (q *Pending) ByID(id int64) (Entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	found, err := q.query(open+` AND e.id = ?`, id)
	if err != nil {
		q.fail(err)
		return Entry{}, false
	}
	if len(found) == 0 {
		return Entry{}, false
	}
	return found[0], true
}

// Complete records that an entry was applied. Every open entry for the
// service that is not newer goes with it, superseded: the service is past
// them now. A newer one stays, and will be offered from where the service is.
func (q *Pending) Complete(e Entry, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	current, err := q.openFor(e.Service)
	if err != nil {
		q.fail(err)
		return
	}
	q.fail(q.st.Tx(func(tx *sql.Tx) error {
		serviceID, err := serviceOf(tx, e.ID)
		if err != nil {
			return err
		}
		if err := setStatus(tx, e.ID, serviceID, now, "completed", e.From+" -> "+e.To); err != nil {
			return err
		}
		for _, c := range current {
			if c.ID != e.ID && !newer(c, e) {
				if err := setStatus(tx, c.ID, serviceID, now, "superseded", "applied "+e.To); err != nil {
					return err
				}
			}
		}
		return nil
	}))
}

// Remove drops every open entry for a service, reporting whether there was
// one. For a service whose update happened some other way.
func (q *Pending) Remove(service string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	current, err := q.openFor(service)
	if err != nil || len(current) == 0 {
		q.fail(err)
		return false
	}
	now := time.Now()
	q.fail(q.st.Tx(func(tx *sql.Tx) error {
		for _, c := range current {
			serviceID, err := serviceOf(tx, c.ID)
			if err != nil {
				return err
			}
			if err := setStatus(tx, c.ID, serviceID, now, "dropped", "removed"); err != nil {
				return err
			}
		}
		return nil
	}))
	return true
}

// Reconcile drops entries the world no longer agrees with.
//
// An entry leaves the queue two ways in the ordinary course: an apply
// completes, or a newer candidate supersedes it. Neither covers a service
// changed by anything else -- a person running `docker pin upgrade` by hand,
// a hand-edited compose file, a manual `docker pull` + `compose up` -- and an
// entry already satisfied by reality would sit offering an update that
// already happened.
//
// check is asked once per open entry. Its error is logged by the caller and
// treated as "still wanted": a compose file that briefly fails to read is not
// evidence an entry is satisfied. An entry it keeps is stored as check
// returned it, which is how a From the file has moved past is brought up to
// date; it keeps its place and its clock.
func (q *Pending) Reconcile(check func(e Entry) (Recheck, error), onError func(service string, err error)) (dropped []Recheck) {
	q.mu.Lock()
	defer q.mu.Unlock()
	current, err := q.query(open)
	if err != nil {
		q.fail(err)
		return nil
	}
	now := time.Now()
	for _, e := range current {
		r, err := check(e)
		if err != nil {
			if onError != nil {
				onError(e.Service, err)
			}
			continue
		}
		if r.Keep {
			kept := r.Entry
			if kept.From == e.From && kept.Why == e.Why && kept.To == e.To {
				continue
			}
			_, err := q.st.DB().Exec(`UPDATE queue_entries SET from_ref = ?, to_ref = ?, why = ? WHERE id = ?`,
				kept.From, kept.To, kept.Why, e.ID)
			q.fail(err)
			continue
		}
		r.Entry = e
		err = q.st.Tx(func(tx *sql.Tx) error {
			serviceID, err := serviceOf(tx, e.ID)
			if err != nil {
				return err
			}
			return setStatus(tx, e.ID, serviceID, now, "dropped", r.Why)
		})
		if err != nil {
			q.fail(err)
			continue
		}
		dropped = append(dropped, r)
	}
	sort.SliceStable(dropped, func(i, j int) bool { return dropped[i].Entry.Service < dropped[j].Entry.Service })
	return dropped
}

// List returns the newest open entry per service, ordered by service so a
// row keeps its place between reloads.
func (q *Pending) List() []Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	all, err := q.query(open)
	if err != nil {
		q.fail(err)
		return []Entry{}
	}
	bySvc := map[string][]Entry{}
	for _, e := range all {
		bySvc[e.Service] = append(bySvc[e.Service], e)
	}
	out := make([]Entry, 0, len(bySvc))
	for _, entries := range bySvc {
		e, _ := newest(entries)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// Waiting returns, per service, the newest entry waiting for a person: what
// the page's queue shows. Soaking entries are not in it; see Soaking.
func (q *Pending) Waiting() []Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	all, err := q.query(open + ` AND e.release_at IS NULL`)
	if err != nil {
		q.fail(err)
		return []Entry{}
	}
	bySvc := map[string][]Entry{}
	for _, e := range all {
		bySvc[e.Service] = append(bySvc[e.Service], e)
	}
	out := make([]Entry, 0, len(bySvc))
	for _, entries := range bySvc {
		e, _ := newest(entries)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// Soaking returns every entry waiting out a soak, the soonest released
// first. All of them, not the newest per service: each is on its own clock.
func (q *Pending) Soaking() []Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	all, err := q.query(open + ` AND e.release_at IS NOT NULL`)
	if err != nil {
		q.fail(err)
		return []Entry{}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ReleaseAt < all[j].ReleaseAt })
	if all == nil {
		all = []Entry{}
	}
	return all
}

// Len is how many services have something waiting.
func (q *Pending) Len() int { return len(q.List()) }

// Due returns the entries a clock has released by now: every open one whose
// soak has run out, not only the newest per service.
//
// Only ones with a release time: an entry waiting for a person is never due.
// An unreadable release time is not a reason to release something early; it
// waits, and stays visible.
func (q *Pending) Due(now time.Time) []Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	all, err := q.query(open + ` AND e.release_at IS NOT NULL`)
	if err != nil {
		q.fail(err)
		return nil
	}
	var out []Entry
	for _, e := range all {
		at, err := time.Parse(time.RFC3339, e.ReleaseAt)
		if err != nil {
			continue
		}
		if !now.Before(at) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// Release ends an entry's soak, returning it as it now stands.
//
// The clock is cleared before the apply starts: from here the entry is an
// auto-apply like any other, retried on a deferral and left for a person on a
// failure. Left set, it would be due again on every tick.
//
// false when there is nothing due to release: the entry is gone, or not yet
// due.
func (q *Pending) Release(e Entry, now time.Time) (Entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	found, err := q.query(open+` AND e.id = ?`, e.ID)
	if err != nil || len(found) == 0 {
		q.fail(err)
		return Entry{}, false
	}
	cur := found[0]
	at, err := time.Parse(time.RFC3339, cur.ReleaseAt)
	if cur.ReleaseAt == "" || err != nil || now.Before(at) {
		return Entry{}, false
	}
	err = q.st.Tx(func(tx *sql.Tx) error {
		serviceID, err := serviceOf(tx, cur.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE queue_entries SET release_at = NULL WHERE id = ?`, cur.ID); err != nil {
			return err
		}
		return event(tx, cur.ID, serviceID, now, "released", "soak is over")
	})
	if err != nil {
		q.fail(err)
		return Entry{}, false
	}
	cur.ReleaseAt = ""
	return cur, true
}

// ErrNotOpen is returned for an entry that is no longer waiting.
var ErrNotOpen = errors.New("that entry is no longer waiting")

// EntryFrom builds a queue entry from a verdict about a service.
//
// Pure, and separate from Put, so the shape of a row can be tested without a
// queue and a clock.
func EntryFrom(v Verdict, svc Service, digest string) Entry {
	return Entry{
		Service:   svc.Name,
		Container: svc.Container,
		File:      svc.File,
		Image:     svc.Image,
		From:      v.From,
		To:        v.To,
		Tag:       tagAfter(v, svc),
		Digest:    digest,
		Kind:      v.Kind,
		Why:       v.Why,
		Auto:      svc.Auto,
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
