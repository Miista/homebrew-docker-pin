// Package jobs records what duva's jobs did, for the page's Jobs list.
//
// The jobs are where they always were -- the watcher's check, and the queue's
// sync, reconcile and release -- and each says what it did here. Recording
// never fails a job: a run whose record could not be written still ran.
package jobs

import (
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
)

// The jobs, by the name a run is recorded under.
const (
	Check     = "check"
	Sync      = "sync"
	Reconcile = "reconcile"
	Release   = "release"
)

// keep is how many runs of each job are kept.
const keep = 20

// Run is one run of a job.
type Run struct {
	Job     string        `json:"job"`
	Started time.Time     `json:"started"`
	Took    time.Duration `json:"took"`
	// Summary is one line: what it did, or why it could not.
	Summary string `json:"summary"`
	// Output is what it said while it ran, a line each.
	Output []string `json:"output,omitempty"`
	Failed bool     `json:"failed,omitempty"`
}

// Status is a job as the page shows it: its last run, and when it runs next.
type Status struct {
	Job  string `json:"job"`
	Last *Run   `json:"last,omitempty"`
	// FailedInARow is how many of its latest runs failed, back to the last
	// one that did not. One failure says little -- a registry or a network
	// had a moment -- so health only counts a run of them.
	FailedInARow int `json:"failed_in_a_row,omitempty"`
	// Next is when it runs on its own next; zero when that is not known.
	Next time.Time `json:"next"`
}

// Tick is the jobs that run together, in this order, as one tick: running
// one by hand runs all three, since each works on what the one before it
// left.
var Tick = []string{Sync, Reconcile, Release}

// Statuses is every job, in the order they are listed, with its last run.
func Statuses(st *store.Store, nextCheck, nextTick time.Time) []Status {
	last := Last(st)
	streaks := failedInARow(st)
	out := []Status{}
	for _, job := range append([]string{Check}, Tick...) {
		s := Status{Job: job, Next: nextTick, FailedInARow: streaks[job]}
		if job == Check {
			s.Next = nextCheck
		}
		if r, ok := last[job]; ok {
			r := r
			s.Last = &r
		}
		out = append(out, s)
	}
	return out
}

// failedInARow is, per job, how many of its latest runs failed in a row.
func failedInARow(st *store.Store) map[string]int {
	out := map[string]int{}
	if st == nil {
		return out
	}
	rows, err := st.DB().Query(`SELECT job, failed FROM job_runs ORDER BY job, id DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	done := map[string]bool{}
	for rows.Next() {
		var job string
		var failed int
		if rows.Scan(&job, &failed) != nil || done[job] {
			continue
		}
		if failed == 0 {
			done[job] = true
			continue
		}
		out[job]++
	}
	return out
}

// Log collects one run's lines as it goes.
type Log struct {
	mu    sync.Mutex
	lines []string
}

// Add appends a line.
func (l *Log) Add(line string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
}

// Run makes a Log a zerolog hook, so a job's own log lines are its output:
// log.Hook(l) while it runs, and what it would say anyway is recorded.
func (l *Log) Run(_ *zerolog.Event, level zerolog.Level, msg string) {
	if level >= zerolog.WarnLevel {
		msg = "! " + msg
	}
	l.Add(msg)
}

// Lines is what was said, in order.
func (l *Log) Lines() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// Record writes a run, and trims the job's history to the last few.
func Record(st *store.Store, r Run) {
	if st == nil {
		return
	}
	failed := 0
	if r.Failed {
		failed = 1
	}
	st.DB().Exec(`INSERT INTO job_runs (job, started_at, took_ms, summary, output, failed) VALUES (?, ?, ?, ?, ?, ?)`,
		r.Job, r.Started.UTC().Format(time.RFC3339), r.Took.Milliseconds(), r.Summary,
		strings.Join(r.Output, "\n"), failed)
	st.DB().Exec(`DELETE FROM job_runs WHERE job = ? AND id NOT IN
		(SELECT id FROM job_runs WHERE job = ? ORDER BY id DESC LIMIT ?)`, r.Job, r.Job, keep)
}

// Last is each job's latest run. A job that has not run yet is absent.
func Last(st *store.Store) map[string]Run {
	out := map[string]Run{}
	if st == nil {
		return out
	}
	rows, err := st.DB().Query(`SELECT job, started_at, took_ms, summary, output, failed FROM job_runs
		WHERE id IN (SELECT MAX(id) FROM job_runs GROUP BY job)`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r Run
		var started, output string
		var took int64
		var failed int
		if rows.Scan(&r.Job, &started, &took, &r.Summary, &output, &failed) != nil {
			continue
		}
		r.Started, _ = time.Parse(time.RFC3339, started)
		r.Took = time.Duration(took) * time.Millisecond
		if output != "" {
			r.Output = strings.Split(output, "\n")
		}
		r.Failed = failed != 0
		out[r.Job] = r
	}
	return out
}
