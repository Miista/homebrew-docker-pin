package queue

import (
	"database/sql"
	"testing"
	"time"
)

func serviceRows(t *testing.T, q *Pending) map[string]bool {
	t.Helper()
	rows, err := q.st.DB().Query(`SELECT container, removed_at IS NULL FROM services`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		var enabled bool
		rows.Scan(&c, &enabled)
		out[c] = enabled
	}
	return out
}

// serviceImage is the image and tag the services table records for one.
func serviceImage(t *testing.T, q *Pending, container string) (image, tag string) {
	t.Helper()
	var tg *string
	if err := q.st.DB().QueryRow(`SELECT i.repo, s.tag FROM services s JOIN images i ON i.id = s.image_id
		WHERE s.container = ?`, container).Scan(&image, &tg); err != nil {
		t.Fatal(err)
	}
	if tg != nil {
		tag = *tg
	}
	return image, tag
}

func declared(name, image, tag string) Declared {
	return Declared{Container: name, Service: name, Image: image, Tag: tag}
}

func running(names ...string) []Container {
	var out []Container
	for _, n := range names {
		out = append(out, Container{Name: n, Service: n})
	}
	return out
}

// Removed takes both: gone from the compose file, and its container gone.
// Either alone is something else -- an edit mid-rebase, a recreate, a service
// someone stopped -- and the service is still here.
func TestAServiceIsRemovedOnlyWhenGoneFromTheFileAndItsContainerGone(t *testing.T) {
	q := NewPending()
	app, db := declared("app", "example.com/app", "1"), declared("db", "postgres", "17")
	added, removed, err := q.SyncServices([]Declared{app, db}, running("app", "db"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || len(removed) != 0 {
		t.Errorf("added %v, removed %v", added, removed)
	}

	// db is gone from the file but still runs.
	if _, removed, _ = q.SyncServices([]Declared{app}, running("app", "db"), now); len(removed) != 0 {
		t.Errorf("removed %v while db's container still runs", removed)
	}
	// db is back in the file, its container gone.
	if _, removed, _ = q.SyncServices([]Declared{app, db}, running("app"), now); len(removed) != 0 {
		t.Errorf("removed %v while db is still declared", removed)
	}
	// Both: db is gone.
	if _, removed, _ = q.SyncServices([]Declared{app}, running("app"), now); len(removed) != 1 || removed[0] != "db" {
		t.Errorf("removed %v, want db", removed)
	}
	if got := serviceRows(t, q); !got["app"] || got["db"] {
		t.Errorf("services = %v, want app enabled and db disabled but kept", got)
	}

	// db comes back: the same row.
	added, _, _ = q.SyncServices([]Declared{app, db}, running("app", "db"), now.Add(time.Hour))
	if len(added) != 1 || added[0] != "db" {
		t.Errorf("added %v, want db back", added)
	}
	var n int
	q.st.DB().QueryRow(`SELECT COUNT(*) FROM services`).Scan(&n)
	if n != 2 {
		t.Errorf("%d service rows, want the same 2", n)
	}
}

// A sync that changes nothing says nothing.
func TestASteadySyncIsQuiet(t *testing.T) {
	q := NewPending()
	d := []Declared{declared("app", "example.com/app", "1")}
	q.SyncServices(d, running("app"), now)
	added, removed, _ := q.SyncServices(d, running("app"), now)
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("added %v, removed %v on a sync that changed nothing", added, removed)
	}
}

// The image and tag are the compose file's. grafana on optiplex: its tag had
// moved on, docker's container list reported it by image ID, and the sync
// recorded "sha256" as its image. Every apply then pulled sha256@sha256:....
// The sync now records what the file declares, and repairs such a row.
func TestTheSyncRecordsTheImageAndTagTheFileDeclares(t *testing.T) {
	q := NewPending()
	q.st.Tx(func(tx *sql.Tx) error {
		_, err := ensureService(tx, "grafana", "grafana", "sha256")
		return err
	})

	q.SyncServices([]Declared{declared("grafana", "grafana/grafana", "latest")}, running("grafana"), now)
	if image, tag := serviceImage(t, q, "grafana"); image != "grafana/grafana" || tag != "latest" {
		t.Errorf("recorded %s:%s, want grafana/grafana:latest", image, tag)
	}

	// The file changes stream: the sync follows it.
	q.SyncServices([]Declared{declared("grafana", "grafana/grafana", "11")}, running("grafana"), now)
	if _, tag := serviceImage(t, q, "grafana"); tag != "11" {
		t.Errorf("recorded tag %s, want 11", tag)
	}
}

// What the snapshot offers "Check…" is the services with a container, as the
// last sync saw them: one declared but stopped is not offered.
func TestRunningIsTheLastSyncsContainers(t *testing.T) {
	q := NewPending()
	q.SyncServices([]Declared{declared("app", "a", "1"), declared("db", "d", "1")}, running("app"), now)
	if got := q.Running(); !got["app"] || got["db"] {
		t.Errorf("running = %v, want app only", got)
	}
}

// An entry carries its image. A release is of one image, and the service's
// row is the sync's to rewrite; reading the image through it is how grafana's
// entry came to pull "sha256".
func TestAnEntryKeepsTheImageItWasFoundFor(t *testing.T) {
	q := NewPending()
	q.SyncServices([]Declared{declared("app", "example.com/app", "1")}, running("app"), now)
	e := entry("app", "1.0.0", "1.1.0")
	e.Image = "example.com/app"
	q.Put(e, now)

	q.SyncServices([]Declared{declared("app", "example.com/other", "1")}, running("app"), now)
	if got, _ := q.Get("app"); got.Image != "example.com/app" {
		t.Errorf("the entry reads %q, want the image it was found for", got.Image)
	}
}

// A finding is a release. It creates a service the sync has not seen yet,
// and leaves one it has seen alone.
func TestAFindingDoesNotRewriteItsService(t *testing.T) {
	q := NewPending()
	q.SyncServices([]Declared{declared("app", "example.com/app", "1")}, running("app"), now)
	e := entry("app", "1.0.0", "1.1.0")
	e.Image = "example.com/elsewhere"
	q.Put(e, now)
	if image, _ := serviceImage(t, q, "app"); image != "example.com/app" {
		t.Errorf("the service reads %q after a finding, want the file's", image)
	}

	fresh := entry("new", "1.0.0", "1.1.0")
	fresh.Image = "example.com/new"
	q.Put(fresh, now)
	if image, _ := serviceImage(t, q, "new"); image != "example.com/new" {
		t.Errorf("a service first seen in a finding reads %q", image)
	}
}
