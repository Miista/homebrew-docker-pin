package main

import (
	"strings"
	"testing"
)

// A commit convention belongs to the repository, not to duva. The default is
// what this project uses; a template replaces it wholesale.

func TestCommitSubject_Default(t *testing.T) {
	got, err := commitSubject(defaultCommitTemplate, commitFields{
		Host: "optiplex", Container: "radarr",
		OldVersion: "1.2.0", NewVersion: "1.3.0",
	})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if want := "optiplex/radarr: 1.2.0 -> 1.3.0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCommitSubject_CustomTemplate(t *testing.T) {
	got, err := commitSubject("chore(deps): bump {{.Container}} to {{.NewVersion}}",
		commitFields{Container: "paperless-db", NewVersion: "16.2"})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if want := "chore(deps): bump paperless-db to 16.2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Every field a template may refer to has to resolve, or the placeholder
// itself ends up in the history of every host duva runs on.
func TestCommitSubject_AllFieldsResolve(t *testing.T) {
	tmpl := "{{.Host}} {{.Container}} {{.Image}} {{.OldVersion}} {{.NewVersion}} " +
		"{{.OldDigest}} {{.NewDigest}}"
	got, err := commitSubject(tmpl, commitFields{
		Host: "h", Container: "c", Image: "i",
		OldVersion: "1", NewVersion: "2", OldDigest: "d1", NewDigest: "d2",
	})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if want := "h c i 1 2 d1 d2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A misspelled field must stop the commit rather than be written literally:
// "{{.Servce}}: 1.0 -> 2.0" in a repository's history is worse than a failed
// update, because it is permanent and looks deliberate.
func TestCommitSubject_UnknownFieldIsAnError(t *testing.T) {
	if _, err := commitSubject("{{.Servce}}: {{.NewVersion}}", commitFields{}); err == nil {
		t.Error("a template naming a field that does not exist should fail")
	}
}

func TestCommitSubject_MalformedTemplateIsAnError(t *testing.T) {
	if _, err := commitSubject("{{.Container", commitFields{}); err == nil {
		t.Error("a template that does not parse should fail")
	}
}

// A subject is one line. A template with a newline would otherwise give a
// commit a body nobody wrote.
func TestCommitSubject_IsOneLine(t *testing.T) {
	got, err := commitSubject("{{.Container}}: updated\n\nand some body text",
		commitFields{Container: "app"})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("a subject should be one line, got %q", got)
	}
	if got != "app: updated" {
		t.Errorf("got %q", got)
	}
}

// A template that renders to nothing would commit with an empty subject,
// which git accepts and nobody can read.
func TestCommitSubject_EmptyResultIsAnError(t *testing.T) {
	if _, err := commitSubject("{{.Host}}", commitFields{Host: ""}); err == nil {
		t.Error("a template that produces nothing should fail")
	}
}

// No template at all means the default. A caller that does not care about the
// convention should get this repository's, not a commit with no subject.
func TestCommitSubject_NoTemplateUsesTheDefault(t *testing.T) {
	got, err := commitSubject("", commitFields{
		Host: "optiplex", Container: "radarr",
		OldVersion: "1.2.0", NewVersion: "1.3.0",
	})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if want := "optiplex/radarr: 1.2.0 -> 1.3.0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
