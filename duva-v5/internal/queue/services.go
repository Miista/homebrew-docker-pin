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
// It reports what changed, by container name.
func (q *Pending) SyncServices(declared []Declared, containers []Container, now time.Time) (added, removed []string, err error) {
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
			existed := tx.QueryRow(`SELECT id, removed_at FROM services WHERE container = ?`, d.Container).Scan(&id, &was) == nil
			if _, err := tx.Exec(`INSERT INTO services (container, name, image_id, tag) VALUES (?, ?, ?, NULLIF(?, ''))
				ON CONFLICT (container) DO UPDATE SET name = excluded.name, image_id = excluded.image_id,
					tag = excluded.tag, removed_at = NULL`,
				d.Container, d.Service, imageID, d.Tag); err != nil {
				return err
			}
			if err := tx.QueryRow(`SELECT id FROM services WHERE container = ?`, d.Container).Scan(&id); err != nil {
				return err
			}
			switch {
			case !existed:
				added = append(added, d.Container)
				if err := serviceEvent(tx, id, now, "service-added", d.Service); err != nil {
					return err
				}
			case was.Valid:
				added = append(added, d.Container)
				if err := serviceEvent(tx, id, now, "service-back", d.Service); err != nil {
					return err
				}
			}
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
			if err := serviceEvent(tx, g.id, now, "service-removed", g.container); err != nil {
				return err
			}
			removed = append(removed, g.container)
		}
		return nil
	})
	if err == nil {
		q.running = running
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed, err
}

func serviceEvent(tx *sql.Tx, serviceID int64, at time.Time, kind, detail string) error {
	_, err := tx.Exec(`INSERT INTO queue_events (service_id, at, kind, actor, detail)
		VALUES (?, ?, ?, 'duva', ?)`, serviceID, at.UTC().Format(time.RFC3339), kind, detail)
	return err
}
