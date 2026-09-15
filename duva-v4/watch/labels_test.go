package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectWith(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// diun's namespace, not duva's: every service on both hosts already carries
// these, and reading the other one would leave them all unconstrained.
func TestReadsDiunsLabels(t *testing.T) {
	file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
    labels:
      diun.include_tags: '^\d+\.\d+\.\d+$'
      diun.exclude_tags: '(alpha|beta|rc)'
`)
	include, exclude, err := tagRules(file, "app")
	if err != nil {
		t.Fatalf("tagRules: %v", err)
	}
	if include == nil || !include.MatchString("1.2.3") || include.MatchString("nightly") {
		t.Errorf("include did not compile to the right pattern: %v", include)
	}
	if exclude == nil || !exclude.MatchString("1.2.3-rc1") {
		t.Errorf("exclude did not compile to the right pattern: %v", exclude)
	}
}

// No labels is no constraint, not an error: a service that never needed one
// should still be watched.
func TestNoLabelsMeansNoConstraint(t *testing.T) {
	file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
`)
	include, exclude, err := tagRules(file, "app")
	if err != nil {
		t.Fatalf("tagRules: %v", err)
	}
	if include != nil || exclude != nil {
		t.Errorf("got %v/%v, want no constraint", include, exclude)
	}
}

// A pattern that does not compile is an error. Ignoring it would widen what
// qualifies rather than narrow it -- the operator wrote a constraint and
// would get none.
func TestAnUncompilablePatternIsAnError(t *testing.T) {
	file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
    labels:
      diun.include_tags: '^[unclosed'
`)
	_, _, err := tagRules(file, "app")
	if err == nil {
		t.Fatal("want an error for a pattern that does not compile")
	}
	if !strings.Contains(err.Error(), "include_tags") {
		t.Errorf("the error should name the label, got %v", err)
	}
}

func TestAnUncompilableExcludeIsAlsoAnError(t *testing.T) {
	file := projectWith(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:abc
    container_name: my-app
    labels:
      diun.exclude_tags: '(unclosed'
`)
	if _, _, err := tagRules(file, "app"); err == nil {
		t.Fatal("want an error for a pattern that does not compile")
	}
}
