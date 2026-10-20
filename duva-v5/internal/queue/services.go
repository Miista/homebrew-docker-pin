package queue

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// Container is one container on this host, as the service sync sees it.
type Container struct {
	// Name is its container_name, which is how duva identifies a service.
	Name string
	// Service is the compose service it belongs to.
	Service string
	// Image is the reference it runs, tag and digest included or not.
	Image string
}

// SyncServices makes the services table a mirror of the containers on this
// host: a container not seen before is added, one that is gone is disabled,
// and one that comes back is enabled again -- the same row, so the same
// history. Nothing is ever deleted.
//
// It reports what changed, by container name.
func (q *Pending) SyncServices(containers []Container, now time.Time) (added, removed []string, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	present := map[string]bool{}
	err = q.st.Tx(func(tx *sql.Tx) error {
		for _, c := range containers {
			if c.Name == "" || c.Service == "" {
				continue
			}
			present[c.Name] = true
			var id int64
			var was sql.NullString
			existed := tx.QueryRow(`SELECT id, removed_at FROM services WHERE container = ?`, c.Name).Scan(&id, &was) == nil
			if id, err = ensureService(tx, c.Name, c.Service, repoOf(c.Image)); err != nil {
				return err
			}
			switch {
			case !existed:
				added = append(added, c.Name)
				if err := serviceEvent(tx, id, now, "service-added", c.Service); err != nil {
					return err
				}
			case was.Valid:
				added = append(added, c.Name)
				if err := serviceEvent(tx, id, now, "service-back", c.Service); err != nil {
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
			if !present[g.container] {
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
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed, err
}

func serviceEvent(tx *sql.Tx, serviceID int64, at time.Time, kind, detail string) error {
	_, err := tx.Exec(`INSERT INTO queue_events (service_id, at, kind, actor, detail)
		VALUES (?, ?, ?, 'duva', ?)`, serviceID, at.UTC().Format(time.RFC3339), kind, detail)
	return err
}

// repoOf is an image reference without its tag or digest.
func repoOf(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	// The last colon after the last slash is the tag; one before a slash is
	// a registry port.
	slash := strings.LastIndexByte(ref, '/')
	if i := strings.LastIndexByte(ref, ':'); i > slash {
		ref = ref[:i]
	}
	return ref
}
