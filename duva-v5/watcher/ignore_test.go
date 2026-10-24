package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// duva.ignore: true is not checked at all. Strict, as the queue reads it: a
// value that is neither true nor false is an error, not a guess.
func TestAnIgnoredServiceHasNoRules(t *testing.T) {
	if _, _, err := rulesFrom(map[string]string{"duva.ignore": "true"}); !errors.Is(err, errIgnored) {
		t.Errorf("true: err = %v, want errIgnored", err)
	}
	if _, _, err := rulesFrom(map[string]string{"duva.ignore": "false"}); err != nil {
		t.Errorf("false: err = %v", err)
	}
	if _, _, err := rulesFrom(map[string]string{"duva.ignore": "yes"}); err == nil || errors.Is(err, errIgnored) {
		t.Errorf("yes: err = %v, want an error that is not errIgnored", err)
	}
}

// The watcher looks only at the pinned services that are not ignored.
func TestAnIgnoredServiceIsNotWatched(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(root, []byte(`
services:
  broker:
    image: redis:7.4-alpine@sha256:aaaa
    container_name: paperless-broker
  app:
    image: example.com/app:1.0.0@sha256:bbbb
    container_name: app
`), 0o644); err != nil {
		t.Fatal(err)
	}
	labels := func(container string) (map[string]string, bool, error) {
		if container == "paperless-broker" {
			return map[string]string{"duva.ignore": "true"}, true, nil
		}
		return map[string]string{}, true, nil
	}
	services, err := readServices(root, zerolog.Nop(), labels)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].Name != "app" {
		t.Errorf("watched %+v, want app alone", services)
	}
}
