package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repository is required, and its absence is loud.
//
// No default. Nothing can guess the path the repository has on the host, and
// an updater pointed at the wrong one behaves exactly like one pointed at the
// right one -- until a commit lands somewhere nobody expected.
func TestTheRepositoryIsRequired(t *testing.T) {
	for _, sub := range []string{"", "optiplex"} {
		old := repoDir
		repoDir = ""
		t.Cleanup(func() { repoDir = old })
		t.Setenv("DUVA_COMPOSE_DIR", sub)

		_, err := projectFile()
		if err == nil {
			t.Fatalf("compose dir %q: no error, so it would start against a path nobody chose", sub)
		}
		if !strings.Contains(err.Error(), "DUVA_REPO") {
			t.Errorf("the error does not name DUVA_REPO: %v", err)
		}
	}
}

// The path is {repo}/{compose dir}, and the compose directory is left out when
// the compose file is at the root of the repository -- the ordinary case for a
// repository holding one project.
func TestTheComposeDirectoryIsOptional(t *testing.T) {
	repo := t.TempDir()
	want := filepath.Join(repo, "docker-compose.yml")
	os.WriteFile(want, []byte("services: {}\n"), 0o644)

	old := repoDir
	repoDir = repo
	t.Cleanup(func() { repoDir = old })
	t.Setenv("DUVA_COMPOSE_DIR", "")

	got, err := projectFile()
	if err != nil {
		t.Fatalf("projectFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want the repository's own compose file %q", got, want)
	}
}

// Left out still means "at the root", not "look around for one": a repository
// whose compose file is in a subdirectory must be told so.
func TestLeavingItOutDoesNotSearch(t *testing.T) {
	repo := t.TempDir()
	sub := filepath.Join(repo, "optiplex")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "docker-compose.yml"), []byte("services: {}\n"), 0o644)

	old := repoDir
	repoDir = repo
	t.Cleanup(func() { repoDir = old })
	t.Setenv("DUVA_COMPOSE_DIR", "")

	if got, err := projectFile(); err == nil {
		t.Errorf("found %q by searching, when the root has no compose file", got)
	}
}

// The project is found by descending, never by climbing.
//
// The repository is mounted and the project is a directory inside it, so the
// subdirectory descends to it. What must not happen is a walk upwards: from
// the project that would reach the repository root, and from the root it would
// leave the mount -- either way answering with a compose file this stage was
// not pointed at, which looks like a right answer.
func TestTheProjectIsFoundByDescendingOnly(t *testing.T) {
	repo := t.TempDir()
	// A repository-shaped tree: a compose file at the root AND one in the
	// project, so a walk in either direction has something to find.
	os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services: {}\n"), 0o644)
	project := filepath.Join(repo, "optiplex")
	os.MkdirAll(project, 0o755)
	want := filepath.Join(project, "docker-compose.yml")
	os.WriteFile(want, []byte("services: {}\n"), 0o644)

	old := repoDir
	repoDir = repo
	t.Cleanup(func() { repoDir = old })

	t.Setenv("DUVA_COMPOSE_DIR", "optiplex")
	got, err := projectFile()
	if err != nil {
		t.Fatalf("projectFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want the project's own file %q -- not the repository root's", got, want)
	}
}

// A subdirectory that climbs out is refused rather than silently resolving
// somewhere else: filepath.Join cleans ".." away without complaining, which is
// exactly how it would go unnoticed.
func TestASubdirectoryCannotEscape(t *testing.T) {
	old := repoDir
	repoDir = t.TempDir()
	t.Cleanup(func() { repoDir = old })

	for _, sub := range []string{"../etc", "..", "/absolute", "optiplex/../.."} {
		t.Setenv("DUVA_COMPOSE_DIR", sub)
		if got, err := projectFile(); err == nil {
			t.Errorf("subdirectory %q was accepted, resolving to %q", sub, got)
		}
	}
}

// With no compose file where it was pointed, it says so rather than finding
// one above.
func TestAMissingProjectIsAnError(t *testing.T) {
	repo := t.TempDir()
	// One at the root, which a walk upwards would find.
	os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services: {}\n"), 0o644)
	os.MkdirAll(filepath.Join(repo, "optiplex"), 0o755)

	old := repoDir
	repoDir = repo
	t.Cleanup(func() { repoDir = old })

	t.Setenv("DUVA_COMPOSE_DIR", "optiplex")
	if got, err := projectFile(); err == nil {
		t.Errorf("found %q by climbing out of the project it was pointed at", got)
	}
}
