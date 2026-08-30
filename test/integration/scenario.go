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
	"time"
)

// repoRoot is the checkout this test runs from. Fixtures are read from it and
// the testbed is written into it -- deliberately, not $TMPDIR: on macOS that
// lives under a symlink, so docker records a resolved path that never matches
// the one a test holds, and any lookup by path silently finds nothing.
func repoRoot(t T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("finding the repository root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// T is what a Scenario needs from whoever is driving it.
//
// *testing.T satisfies this, and so does the small implementation the sandbox
// uses -- which is the point: standing a scenario up by hand and testing one
// are the same work, and a second copy of it would drift from the first.
type T interface {
	Helper()
	Logf(format string, args ...any)
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Scenario is one test's world: a compose project in the testbed, with
// whatever it declares running.
type Scenario struct {
	t    T
	Name string
	// Dir is the project, a git repository, under testbed/.
	Dir  string
	root string
	// pushed is every image reference this scenario built, so teardown can
	// remove exactly those.
	pushed []string
}

// Up copies a fixture into the testbed and brings up everything it declares.
//
// The name is <suite>/<scenario>, matching the fixture's path. Teardown is
// registered with the test, so a scenario is removed however the test ends --
// including a panic or a failed assertion.
func Up(t T, name string) *Scenario {
	t.Helper()

	root := repoRoot(t)
	suite, scenarioName, ok := strings.Cut(name, "/")
	if !ok {
		t.Fatalf("scenario name should be <suite>/<scenario>, got %q", name)
	}

	s := &Scenario{
		t:    t,
		Name: name,
		root: root,
		Dir:  filepath.Join(root, "testbed"),
	}

	// One directory, always the same one. Compose derives a project name from
	// it, so a fixed name is a project that can always be found again --
	// which is what teardown needs, and a random one was how containers used
	// to survive it. Tests run sequentially, so there is nothing a
	// per-scenario name would protect against.
	//
	// Swept before the fixture is copied in as well as after the test: a run
	// killed part way through leaves the testbed populated, and the next one
	// must not inherit it. The order matters -- sweep reads the compose file
	// to bring the project down, so deleting the directory first would strand
	// the containers.
	s.sweep()
	t.Cleanup(s.sweep)

	src := filepath.Join(root, "hack", "fixtures", suite, scenarioName)
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("no fixture at %s", src)
	}
	s.copyTree(src, s.Dir)

	// duva's state directory. Docker would create it as a side effect of the
	// bind mount, but only when its parent already exists -- so whether it
	// worked depended on what a previous test happened to leave behind.
	if err := os.MkdirAll(filepath.Join(s.Dir, "data"), 0o777); err != nil {
		t.Fatalf("preparing the state directory: %v", err)
	}

	s.copyCerts()
	s.initRepo()

	// The registry is shared and already up, but its contents are this
	// scenario's alone: what another test pushed must not look like a newer
	// tag to this one.
	if err := emptyRegistry(); err != nil {
		t.Fatal(err)
	}
	s.pushDeclaredImages()

	s.composeUp()

	// The duvas are stopped again straight away. They come up with everything
	// else -- a fixture declares them like any other service, and compose has
	// no way to say "all but these" -- but a test is not finished building its
	// world yet, and duva checking a half-built one is noise at best.
	// Start() puts them back when the test is ready.
	s.stop(s.duvaServices()...)

	return s
}

// Bare prepares an empty project in the testbed, with no fixture and nothing
// running.
//
// For the scenarios that are about docker pin rather than duva: what those
// need is a compose file and a registry, and what they assert is the image
// line before and after. A fixture per permutation would be a directory of
// files differing by one word.
func Bare(t T) *Scenario {
	t.Helper()

	root := repoRoot(t)
	s := &Scenario{t: t, Name: "bare", root: root, Dir: filepath.Join(root, "testbed")}

	s.sweep()
	t.Cleanup(s.sweep)
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatalf("preparing the testbed: %v", err)
	}
	if err := emptyRegistry(); err != nil {
		t.Fatal(err)
	}
	return s
}

// Start runs duva, which checks once as it comes up.
//
// A test calls this when its world is complete: images pushed, services
// pinned, everything committed. duva's first check is then the one the test
// is about, rather than one that happened to run while the world was still
// being built.
func (s *Scenario) Start(services ...string) {
	s.t.Helper()
	if len(services) == 0 {
		services = s.duvaServices()
	}
	s.composeUp(services...)
	for _, service := range services {
		s.waitFor("a check by "+service, 60*time.Second,
			func() bool { return s.checksDone(service) > 0 },
			func() string { return s.Logs(service) })
	}
}

// duvaServices is every service in the fixture that runs duva.
//
// Asked of compose rather than assumed: a scenario decides what it is made
// of, and one that runs two duvas in different timezones names them
// accordingly.
func (s *Scenario) duvaServices() []string {
	s.t.Helper()
	out, err := s.compose("config", "--services")
	if err != nil {
		s.t.Fatalf("reading the fixture's services: %v\n%s", err, out)
	}
	var duvas []string
	for _, name := range strings.Fields(out) {
		if strings.HasPrefix(name, "duva") {
			duvas = append(duvas, name)
		}
	}
	if len(duvas) == 0 {
		s.t.Fatalf("the fixture for %s declares no duva service", s.Name)
	}
	return duvas
}

// hasService reports whether the fixture declares a service by this name.
func (s *Scenario) hasService(name string) bool {
	s.t.Helper()
	out, err := s.compose("config", "--services")
	if err != nil {
		s.t.Fatalf("reading the fixture's services: %v\n%s", err, out)
	}
	for _, declared := range strings.Fields(out) {
		if declared == name {
			return true
		}
	}
	return false
}

// stop halts services without removing them, so they keep their logs and can
// be started again.
func (s *Scenario) stop(services ...string) {
	s.t.Helper()
	if out, err := s.compose(append([]string{"stop", "-t", "2"}, services...)...); err != nil {
		s.t.Fatalf("stopping %s: %v\n%s", strings.Join(services, ", "), err, out)
	}
}

// sweep empties the testbed: the compose project it holds, the images a
// scenario pushed, then the directory itself. It runs before a scenario as
// well as after, so a run killed outright cannot influence the next one.
func (s *Scenario) sweep() {
	if _, err := os.Stat(filepath.Join(s.Dir, "docker-compose.yml")); err == nil {
		cmd := exec.Command("docker", "compose", "down",
			"--remove-orphans", "--volumes", "--timeout", "3")
		cmd.Dir = s.Dir
		_ = cmd.Run()
	}

	// The registry goes down with the project, taking what was pushed to it.
	// The images themselves are on the HOST though: building one leaves it
	// tagged here, and a later scenario building the same tag would find that
	// tag already present -- so a moving tag would not move, and content from
	// one test would leak into the next.
	//
	// Removed by reference rather than by label: the scenario built them, so
	// it knows exactly which they are, and a filter cannot match something it
	// should not have.
	if len(s.pushed) > 0 {
		_ = exec.Command("docker", append([]string{"rmi", "-f"}, s.pushed...)...).Run()
		s.pushed = nil
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
		// A service that would not start explains itself in its own log,
		// which compose's error does not include.
		logs, _ := s.compose("logs", "--no-log-prefix")
		s.t.Fatalf("bringing up %s: %v\n%s\nservice logs:\n%s",
			s.Name, err, indent(out), indent(logs))
	}
}

// Logs is everything a service has printed.
func (s *Scenario) Logs(service string) string {
	out, _ := s.compose("logs", "--no-log-prefix", service)
	return out
}
