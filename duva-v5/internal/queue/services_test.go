package queue

import (
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

func TestTheSyncMirrorsTheContainers(t *testing.T) {
	q := NewPending()
	added, removed, err := q.SyncServices([]Container{
		{Name: "app", Service: "app", Image: "example.com/app:1.0.0@sha256:abc"},
		{Name: "db", Service: "db", Image: "postgres:17"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || len(removed) != 0 {
		t.Errorf("added %v, removed %v", added, removed)
	}

	// db goes.
	_, removed, _ = q.SyncServices([]Container{{Name: "app", Service: "app", Image: "example.com/app:1.0.0"}}, now)
	if len(removed) != 1 || removed[0] != "db" {
		t.Errorf("removed %v, want db", removed)
	}
	if got := serviceRows(t, q); !got["app"] || got["db"] {
		t.Errorf("services = %v, want app enabled and db disabled but kept", got)
	}

	// db comes back: the same row.
	added, _, _ = q.SyncServices([]Container{
		{Name: "app", Service: "app", Image: "example.com/app:1.0.0"},
		{Name: "db", Service: "db", Image: "postgres:17"},
	}, now.Add(time.Hour))
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
	cs := []Container{{Name: "app", Service: "app", Image: "example.com/app:1.0.0"}}
	q.SyncServices(cs, now)
	added, removed, _ := q.SyncServices(cs, now)
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("added %v, removed %v on a sync that changed nothing", added, removed)
	}
}

func TestRepoOf(t *testing.T) {
	for in, want := range map[string]string{
		"example.com/app:1.0.0@sha256:abc": "example.com/app",
		"postgres:17":                      "postgres",
		"localhost:5555/app:1.0.0":         "localhost:5555/app",
		"localhost:5555/app":               "localhost:5555/app",
		"app@sha256:abc":                   "app",
	} {
		if got := repoOf(in); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", in, got, want)
		}
	}
}
