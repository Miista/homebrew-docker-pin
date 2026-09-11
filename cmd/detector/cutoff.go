package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
)

// The detector's entire state: when it last looked.
//
// One timestamp rather than a per-service record of what each image pointed
// at. That is the difference between losing this file costing a single noisy
// run and costing a full re-listing of every tag of every repository -- which
// is what a per-service baseline costs, and what makes such a baseline
// dangerous to depend on.
type state struct {
	// LastCheck is the cutoff: tags published after it are new.
	LastCheck time.Time `json:"last_check"`
}

// defaultWindow is how far back a first run looks.
//
// Not "the beginning of time": a detector with no memory that reported every
// tag ever published would bury the first run in noise nobody would read. A
// week is long enough to catch what a fresh install should know about and
// short enough to stay legible.
const defaultWindow = 7 * 24 * time.Hour

// loadCutoff works out what "since" means for this run, and says where the
// answer came from so the log can too.
func loadCutoff(log zerolog.Logger) (time.Time, string, error) {
	// An override wins, for asking "what appeared in the last week" without
	// disturbing the recorded cutoff -- which is how you re-run a check
	// after a failure without having to reason about state.
	if raw := os.Getenv("DETECTOR_SINCE"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			return time.Now().Add(-d), fmt.Sprintf("DETECTOR_SINCE=%s", raw), nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, "", fmt.Errorf(
				"DETECTOR_SINCE=%q is neither a duration (168h) nor an RFC 3339 time", raw)
		}
		return t, "DETECTOR_SINCE", nil
	}

	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return time.Now().Add(-defaultWindow), "first run, looking back a week", nil
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("reading %s: %w", stateFile, err)
	}

	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		// A state file that cannot be read is not a reason to report
		// everything ever published. It falls back to the default window and
		// says so, which is noisy for one run rather than unreadable.
		log.Warn().Msgf("%s could not be read (%v), falling back to the last week", stateFile, err)
		return time.Now().Add(-defaultWindow), "unreadable state, looking back a week", nil
	}
	if s.LastCheck.IsZero() {
		return time.Now().Add(-defaultWindow), "no recorded check, looking back a week", nil
	}
	return s.LastCheck, "the last check", nil
}

// saveCutoff records when this check ran.
//
// Atomic: a temp file in the same directory then a rename, so a crash
// mid-write cannot leave a truncated cutoff behind -- which would read as a
// zero time and re-report a week of tags.
func saveCutoff(at time.Time) error {
	data, err := json.MarshalIndent(state{LastCheck: at.UTC()}, "", "  ")
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
