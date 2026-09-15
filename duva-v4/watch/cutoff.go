package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/watch"
)

// The watcher's state: two things, per service, and they are independent.
//
// **Cutoff** is how far back to look. It advances whenever a service was
// checked successfully, whether or not anything was handed on -- looking is
// not publishing, and conflating them is what made one unreachable registry
// cost every other service a weekly re-scan.
//
// **Pending** is what was found and has not been handed on. A watcher with
// no webhook configured is a legitimate way to run: findings go to the log
// and wait here, and the moment a webhook exists they are sent. So is a
// webhook that is down -- the findings are not lost, they are just still in
// the outbox.
//
// Per service, because both failures are per service. One image whose
// registry refuses says nothing about whether another was checked.
//
// Still cheap to lose. A cutoff is one timestamp and a pending finding is the
// handful of fields already on the wire -- not a baseline of what each image
// pointed at, which is what would make losing this file cost a full re-listing
// of every repository rather than one noisy run.
type state struct {
	// Services is what is known per compose service.
	Services map[string]serviceState `json:"services"`

	// LastCheck is the old single cutoff, read but never written.
	//
	// Kept so an existing state file still means something: a service with no
	// line of its own starts from it rather than from a week ago. Dropped on
	// the first save.
	LastCheck time.Time `json:"last_check,omitempty"`
}

type serviceState struct {
	// Cutoff is when this service was last checked completely.
	Cutoff time.Time `json:"cutoff"`
	// Pending is what was found for it and not yet handed on, keyed by tag.
	//
	// Keyed by tag rather than appended, so a moving tag that moves forty
	// times during an outage leaves one entry rather than forty. That matches
	// what the gate does with them anyway -- its queue is keyed by service,
	// so all but the last would be discarded on arrival.
	Pending map[string]watch.Finding `json:"pending,omitempty"`
}

// defaultWindow is how far back a service with no recorded check looks.
//
// Not "the beginning of time": a watcher with no memory that reported every
// tag ever published would bury the first run in noise nobody would read. A
// week is long enough to catch what a fresh install should know about and
// short enough to stay legible.
const defaultWindow = 7 * 24 * time.Hour

// memory is the state a run works against, and writes back.
type memory struct {
	services map[string]serviceState
	// fallback is the cutoff for a service with none recorded.
	fallback time.Time
	// override is set when DUVA_WATCH_SINCE forced the cutoff. Nothing is
	// recorded in that case: an override asks a question, it does not change
	// what the watcher believes.
	override bool
	// why describes where the cutoffs came from, for the log.
	why string
}

// Cutoff is how far back to look for one service.
func (m *memory) Cutoff(service string) time.Time {
	if !m.override {
		if s, ok := m.services[service]; ok && !s.Cutoff.IsZero() {
			return s.Cutoff
		}
	}
	return m.fallback
}

// Checked records that a service was looked at completely, up to `at`.
func (m *memory) Checked(service string, at time.Time) {
	s := m.services[service]
	s.Cutoff = at.UTC()
	m.services[service] = s
}

// Hold keeps a finding that has not been handed on.
func (m *memory) Hold(f watch.Finding) {
	s := m.services[f.Service]
	if s.Pending == nil {
		s.Pending = map[string]watch.Finding{}
	}
	s.Pending[f.Tag] = f
	m.services[f.Service] = s
}

// Handed drops a finding that has been handed on.
func (m *memory) Handed(f watch.Finding) {
	s, ok := m.services[f.Service]
	if !ok || s.Pending == nil {
		return
	}
	delete(s.Pending, f.Tag)
	if len(s.Pending) == 0 {
		s.Pending = nil
	}
	m.services[f.Service] = s
}

// Held is everything waiting to be handed on, oldest service first by name so
// the order does not change between runs.
func (m *memory) Held() []watch.Finding {
	var out []watch.Finding
	for _, name := range sortedKeys(m.services) {
		for _, tag := range sortedKeys(m.services[name].Pending) {
			out = append(out, m.services[name].Pending[tag])
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// A stable order, so a log read twice says the same thing twice.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// loadMemory reads what the watcher knows, and says where it came from so
// the log can too.
func loadMemory(log zerolog.Logger) (*memory, error) {
	week := time.Now().Add(-defaultWindow)

	// An override wins, for asking "what appeared in the last week" without
	// disturbing what is recorded -- which is how you re-run a check after a
	// failure without having to reason about state.
	if raw := os.Getenv("DUVA_WATCH_SINCE"); raw != "" {
		m := &memory{services: map[string]serviceState{}, override: true}
		if d, err := time.ParseDuration(raw); err == nil {
			m.fallback, m.why = time.Now().Add(-d), fmt.Sprintf("DUVA_WATCH_SINCE=%s", raw)
			return m, nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, fmt.Errorf(
				"DUVA_WATCH_SINCE=%q is neither a duration (168h) nor an RFC 3339 time", raw)
		}
		m.fallback, m.why = t, "DUVA_WATCH_SINCE"
		return m, nil
	}

	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return &memory{services: map[string]serviceState{}, fallback: week,
			why: "first run, looking back a week"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", stateFile, err)
	}

	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		// A state file that cannot be read is not a reason to report
		// everything ever published. It falls back to the default window and
		// says so, which is noisy for one run rather than unreadable.
		log.Warn().Msgf("%s could not be read (%v), falling back to the last week", stateFile, err)
		return &memory{services: map[string]serviceState{}, fallback: week,
			why: "unreadable state, looking back a week"}, nil
	}

	m := &memory{services: s.Services, fallback: week}
	if m.services == nil {
		m.services = map[string]serviceState{}
	}
	switch {
	case !s.LastCheck.IsZero():
		// An older file, from when one line covered every service. It seeds
		// anything without a line of its own, so upgrading does not re-report
		// a week.
		m.fallback = s.LastCheck
		m.why = fmt.Sprintf("%d service(s) recorded, the rest from the last whole-run check", len(m.services))
	case len(m.services) > 0:
		m.why = fmt.Sprintf("%d service(s) recorded, the rest looking back a week", len(m.services))
	default:
		m.why = "no recorded checks, looking back a week"
	}
	return m, nil
}

// save writes what the watcher knows.
//
// Atomic: a temp file in the same directory then a rename, so a crash
// mid-write cannot leave a truncated file behind -- which would read as no
// recorded checks and re-report a week of tags.
func (m *memory) save() error {
	if m.override {
		// An override asked a question; it does not answer for the watcher's
		// memory.
		return nil
	}

	// LastCheck is deliberately not carried forward: once services have their
	// own lines, a single fallback would be a second answer to the same
	// question and the two would drift.
	data, err := json.MarshalIndent(state{Services: m.services}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(stateFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".detector-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), stateFile)
}
