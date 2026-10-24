package watcher

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
)

// serviceState is one service's memory: how far back to look, and what was
// found and not handed on yet.
type serviceState struct {
	Cutoff  time.Time
	Pending map[string]watch.Finding
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
	// st is where this is kept between runs.
	st       *store.Store
	services map[string]serviceState
	// fallback is the cutoff for a service with none recorded.
	fallback time.Time
	// override is set when DUVA_SETTING_SINCE forced the cutoff. Nothing is
	// recorded in that case: an override asks a question, it does not change
	// what the watcher believes.
	override bool
	// back, when set, is how far back a person asked this check to look:
	// each service looks back to whichever is earlier, its own cutoff or
	// this. Unlike an override it is recorded as any check is, because it
	// covered at least everything an ordinary check would have.
	back time.Time
	// why describes where the cutoffs came from, for the log.
	why string
}

// FirstCheck reports whether this service has never been checked completely.
//
// Not the same as "the state file is new": a service added to the compose file
// today is on its first check whatever else has been recorded, and it is the
// one this matters for -- it has no history, so the window is the only thing
// standing between it and being reported at all.
//
// An override is not a first check. DUVA_SETTING_SINCE asks a question against
// a window someone chose, and answering a different one would make the flag
// mean something other than what it says.
func (m *memory) FirstCheck(service string) bool {
	if m.override {
		return false
	}
	s, ok := m.services[service]
	return !ok || s.Cutoff.IsZero()
}

// Cutoff is how far back to look for one service.
func (m *memory) Cutoff(service string) time.Time {
	c := m.fallback
	if !m.override {
		if s, ok := m.services[service]; ok && !s.Cutoff.IsZero() {
			c = s.Cutoff
		}
	}
	// Never later than its own cutoff: asking for a week must not look less
	// far back than an ordinary check would, for a service that has not been
	// checked in longer.
	if !m.back.IsZero() && m.back.Before(c) {
		return m.back
	}
	return c
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
// the log can too. back is how far back a person asked this check to look;
// zero is each service's own cutoff.
func loadMemory(log zerolog.Logger, back time.Duration) (*memory, error) {
	m, err := readMemory(log)
	if err != nil || back == 0 {
		return m, err
	}
	m.back = time.Now().Add(-back)
	m.why += ", and at least " + watch.FormatLookBack(back) + " back, as asked"
	return m, nil
}

func readMemory(log zerolog.Logger) (*memory, error) {
	if memStore == nil {
		return nil, fmt.Errorf("no store to keep what was checked in")
	}
	week := time.Now().Add(-defaultWindow)

	raw, err := settings.String(settings.Since, "")
	if err != nil {
		return nil, err
	}
	if raw != "" {
		m := &memory{st: memStore, services: map[string]serviceState{}, override: true}
		if d, err := time.ParseDuration(raw); err == nil {
			m.fallback, m.why = time.Now().Add(-d), fmt.Sprintf("%s=%s", settings.EnvVar(settings.Since), raw)
			return m, nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, fmt.Errorf(
				"%s is neither a duration (168h) nor an RFC 3339 time", settings.EnvVar(settings.Since))
		}
		m.fallback, m.why = t, settings.EnvVar(settings.Since)
		return m, nil
	}

	m := &memory{st: memStore, services: map[string]serviceState{}, fallback: week}
	rows, err := memStore.DB().Query(`SELECT service, cutoff FROM watch_cutoffs`)
	if err != nil {
		return nil, fmt.Errorf("reading the cutoffs: %w", err)
	}
	for rows.Next() {
		var service, raw string
		if err := rows.Scan(&service, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			// One unreadable line is that service looking back a week, not
			// every service: the same cost as losing it, and no more.
			log.Warn().Msgf("%s: its cutoff %q could not be read, looking back a week", service, raw)
			continue
		}
		s := m.services[service]
		s.Cutoff = at
		m.services[service] = s
	}
	rows.Close()

	held, err := memStore.DB().Query(`SELECT service, tag, finding FROM watch_held`)
	if err != nil {
		return nil, fmt.Errorf("reading what is held: %w", err)
	}
	defer held.Close()
	for held.Next() {
		var service, tag, raw string
		if err := held.Scan(&service, &tag, &raw); err != nil {
			return nil, err
		}
		var f watch.Finding
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			log.Warn().Msgf("%s: a held finding for %s could not be read, and is dropped", service, tag)
			continue
		}
		s := m.services[service]
		if s.Pending == nil {
			s.Pending = map[string]watch.Finding{}
		}
		s.Pending[tag] = f
		m.services[service] = s
	}

	if len(m.services) > 0 {
		m.why = fmt.Sprintf("%d service(s) recorded, the rest looking back a week", len(m.services))
	} else {
		m.why = "first run, looking back a week"
	}
	return m, held.Err()
}

// save writes what the watcher knows.
//
// Atomic: a temp file in the same directory then a rename, so a crash
// mid-write cannot leave a truncated file behind -- which would read as no
// recorded checks and re-report a week of tags.
func (m *memory) save() error {
	if m.override {
		return nil
	}
	return m.st.Tx(func(tx *sql.Tx) error {
		// What is held is replaced whole: the memory is the truth for this
		// run, and a finding handed on since the load must not survive.
		if _, err := tx.Exec(`DELETE FROM watch_held`); err != nil {
			return err
		}
		for _, name := range sortedKeys(m.services) {
			s := m.services[name]
			if !s.Cutoff.IsZero() {
				if _, err := tx.Exec(`INSERT INTO watch_cutoffs (service, cutoff) VALUES (?, ?)
					ON CONFLICT (service) DO UPDATE SET cutoff = excluded.cutoff`,
					name, s.Cutoff.UTC().Format(time.RFC3339Nano)); err != nil {
					return err
				}
			}
			for _, tag := range sortedKeys(s.Pending) {
				raw, err := json.Marshal(s.Pending[tag])
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO watch_held (service, tag, finding) VALUES (?, ?, ?)`,
					name, tag, string(raw)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
