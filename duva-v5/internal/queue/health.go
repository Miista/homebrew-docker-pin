package queue

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/jobs"
)

// The levels of health, from the reader's side: ok needs nothing; degraded is
// worth a look, but proves nothing on its own; unhealthy is duva itself not
// working. Only unhealthy is a 503 -- the code a container runtime, and
// autoheal behind it, acts on by restarting. A restart fixes a stuck process,
// not a registry that keeps timing out.
const (
	HealthOK        = "ok"
	HealthDegraded  = "degraded"
	HealthUnhealthy = "unhealthy"
)

// FailingRuns is how many runs of a job in a row make it worth a look. One
// failure is a registry or a network having a moment.
const FailingRuns = 3

// Health is duva's state for a monitor: the level, and each reason for it.
type Health struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons,omitempty"`
}

// Worse is the more serious of two levels.
func Worse(a, b string) string {
	rank := map[string]int{HealthOK: 0, HealthDegraded: 1, HealthUnhealthy: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// CheckStore reads from the store and takes its write lock without writing:
// a store that cannot do both is a duva that cannot queue or record anything.
func (q *Pending) CheckStore() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	db := q.st.DB()
	var one int
	if err := db.QueryRow(`SELECT 1 FROM queue_entries LIMIT 1`).Scan(&one); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("writing: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE queue_entries SET id = id WHERE 0`); err != nil {
		return fmt.Errorf("writing: %w", err)
	}
	return nil
}

// Health is this host's: unhealthy if its store cannot be used, degraded if
// a job has failed FailingRuns times in a row, ok otherwise. What waits, what
// is deferred, and an uncommitted compose file are states, not failures.
func (s *Server) Health() Health {
	h := Health{Status: HealthOK}
	if err := s.Queue.CheckStore(); err != nil {
		h.Status = HealthUnhealthy
		h.Reasons = append(h.Reasons, "store: "+err.Error())
	}
	var statuses []jobs.Status
	if s.Jobs != nil {
		statuses = s.Jobs()
	}
	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].Job < statuses[j].Job })
	for _, j := range statuses {
		if j.FailedInARow < FailingRuns {
			continue
		}
		h.Status = Worse(h.Status, HealthDegraded)
		reason := fmt.Sprintf("%s: failed %d times in a row", j.Job, j.FailedInARow)
		if j.Last != nil && j.Last.Summary != "" {
			reason += " (last: " + j.Last.Summary + ")"
		}
		h.Reasons = append(h.Reasons, reason)
	}
	return h
}
