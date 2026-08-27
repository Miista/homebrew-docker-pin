package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// stateFile is where the last-notified-tag-per-service map is persisted:
// a fixed path on the /data volume mount, matching diun's own convention.
// A package variable only so tests can point it at a fixture.
var stateFile = "/data/duva.json"

// loadState reads the service -> last-notified-tag map. A missing file is
// not an error — it means nothing has been notified about yet.
func loadState(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st map[string]string
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st == nil {
		st = map[string]string{}
	}
	return st, nil
}

// saveState writes the service -> last-notified-tag map, creating its
// parent directory if needed.
func saveState(path string, st map[string]string) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
