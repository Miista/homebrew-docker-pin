//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The parts of a scenario's world that are not the compose file: the duva
// image under test, the registry's certificate, the git repository duva
// commits into, and the images the fixture refers to.

// label marks everything a scenario creates, so a run killed outright can
// still be swept by the next one.
const label = "io.github.miista.docker-pin.integration"

func (s *Scenario) suite() string {
	suite, _, _ := strings.Cut(s.Name, "/")
	return suite
}

// copyCerts puts the package's certificate where this scenario's fixture
// expects it.
func (s *Scenario) copyCerts() {
	s.t.Helper()
	dir := filepath.Join(s.Dir, "certs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	if out, err := exec.Command("cp", suiteCerts+"/cert.pem", suiteCerts+"/key.pem", dir).CombinedOutput(); err != nil {
		s.t.Fatalf("copying the certificate: %v: %s", err, out)
	}
}

func trimmed(b []byte) string { return strings.TrimSpace(string(b)) }

// initRepo makes the project a git repository and commits it.
//
// duva commits what it applies and refuses to act on a dirty tree, so "not a
// repository at all" would look like a git failure rather than a clean start.
func (s *Scenario) initRepo() {
	s.t.Helper()
	s.git("init", "-q")
	s.Commit("the scenario as it starts")
}

// git runs a git command in the project.
func (s *Scenario) git(args ...string) string {
	s.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test",
		"-c", "user.email=test@localhost"}, args...)...)
	cmd.Dir = s.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Commit stages everything and commits it, then waits for git to be quiet.
//
// The wait matters: duva refuses to act on a repository with a locked index,
// and it may be checking while a test commits. Returning only once git has
// finished means a test never leaves the world half-built.
func (s *Scenario) Commit(message string) {
	s.t.Helper()
	s.git("add", "-A")
	// An empty commit is not an error here: a scenario may have nothing to
	// record, and failing would be about git rather than about duva.
	cmd := exec.Command("git", "-c", "user.name=test", "-c", "user.email=test@localhost",
		"commit", "-q", "--allow-empty", "-m", message)
	cmd.Dir = s.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		s.t.Fatalf("committing: %v\n%s", err, out)
	}
	s.waitForQuietRepo()
}

// waitForQuietRepo blocks until no git process holds the index.
func (s *Scenario) waitForQuietRepo() {
	s.t.Helper()
	lock := filepath.Join(s.Dir, ".git", "index.lock")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(lock); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("the repository at %s still holds its index lock", s.Dir)
}

// Log is the subject of the most recent commit.
func (s *Scenario) Log() string {
	return s.git("log", "--oneline", "-1")
}

// Status is what git considers uncommitted. Empty means clean.
func (s *Scenario) Status() string {
	return s.git("status", "--porcelain")
}

// pushDeclaredImages publishes the tags the fixture's own compose file refers
// to, so the stack can start.
//
// Only those: what is AVAILABLE to update to is the variable a test is about,
// so a test pushes that itself.
func (s *Scenario) pushDeclaredImages() {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "images"))
	if err != nil {
		return // a scenario need not declare any
	}
	for _, tag := range strings.Fields(string(raw)) {
		s.Push("app", tag)
	}
}

// Push publishes a runnable image under a tag.
//
// The content is derived from the tag, so two tags never produce the same
// digest by accident: duva compares digests, and identical content would make
// "the container was replaced" indistinguishable from "nothing happened".
//
// Pass content to override that, which is how a moving tag is made to move:
// the tag stays put while what it points at changes, exactly as it does when
// upstream rebuilds.
//
//	s.Push("app", "1.0.1")             // a new tag
//	s.Push("app", "latest", "second")  // the same tag, new digest
func (s *Scenario) Push(repo, tag string, content ...string) {
	s.t.Helper()
	marker := tag
	if len(content) > 0 {
		marker = content[0]
	}
	dir := s.scratch("build")

	main := fmt.Sprintf(`package main

import (
	"fmt"
	"time"
)

func main() {
	for {
		fmt.Println("alive: %s")
		time.Sleep(5 * time.Second)
	}
}
`, marker)
	s.writeFile(filepath.Join(dir, "main.go"), main)
	s.writeFile(filepath.Join(dir, "go.mod"), "module sleeper\n\ngo 1.22\n")
	// FROM scratch with a static binary: nothing is pulled, so the suites
	// work offline, and the container genuinely runs so it can be replaced.
	s.writeFile(filepath.Join(dir, "Dockerfile"),
		"FROM scratch\nCOPY sleeper /sleeper\nCMD [\"/sleeper\"]\n")

	build := exec.Command("go", "build", "-ldflags", "-s -w", "-o", "sleeper", ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		s.t.Fatalf("building the test image binary: %v\n%s", err, out)
	}

	ref := fmt.Sprintf("%s/%s:%s", registryHost, repo, tag)

	// Remove any image already under this tag before building.
	//
	// Building leaves the image on the HOST, so a tag from an earlier test --
	// or from a run that was killed before it could clean up -- would still
	// be here. A moving tag would then not move, and content from one test
	// would silently become another's. Removing it first means the build
	// cannot see a stale one, whoever left it.
	_ = exec.Command("docker", "rmi", "-f", ref).Run()

	s.docker("build", "-q", "-t", ref, dir)
	s.docker("push", "-q", ref)
	s.pushed = append(s.pushed, ref)
}

// PushUnrunnable publishes an image that exists but cannot start: FROM
// scratch with no command. For testing what duva does when a container
// refuses the image it was just pinned to.
func (s *Scenario) PushUnrunnable(repo, tag string) {
	s.t.Helper()
	dir := s.scratch("broken")
	s.writeFile(filepath.Join(dir, "marker"), repo+":"+tag)
	s.writeFile(filepath.Join(dir, "Dockerfile"), "FROM scratch\nCOPY marker /marker\n")

	ref := fmt.Sprintf("%s/%s:%s", registryHost, repo, tag)

	// Remove any image already under this tag before building.
	//
	// Building leaves the image on the HOST, so a tag from an earlier test --
	// or from a run that was killed before it could clean up -- would still
	// be here. A moving tag would then not move, and content from one test
	// would silently become another's. Removing it first means the build
	// cannot see a stale one, whoever left it.
	_ = exec.Command("docker", "rmi", "-f", ref).Run()

	s.docker("build", "-q", "-t", ref, dir)
	s.docker("push", "-q", ref)
	s.pushed = append(s.pushed, ref)
}

// scratch is a directory for the harness's own working files -- image build
// contexts, mostly.
//
// Outside the project, because the project is a git repository that duva
// refuses to act on when it is dirty: a build context left inside would look
// like a half-applied change.
func (s *Scenario) scratch(name string) string {
	dir := filepath.Join(s.root, "testbed-scratch", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { os.RemoveAll(filepath.Join(s.root, "testbed-scratch")) })
	return dir
}

func (s *Scenario) writeFile(path, content string) {
	s.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}
