//go:build integration

// Package integration runs duva against a real docker daemon, a real
// registry and real compose files.
//
// The world a test needs is a compose file under hack/fixtures/<suite>/<name>,
// readable on its own terms. A test copies one into the testbed, changes what
// it needs, makes duva look, and asserts on what happened.
//
// These were shell scripts. They moved here for two reasons. Waiting: a test
// has to know when the world has settled -- when git is quiet, when a check
// has finished -- and expressing that in bash produced wall-clock guesses that
// were wrong often enough to be flaky. And reporting: a failing shell
// assertion said which check failed but never what it saw, so every
// investigation started by adding echo statements.
package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the checkout this test runs from. Fixtures are read from it and
// the testbed is written into it -- deliberately, not $TMPDIR: on macOS that
// lives under a symlink, so docker records a resolved path that never matches
// the one a test holds, and any lookup by path silently finds nothing.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("finding the repository root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// Scenario is one test's world: a compose project in the testbed, with
// whatever it declares running.
type Scenario struct {
	t    *testing.T
	Name string
	// Dir is the project, a git repository, under testbed/.
	Dir string
	// Image is the duva image this scenario's fixture refers to.
	Image string
	root  string
}

// Up copies a fixture into the testbed and brings up everything it declares.
//
// The name is <suite>/<scenario>, matching the fixture's path. Teardown is
// registered with the test, so a scenario is removed however the test ends --
// including a panic or a failed assertion.
func Up(t *testing.T, name string) *Scenario {
	t.Helper()

	root := repoRoot(t)
	suite, scenarioName, ok := strings.Cut(name, "/")
	if !ok {
		t.Fatalf("scenario name should be <suite>/<scenario>, got %q", name)
	}

	s := &Scenario{
		t:     t,
		Name:  name,
		root:  root,
		Image: suiteImage,
		Dir:   filepath.Join(root, "testbed", suite+"-"+scenarioName),
	}

	// A fixed directory, never a random one: compose derives a project name
	// from it, so a random name is a project nothing can find afterwards --
	// which is how containers and networks used to survive teardown.
	if err := os.RemoveAll(s.Dir); err != nil {
		t.Fatalf("clearing the testbed: %v", err)
	}
	s.sweep()
	t.Cleanup(s.sweep)

	src := filepath.Join(root, "hack", "fixtures", suite, scenarioName)
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("no fixture at %s", src)
	}
	s.copyTree(src, s.Dir)

	s.copyCerts()
	s.initRepo()

	// The registry alone first: nothing else can start until it holds the
	// image the fixture names, since a service whose image does not exist
	// cannot be created.
	s.composeUp("testregistry")
	s.pushDeclaredImages()
	s.composeUp()

	return s
}

// sweep removes everything this scenario created: the compose project, and
// the directory it lived in.
func (s *Scenario) sweep() {
	if _, err := os.Stat(filepath.Join(s.Dir, "docker-compose.yml")); err == nil {
		cmd := exec.Command("docker", "compose", "down",
			"--remove-orphans", "--volumes", "--timeout", "3")
		cmd.Dir = s.Dir
		_ = cmd.Run()
	}
	_ = os.RemoveAll(s.Dir)
}

func (s *Scenario) copyTree(src, dst string) {
	s.t.Helper()
	if out, err := exec.Command("cp", "-R", src+"/.", dst).CombinedOutput(); err != nil {
		s.t.Fatalf("copying the fixture: %v: %s", err, out)
	}
	// Fixtures include common/*.yml for what every scenario shares. It is
	// copied inside the project so the include resolves from below the
	// compose file rather than above it, where nothing is mounted.
	common := filepath.Join(s.root, "hack", "fixtures", "common")
	if _, err := os.Stat(common); err == nil {
		dest := filepath.Join(dst, "common")
		_ = os.MkdirAll(dest, 0o755)
		if out, err := exec.Command("cp", "-R", common+"/.", dest).CombinedOutput(); err != nil {
			s.t.Fatalf("copying the shared fixture: %v: %s", err, out)
		}
	}
}

// docker runs a docker command and returns its combined output, failing the
// test on error with everything the command said.
func (s *Scenario) docker(args ...string) string {
	s.t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		s.t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// compose runs a compose command in this scenario's project.
func (s *Scenario) compose(args ...string) (string, error) {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = s.Dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func (s *Scenario) composeUp(services ...string) {
	s.t.Helper()
	// --wait blocks until services with a healthcheck report healthy, so a
	// test never has to poll for the registry to accept pushes.
	args := append([]string{"up", "-d", "--wait", "--remove-orphans"}, services...)
	if out, err := s.compose(args...); err != nil {
		s.t.Fatalf("bringing up %s: %v\n%s", s.Name, err, out)
	}
}

// Logs is everything a service has printed.
func (s *Scenario) Logs(service string) string {
	out, _ := s.compose("logs", "--no-log-prefix", service)
	return out
}
