package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A commit convention belongs to the repository, not to duva. The default is
// what this project uses; a template replaces it wholesale.

func TestCommitSubject_Default(t *testing.T) {
	got, err := commitSubject(defaultCommitTemplate, commitFields{
		Container: "radarr",
		OldVersion: "1.2.0", NewVersion: "1.3.0",
	})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if want := "radarr: 1.2.0 -> 1.3.0"; got != want {
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
	tmpl := "{{.Container}} {{.Image}} {{.OldVersion}} {{.NewVersion}} " +
		"{{.OldDigest}} {{.NewDigest}}"
	got, err := commitSubject(tmpl, commitFields{
		Container: "c", Image: "i",
		OldVersion: "1", NewVersion: "2", OldDigest: "d1", NewDigest: "d2",
	})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if want := "c i 1 2 d1 d2"; got != want {
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
	if _, err := commitSubject("{{.Container}}", commitFields{Container: ""}); err == nil {
		t.Error("a template that produces nothing should fail")
	}
}

// No template at all means the default. A caller that does not care about the
// convention should get this repository's, not a commit with no subject.
func TestCommitSubject_NoTemplateUsesTheDefault(t *testing.T) {
	got, err := commitSubject("", commitFields{
		Container: "radarr",
		OldVersion: "1.2.0", NewVersion: "1.3.0",
	})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if want := "radarr: 1.2.0 -> 1.3.0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Reading the template: most stacks mount nothing, so a missing file is the
// normal case rather than a problem.

func TestLoadCommitTemplate_MissingFileIsTheDefault(t *testing.T) {
	commitTemplatePath = filepath.Join(t.TempDir(), "not-there")

	got, err := loadCommitTemplate()
	if err != nil {
		t.Fatalf("a missing template is the common case, not an error: %v", err)
	}
	if got != defaultCommitTemplate {
		t.Errorf("got %q, want the default", got)
	}
}

func TestLoadCommitTemplate_ReadsWhatIsMounted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commit-template")
	// With a trailing newline, as any editor would leave it.
	if err := os.WriteFile(path, []byte("bump {{.Container}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitTemplatePath = path

	got, err := loadCommitTemplate()
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if got != "bump {{.Container}}" {
		t.Errorf("got %q, want the trailing newline trimmed", got)
	}
}

// An empty file is a mistake rather than a preference: someone mounted a
// template and it is not there. Falling back to the default would hide that.
func TestLoadCommitTemplate_EmptyFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commit-template")
	if err := os.WriteFile(path, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitTemplatePath = path

	if _, err := loadCommitTemplate(); err == nil {
		t.Error("an empty template file should be reported, not silently ignored")
	}
}

// Host is deliberately not a field: the actor needs the host for nothing, and
// a template wanting one puts it in as literal text. A template that names it
// must fail loudly rather than render "<no value>" into history.
func TestCommitSubject_HostIsNotAField(t *testing.T) {
	_, err := commitSubject("{{.Host}}/{{.Container}}: x", commitFields{Container: "radarr"})
	if err == nil {
		t.Fatal("a template naming a field that does not exist was accepted")
	}
}

// Which is how a host prefix is actually written: as text.
func TestCommitSubject_AHostPrefixIsLiteralText(t *testing.T) {
	got, err := commitSubject("optiplex/{{.Container}}: update to {{.NewVersion}}",
		commitFields{Container: "radarr", NewVersion: "5.28.0"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "optiplex/radarr: update to 5.28.0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The default handles both kinds: a version change reads as one, and a digest
// move reads as a move rather than as "latest -> latest".
func TestDefaultTemplate_ReadsBothKinds(t *testing.T) {
	version, err := commitSubject("", commitFields{
		Container: "cloudflared", OldVersion: "2026.8.3", NewVersion: "2026.9.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "cloudflared: 2026.8.3 -> 2026.9.1"; version != want {
		t.Errorf("version change = %q, want %q", version, want)
	}

	moved, err := commitSubject("", commitFields{
		Container: "bazarr", OldVersion: "latest", NewVersion: "latest",
		NewDigest: "sha256:d24bd004",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "bazarr: latest moved to sha256:d24bd004"; moved != want {
		t.Errorf("digest move = %q, want %q", moved, want)
	}
}
