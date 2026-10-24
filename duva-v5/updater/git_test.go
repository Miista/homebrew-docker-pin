package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Against real git: what these hold is git's behaviour under the arguments
// realGit passes, which a fake cannot say anything about.

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo is a clone of a bare origin with two compose files committed, and the
// clone's commit identity set, as a host's checkout has.
func repo(t *testing.T) (origin, clone string) {
	t.Helper()
	root := t.TempDir()
	origin, clone = filepath.Join(root, "origin.git"), filepath.Join(root, "clone")
	git(t, root, "init", "-q", "--bare", "-b", "main", origin)
	git(t, root, "clone", "-q", origin, clone)
	git(t, clone, "config", "user.name", "t")
	git(t, clone, "config", "user.email", "t@t")
	write(t, filepath.Join(clone, "a", "compose.yml"), "a: 1\n")
	write(t, filepath.Join(clone, "b", "compose.yml"), "b: 1\n")
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-q", "-m", "init")
	git(t, clone, "push", "-q", "origin", "HEAD:main")
	git(t, clone, "branch", "-q", "--set-upstream-to=origin/main")
	return origin, clone
}

// pushFromElsewhere lands a commit on origin changing one file, as another
// host would.
func pushFromElsewhere(t *testing.T, origin, file, content string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", origin, other)
	write(t, filepath.Join(other, file), content)
	git(t, other, "commit", "-q", "-am", "elsewhere")
	git(t, other, "push", "-q")
}

func TestIsClean_OnlyAsksAboutTheFile(t *testing.T) {
	_, dir := repo(t)
	write(t, filepath.Join(dir, "b", "compose.yml"), "b: edited\n")

	clean, err := realGit.IsClean(dir, filepath.Join(dir, "a", "compose.yml"))
	if err != nil || !clean {
		t.Errorf("an edit to b should not make a dirty: %v %v", clean, err)
	}
	clean, err = realGit.IsClean(dir, filepath.Join(dir, "b", "compose.yml"))
	if err != nil || clean {
		t.Errorf("b has an edit and should read dirty: %v %v", clean, err)
	}
}

// A person's staged work is theirs. Committing with `git add` and a bare
// `git commit` would have put it under duva's message.
func TestCommit_LeavesWhatElseIsStagedAlone(t *testing.T) {
	_, dir := repo(t)
	a, b := filepath.Join(dir, "a", "compose.yml"), filepath.Join(dir, "b", "compose.yml")
	write(t, b, "b: staged by a person\n")
	git(t, dir, "add", b)
	write(t, a, "a: 2\n")

	if err := realGit.Add(dir, a); err != nil {
		t.Fatal(err)
	}
	if err := realGit.Commit(dir, a, "a: update"); err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "show", "--name-only", "--format=", "HEAD"); got != "a/compose.yml" {
		t.Errorf("the commit should hold only a, got %q", got)
	}
	if got := git(t, dir, "status", "--porcelain"); got != "M  b/compose.yml" {
		t.Errorf("b should still be staged and uncommitted, got %q", got)
	}
}

func TestPullRebase_CarriesUnrelatedEditsAcross(t *testing.T) {
	origin, dir := repo(t)
	pushFromElsewhere(t, origin, "a/compose.yml", "a: from elsewhere\n")
	write(t, filepath.Join(dir, "b", "compose.yml"), "b: edited\n")

	if err := realGit.PullRebase(dir); err != nil {
		t.Fatalf("an edit origin does not touch should not stop the pull: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a", "compose.yml")); string(got) != "a: from elsewhere\n" {
		t.Errorf("origin's commit was not pulled: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b", "compose.yml")); string(got) != "b: edited\n" {
		t.Errorf("the edit did not survive the pull: %q", got)
	}
}

func TestPullRebase_RefusesWhenOriginChangesAnEditedFile(t *testing.T) {
	origin, dir := repo(t)
	pushFromElsewhere(t, origin, "b/compose.yml", "b: from elsewhere\n")
	write(t, filepath.Join(dir, "b", "compose.yml"), "b: edited\n")
	before := git(t, dir, "rev-parse", "HEAD")

	err := realGit.PullRebase(dir)
	if err == nil || !strings.Contains(err.Error(), "b/compose.yml") {
		t.Fatalf("want a refusal naming b/compose.yml, got %v", err)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != before {
		t.Error("nothing should have been pulled")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b", "compose.yml")); string(got) != "b: edited\n" {
		t.Errorf("the edit should be untouched: %q", got)
	}
}
