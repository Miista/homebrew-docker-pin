package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	"github.com/Miista/homebrew-docker-pin/internal/schedule"
)

// stateFilePath returns where the last-notified-tag-per-service map is
// persisted, overridable via TAGWATCH_STATE_FILE for tests and non-default
// mounts. The default assumes a volume mounted at /data, matching diun's
// own convention.
func stateFilePath() string {
	if p := os.Getenv("TAGWATCH_STATE_FILE"); p != "" {
		return p
	}
	return "/data/tagwatch.json"
}

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

// compileTagFilters compiles a service's tags/exclude regexes.
func compileTagFilters(svc schedule.Service) (include, exclude *regexp.Regexp, err error) {
	include, err = regexp.Compile(svc.Tags)
	if err != nil {
		return nil, nil, err
	}
	if svc.Exclude != "" {
		exclude, err = regexp.Compile(svc.Exclude)
		if err != nil {
			return nil, nil, err
		}
	}
	return include, exclude, nil
}
