package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// store is where a queue survives a restart.
//
// The queue is what is waiting on a person, and a restart is not a decision
// about any of it. Without this a deploy silently dropped every pending
// approval -- and, because an entry restored is an entry that is not new,
// re-announced every one of them the next time the watcher re-sent it.
//
// Empty path means no store, which is how a queue runs with no volume: it
// keeps everything in memory exactly as before.
type store struct {
	path string
}

// load reads what was waiting. A missing file is not an error: it is the
// first run.
func (s store) load() (map[string]Entry, error) {
	if s.path == "" {
		return map[string]Entry{}, nil
	}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return map[string]Entry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	var entries map[string]Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		// A file that cannot be read is not a reason to refuse to start. The
		// queue rebuilds itself from the next check -- noisily, because
		// everything in it looks new again, but a queue that starts is worth
		// more than one that is right about what it already told someone.
		return nil, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	if entries == nil {
		entries = map[string]Entry{}
	}
	return entries, nil
}

// save writes what is waiting, atomically.
//
// Temp file then rename, like the watcher's: a crash partway through a write
// must not leave a file that parses as half a queue.
func (s store) save(entries map[string]Entry) error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".queue-*.json")
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
	return os.Rename(tmp.Name(), s.path)
}
