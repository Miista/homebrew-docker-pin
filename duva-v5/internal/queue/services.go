package queue

import (
	"database/sql"
	"sort"
	"time"
)

// Container is one container on this host, as the service sync sees it.
type Container struct {
	// Name is its container_name, which is how duva identifies a service.
	Name string
	// Service is the compose service it belongs to.
	Service string
}

// Declared is one service as the compose file declares it.
//
// The image and tag come from here and never from the container: docker's
// container list reports a container by image ID once the tag it was created
// from has moved on, and recording that took grafana's image to be "sha256".
type Declared struct {
	// Container is its container_name.
	Container string
	// Service is the compose service name.
	Service string
	// Image is the base reference, without tag or digest.
	Image string
	// Tag is the tag the file follows.
	Tag string
	// Pinned is whether the file pins it by digest: what duva tracks.
	Pinned bool
}

// Tracked is every service duva tracks and that runs here, by compose name,
// sorted: pinned in the compose file, with a container, both as the last sync
// saw them. What "Check…" offers, so a service pinned a moment ago can be
// checked at once rather than after the next full check.
func (q *Pending) Tracked() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	rows, err := q.st.DB().Query(`SELECT DISTINCT name FROM services WHERE removed_at IS NULL AND pinned = 1 ORDER BY name`)
	if err != nil {
		q.fail(err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && q.running[name] {
			out = append(out, name)
		}
	}
	return out
}

// Running is the compose name of every service with a container on this host
// now, as the last service sync saw it.
func (q *Pending) Running() map[string]bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[string]bool, len(q.running))
	for name := range q.running {
		out[name] = true
	}
	return out
}

// Removed is whether the sync recorded a service as removed. A service it has
// never seen is not: nothing says it went.
func (q *Pending) Removed(container string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	var removed sql.NullString
	err := q.st.DB().QueryRow(`SELECT removed_at FROM services WHERE container = ?`, container).Scan(&removed)
	return err == nil && removed.Valid
}

// SyncServices brings the services table up to date with the world outside
// duva: the compose file says which services exist and what image and tag
// each follows, and the containers say which are still there.
//
// A service declared in the file is added, or brought back on its old row --
// the same history -- and has its image and tag recorded. One that is neither
// declared nor has a container is removed: both, because a service briefly
// gone from the file (an edit mid-rebase) still runs, and one whose container
// is gone for a recreate is still declared. Nothing is ever deleted.
//
// Every change it makes is reported, and recorded in the history: a sync
// that changed something and said nothing is how pinning a service looked
// like it had not taken.
func (q *Pending) SyncServices(declared []Declared, containers []Container, now time.Time) (changes []Change, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	present := map[string]bool{}
	running := map[string]bool{}
	for _, c := range containers {
		if c.Name == "" {
			continue
		}
		present[c.Name] = true
		if c.Service != "" {
			running[c.Service] = true
		}
	}
	inFile := map[string]bool{}
	err = q.st.Tx(func(tx *sql.Tx) error {
		for _, d := range declared {
			if d.Container == "" || d.Service == "" || d.Image == "" {
				continue
			}
			inFile[d.Container] = true
			imageID, err := ensureImage(tx, d.Image)
			if err != nil {
				return err
			}
			var id int64
			var was sql.NullString
			var oldImage, oldTag string
			var oldPinned bool
			existed := tx.QueryRow(`SELECT s.id, s.removed_at, i.repo, COALESCE(s.tag, ''), s.pinned
				FROM services s JOIN images i ON i.id = s.image_id WHERE s.container = ?`, d.Container).
				Scan(&id, &was, &oldImage, &oldTag, &oldPinned) == nil
			if _, err := tx.Exec(`INSERT INTO services (container, name, image_id, tag, pinned) VALUES (?, ?, ?, NULLIF(?, ''), ?)
				ON CONFLICT (container) DO UPDATE SET name = excluded.name, image_id = excluded.image_id,
					tag = excluded.tag, pinned = excluded.pinned, removed_at = NULL`,
				d.Container, d.Service, imageID, d.Tag, d.Pinned); err != nil {
				return err
			}
			if err := tx.QueryRow(`SELECT id FROM services WHERE container = ?`, d.Container).Scan(&id); err != nil {
				return err
			}
			var these []Change
			switch {
			case !existed:
				these = append(these, Change{d.Container, ChangeAdded, tracking(d.Pinned)})
			case was.Valid:
				these = append(these, Change{d.Container, ChangeBack, tracking(d.Pinned)})
			default:
				if d.Pinned != oldPinned {
					if d.Pinned {
						these = append(these, Change{d.Container, ChangeTracked, "pinned in the compose file"})
					} else {
						these = append(these, Change{d.Container, ChangeUntracked, "no longer pinned"})
					}
				}
				if d.Image != oldImage {
					these = append(these, Change{d.Container, ChangeImage, oldImage + " → " + d.Image})
				}
				if d.Tag != oldTag {
					these = append(these, Change{d.Container, ChangeTag, orNone(oldTag) + " → " + orNone(d.Tag)})
				}
			}
			for _, c := range these {
				if err := serviceEvent(tx, id, now, "service-"+string(c.Kind), c.Detail); err != nil {
					return err
				}
			}
			changes = append(changes, these...)
		}

		rows, err := tx.Query(`SELECT id, container FROM services WHERE removed_at IS NULL`)
		if err != nil {
			return err
		}
		type gone struct {
			id        int64
			container string
		}
		var missing []gone
		for rows.Next() {
			var g gone
			if err := rows.Scan(&g.id, &g.container); err != nil {
				rows.Close()
				return err
			}
			if !inFile[g.container] && !present[g.container] {
				missing = append(missing, g)
			}
		}
		rows.Close()
		for _, g := range missing {
			if _, err := tx.Exec(`UPDATE services SET removed_at = ? WHERE id = ?`,
				now.UTC().Format(time.RFC3339), g.id); err != nil {
				return err
			}
			c := Change{g.container, ChangeRemoved, "gone from the compose file, and its container gone"}
			if err := serviceEvent(tx, g.id, now, "service-removed", c.Detail); err != nil {
				return err
			}
			changes = append(changes, c)
		}
		return nil
	})
	if err == nil {
		q.running = running
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Container < changes[j].Container })
	return changes, err
}

// Change is one thing the sync changed about a service.
type Change struct {
	// Container is the service's container_name.
	Container string
	Kind      ChangeKind
	// Detail says what, in a clause: "grafana/grafana → grafana/grafana-oss".
	Detail string
}

// ChangeKind is what kind of change the sync made.
type ChangeKind string

const (
	ChangeAdded     ChangeKind = "added"
	ChangeBack      ChangeKind = "back"
	ChangeRemoved   ChangeKind = "removed"
	ChangeTracked   ChangeKind = "tracked"
	ChangeUntracked ChangeKind = "untracked"
	ChangeImage     ChangeKind = "image"
	ChangeTag       ChangeKind = "tag"
)

// String is the change as one line of the sync's output.
func (c Change) String() string {
	what := map[ChangeKind]string{
		ChangeAdded: "added", ChangeBack: "back", ChangeRemoved: "removed",
		ChangeTracked: "now tracked", ChangeUntracked: "no longer tracked",
		ChangeImage: "image", ChangeTag: "tag",
	}[c.Kind]
	return c.Container + ": " + what + " (" + c.Detail + ")"
}

func tracking(pinned bool) string {
	if pinned {
		return "tracked: pinned in the compose file"
	}
	return "not tracked: not pinned"
}

func orNone(tag string) string {
	if tag == "" {
		return "no tag"
	}
	return tag
}

func serviceEvent(tx *sql.Tx, serviceID int64, at time.Time, kind, detail string) error {
	_, err := tx.Exec(`INSERT INTO queue_events (service_id, at, kind, actor, detail)
		VALUES (?, ?, ?, 'duva', ?)`, serviceID, at.UTC().Format(time.RFC3339), kind, detail)
	return err
}
