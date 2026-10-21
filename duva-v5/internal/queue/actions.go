package queue

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What a person does to the queue, beyond approving: discard an entry, block
// a release, unblock it. Each is an event carrying who did it.

// actorEvent appends to the history on someone's behalf.
func actorEvent(tx *sql.Tx, entryID, serviceID int64, at time.Time, kind, actor, detail string) error {
	if actor == "" {
		actor = "someone"
	}
	_, err := tx.Exec(`INSERT INTO queue_events (entry_id, service_id, at, kind, actor, detail)
		VALUES (NULLIF(?, 0), NULLIF(?, 0), ?, ?, ?, ?)`,
		entryID, serviceID, at.UTC().Format(time.RFC3339), kind, actor, detail)
	return err
}

// Approved records that a person approved an entry. The apply itself is the
// applier's; this is the record of who asked for it.
func (q *Pending) Approved(e Entry, actor string, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.fail(q.st.Tx(func(tx *sql.Tx) error {
		serviceID, err := serviceOf(tx, e.ID)
		if err != nil {
			return err
		}
		return actorEvent(tx, e.ID, serviceID, now, "approved", actor, e.From+" -> "+e.To)
	}))
}

// Discard drops an entry a person does not want now. Not a judgement on the
// release: the next check that finds it offers it again. That is Block.
func (q *Pending) Discard(id int64, actor string, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	found, err := q.query(open+` AND e.id = ?`, id)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return ErrNotOpen
	}
	return q.st.Tx(func(tx *sql.Tx) error {
		serviceID, err := serviceOf(tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE queue_entries SET status = 'discarded' WHERE id = ?`, id); err != nil {
			return err
		}
		return actorEvent(tx, id, serviceID, now, "discarded", actor, found[0].From+" -> "+found[0].To)
	})
}

// ErrAlreadyBlocked is returned for a release that is already blocked.
var ErrAlreadyBlocked = errors.New("that release is already blocked")

// Block marks a release as bad: an image at an index digest, never a tag --
// a maintainer re-pushing a fixed release under the same tag must not be
// blocked with the broken one. Scoped to the image, so every service running
// it is covered.
//
// Open entries for that release are blocked with it: those that carry the
// digest, and the entry the block was made from (a version change, whose
// digest the caller resolved from its tag). One queued or approved before
// the block is still refused by the updater, which checks after the pull.
func (q *Pending) Block(entryID int64, digest, reason, actor string, now time.Time) error {
	if digest == "" {
		return fmt.Errorf("a release is blocked by its digest, and none was given")
	}
	// Optional: the page does not ask. Recorded as who blocked it, so a log
	// line about a refused release never has an empty "why".
	if reason == "" {
		if actor == "" {
			actor = "someone"
		}
		reason = "blocked by " + actor
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	found, err := q.query(`e.id = ?`, entryID)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("no entry %d", entryID)
	}
	e := found[0]
	return q.st.Tx(func(tx *sql.Tx) error {
		var imageID int64
		if err := tx.QueryRow(`SELECT id FROM images WHERE repo = ?`, e.Image).Scan(&imageID); err != nil {
			return err
		}
		release := e.To
		if e.Kind == "" {
			release = e.Tag // a digest move: the tag it moved on
		}
		_, err := tx.Exec(`INSERT INTO blocked_releases (image_id, digest, release, entry_id, reason, blocked_by, blocked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, imageID, digest, release, entryID, reason, actor, now.UTC().Format(time.RFC3339))
		if err != nil {
			var exists int
			if tx.QueryRow(`SELECT COUNT(*) FROM blocked_releases
				WHERE image_id = ? AND digest = ? AND unblocked_at IS NULL`, imageID, digest).Scan(&exists) == nil && exists > 0 {
				return ErrAlreadyBlocked
			}
			return err
		}
		rows, err := tx.Query(`SELECT e.id, e.service_id FROM queue_entries e
			JOIN services s ON s.id = e.service_id
			WHERE `+open+` AND s.image_id = ? AND (e.id = ? OR e.digest = ?)`, imageID, entryID, digest)
		if err != nil {
			return err
		}
		type hit struct{ entry, service int64 }
		var hits []hit
		for rows.Next() {
			var h hit
			if err := rows.Scan(&h.entry, &h.service); err != nil {
				rows.Close()
				return err
			}
			hits = append(hits, h)
		}
		rows.Close()
		for _, h := range hits {
			if _, err := tx.Exec(`UPDATE queue_entries SET status = 'blocked' WHERE id = ?`, h.entry); err != nil {
				return err
			}
			if err := actorEvent(tx, h.entry, h.service, now, "blocked", actor, reason); err != nil {
				return err
			}
		}
		return nil
	})
}

// Unblock lifts a standing block, and puts what it took out of the queue back
// in: the entry it was made from, and any other entry it blocked by digest.
//
// Back waiting for a person, a soaking one included. Its soak has most likely
// run out while it was blocked, and restored it would apply itself on the
// next tick -- seconds after someone only meant to lift a ban. Unblocked, it
// is theirs to approve.
//
// The block stays in the history. An entry the service has since moved past
// is put back too, and the next reconcile drops it as satisfied.
func (q *Pending) Unblock(image, digest, actor string, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.st.Tx(func(tx *sql.Tx) error {
		var blockID, imageID int64
		var entryID sql.NullInt64
		err := tx.QueryRow(`SELECT b.id, b.image_id, b.entry_id FROM blocked_releases b
			JOIN images i ON i.id = b.image_id
			WHERE i.repo = ? AND b.digest = ? AND b.unblocked_at IS NULL`, image, digest).Scan(&blockID, &imageID, &entryID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s@%s is not blocked", image, digest)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE blocked_releases SET unblocked_at = ? WHERE id = ?`,
			now.UTC().Format(time.RFC3339), blockID); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT e.id, e.service_id FROM queue_entries e
			JOIN services s ON s.id = e.service_id
			WHERE e.status = 'blocked' AND s.image_id = ? AND (e.id = ? OR e.digest = ?)`,
			imageID, entryID.Int64, digest)
		if err != nil {
			return err
		}
		type hit struct{ entry, service int64 }
		var back []hit
		for rows.Next() {
			var h hit
			if err := rows.Scan(&h.entry, &h.service); err != nil {
				rows.Close()
				return err
			}
			back = append(back, h)
		}
		rows.Close()
		for _, h := range back {
			if _, err := tx.Exec(`UPDATE queue_entries SET status = 'waiting', release_at = NULL WHERE id = ?`, h.entry); err != nil {
				return err
			}
			if err := actorEvent(tx, h.entry, h.service, now, "unblocked", actor, image+"@"+digest); err != nil {
				return err
			}
		}
		if len(back) == 0 {
			return actorEvent(tx, 0, 0, now, "unblocked", actor, image+"@"+digest)
		}
		return nil
	})
}

// IsBlocked reports whether a release is blocked, and why.
//
// A store that cannot be read answers "not blocked" and reports the error:
// the blocklist is a person's veto on top of policy, and an unreadable one
// must not stop every update -- the next apply asks again.
func (q *Pending) IsBlocked(image, digest string) (reason string, blocked bool) {
	if digest == "" {
		return "", false
	}
	err := q.st.DB().QueryRow(`SELECT b.reason FROM blocked_releases b JOIN images i ON i.id = b.image_id
		WHERE i.repo = ? AND b.digest = ? AND b.unblocked_at IS NULL`, image, digest).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		q.fail(fmt.Errorf("reading the blocklist: %w", err))
		return "", false
	}
	return reason, true
}

// MarkBlocked records that the updater refused an entry because its release
// is blocked: what the queue learns when a version change's digest turns out,
// on pulling, to be one a person blocked.
func (q *Pending) MarkBlocked(e Entry, reason string, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.fail(q.st.Tx(func(tx *sql.Tx) error {
		serviceID, err := serviceOf(tx, e.ID)
		if err != nil {
			return err
		}
		return setStatus(tx, e.ID, serviceID, now, "blocked", reason)
	}))
}

// Asked records that a person asked for a service to be checked or
// rescanned. What it found arrives in the queue as any finding does.
func (q *Pending) Asked(service, what, actor string, now time.Time) {
	q.fail(q.st.Tx(func(tx *sql.Tx) error {
		var serviceID int64
		if err := tx.QueryRow(`SELECT id FROM services WHERE name = ?`, service).Scan(&serviceID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return actorEvent(tx, 0, serviceID, now, what, actor, service)
	}))
}

// Block is one standing block, as a list of them shows it.
type Block struct {
	Image   string `json:"image"`
	Digest  string `json:"digest"`
	Release string `json:"release"` // the version, or tag, it was blocked at
	Reason  string `json:"reason"`
	By      string `json:"by"`
	At      string `json:"at"`
}

// Blocks returns the standing blocks, the most recent first.
func (q *Pending) Blocks() []Block {
	rows, err := q.st.DB().Query(`SELECT i.repo, b.digest, b.release, b.reason, b.blocked_by, b.blocked_at
		FROM blocked_releases b JOIN images i ON i.id = b.image_id
		WHERE b.unblocked_at IS NULL ORDER BY b.blocked_at DESC, b.id DESC`)
	if err != nil {
		q.fail(err)
		return []Block{}
	}
	defer rows.Close()
	out := []Block{}
	for rows.Next() {
		var b Block
		if err := rows.Scan(&b.Image, &b.Digest, &b.Release, &b.Reason, &b.By, &b.At); err != nil {
			q.fail(err)
			return out
		}
		out = append(out, b)
	}
	return out
}
