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
	"fmt"
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
	// volumesAtStart is the set of anonymous volumes that already existed when
	// this scenario began, so teardown can remove the ones it caused without
	// touching anything else on this host.
	volumesAtStart map[string]bool
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

	// duva's state directories. Docker would create them as a side effect of
	// the bind mount, but only when the parent already exists -- so whether
	// it worked depended on what a previous test happened to leave behind --
	// and it creates them owned by root, which duva then cannot write.
	//
	// Every /data bind the fixture declares, not just "data": a scenario
	// running several duvas gives each its own, and one of them silently
	// unwritable looks like a duva that found nothing.
	for _, dir := range s.stateDirs() {
		if err := os.MkdirAll(filepath.Join(s.Dir, dir), 0o777); err != nil {
			t.Fatalf("preparing the state directory %s: %v", dir, err)
		}
	}

	s.copyCerts()
	s.writeEnv()
	s.initRepo()

	// The registry is shared and already up, but its contents are this
	// scenario's alone: what another test pushed must not look like a newer
	// tag to this one.
	if err := emptyRegistry(); err != nil {
		t.Fatal(err)
	}
	s.pushDeclaredImages()

	s.volumesAtStart = danglingVolumes()

	// Everything EXCEPT the duva services, which Start() brings up once the
	// test has finished building its world.
	//
	// Not started-then-stopped, which is what this did: the watcher checks
	// once at startup, and a watcher started here checks a world whose newer
	// tags have not been pushed yet. That check is not merely noise -- it
	// records a cutoff in /data, so the real check later reports "nothing
	// published since the last check" and the pipeline correctly does
	// nothing. The symptom is an update that never applies, in a test that
	// passes alone (where the timing differs) and fails in the suite.
	if others := s.nonDuvaServices(); len(others) > 0 {
		s.composeUp(others...)
	}

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

// writeEnv writes the two values a fixture cannot know: who this host runs
// as, and what its docker group is.
//
// Compose reads .env from the project directory on its own, so a fixture names
// ${UID} and ${DOCKER_GID} and needs nothing else. They cannot be baked in:
// the update stage must run as the user that owns the repository -- root would
// leave root-owned files in it and lose the commit identity -- and the docker
// group's id differs per host.
func (s *Scenario) writeEnv() {
	s.t.Helper()
	// Asked of the daemon rather than of this host, because those differ and
	// only one of them is the answer. Under Docker Desktop the socket on the
	// host is a shim -- stat reports nothing useful -- while the daemon's own
	// socket, which is the one a container mounts, is group 991. Reading the
	// host's would silently produce 0, and the update stage would fail to
	// open the socket after everything else had gone right.
	out, err := exec.Command("docker", "run", "--rm",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"busybox", "stat", "-c", "%g", "/var/run/docker.sock").Output()
	if err != nil {
		s.t.Fatalf("asking the daemon for the docker socket's group: %v", err)
	}
	gid := strings.TrimSpace(string(out))
	// PROJECT_DIR is where the testbed is on this host, for the fixture that
	// mounts it at its own path rather than at /compose. A fixture cannot
	// write it: it is wherever the repository happens to be checked out.
	body := fmt.Sprintf("UID=%d\nGID=%d\nDOCKER_GID=%s\nPROJECT_DIR=%s\n",
		os.Getuid(), os.Getgid(), gid, s.Dir)
	if err := os.WriteFile(filepath.Join(s.Dir, ".env"), []byte(body), 0o644); err != nil {
		s.t.Fatalf("writing the scenario's .env: %v", err)
	}
}

// duvaServices is every service in the fixture that runs duva.
//
// Asked of compose rather than assumed: a scenario decides what it is made
// of, and one that runs two duvas in different timezones names them
// accordingly.
// nonDuvaServices is every service Start() does not bring up.
//
// The complement of duvaServices, so the two cannot drift: what Up starts and
// what Start starts must together be everything the fixture declares, and a
// service missing from both would simply never run.
//
// This is everything but the watcher -- the queue and the updater are in it,
// because they answer requests rather than acting on a schedule, so starting
// them early builds the world rather than observing it.
func (s *Scenario) nonDuvaServices() []string {
	s.t.Helper()
	out, err := s.compose("config", "--services")
	if err != nil {
		s.t.Fatalf("reading the fixture's services: %v\n%s", err, out)
	}
	deferred := make(map[string]bool)
	for _, name := range s.duvaServices() {
		deferred[name] = true
	}
	var rest []string
	for _, name := range strings.Fields(out) {
		if !deferred[name] {
			rest = append(rest, name)
		}
	}
	return rest
}

func (s *Scenario) duvaServices() []string {
	s.t.Helper()
	out, err := s.compose("config", "--services")
	if err != nil {
		s.t.Fatalf("reading the fixture's services: %v\n%s", err, out)
	}
	var duvas []string
	for _, name := range strings.Fields(out) {
		if !strings.HasPrefix(name, "duva") {
			continue
		}
		// Only the stage that checks is waited for. A queue and an updater
		// answer requests -- they have no schedule and no registry, so there
		// is no check to wait for, and waiting on one would time out on a
		// service that is working correctly. Same for a v3 hub.
		if strings.Contains(name, "hub") ||
			strings.Contains(name, "queue") || strings.Contains(name, "update") {
			continue
		}
		duvas = append(duvas, name)
	}
	if len(duvas) == 0 {
		s.t.Fatalf("the fixture for %s declares nothing that checks", s.Name)
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
	// By project name, and without needing the directory: a sandbox torn down
	// twice, or a run killed after the directory went but before the
	// containers did, must still remove them. `compose down -p` needs no
	// compose file, so this is not guarded on one existing.
	down := exec.Command("docker", "compose", "-p", projectName, "down",
		"--remove-orphans", "--volumes", "--timeout", "3")
	if _, err := os.Stat(s.Dir); err == nil {
		down.Dir = s.Dir
	}
	_ = down.Run()

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

	// Anonymous volumes outlive `down --volumes`: that removes what compose
	// considers the project's, and duva does not go through compose -- it
	// recreates containers over the API, and each recreation leaves the old
	// container's anonymous volumes behind with no project label on them.
	// Left alone they accumulate, hundreds per week of running this suite.
	//
	// Removed by diffing against what existed before the scenario started,
	// rather than by pruning: an anonymous volume carries nothing that says
	// which project made it, so a prune here would also take volumes belonging
	// to whatever else this host happens to be running.
	if s.volumesAtStart != nil {
		var leaked []string
		for v := range danglingVolumes() {
			if !s.volumesAtStart[v] {
				leaked = append(leaked, v)
			}
		}
		if len(leaked) > 0 {
			_ = exec.Command("docker", append([]string{"volume", "rm", "-f"}, leaked...)...).Run()
		}
		s.volumesAtStart = nil
	}

	// duva commits from inside a container as root, so the repository it
	// wrote into -- and the state files beside it -- can be owned by root
	// on this host. RemoveAll then leaves them, and the next scenario's
	// git init fails on a .git it cannot write. Falling back to a
	// throwaway container, which is root, removes what this user cannot.
	if err := os.RemoveAll(s.Dir); err != nil {
		_ = exec.Command("docker", "run", "--rm",
			"-v", filepath.Dir(s.Dir)+":/w", "alpine:3.20",
			"rm", "-rf", "/w/"+filepath.Base(s.Dir)).Run()
	}
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
// projectName is what the testbed's compose project is called.
//
// Explicit rather than derived from the directory, which is what compose does
// when nothing says otherwise. A project named after a directory can only be
// addressed from that directory -- so once the directory is gone, `compose
// down` has nothing to resolve and the containers it should have removed are
// unreachable by that route. The fixtures set container_name, which is global
// to the daemon rather than scoped to a project, so they are not addressable
// by the project either. Naming the project makes teardown independent of the
// filesystem.
const projectName = "duva-testbed"

func (s *Scenario) compose(args ...string) (string, error) {
	cmd := exec.Command("docker", append([]string{"compose", "-p", projectName}, args...)...)
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
	out, err := s.compose(args...)

	// The daemon runs in a VM with the repository shared in over virtiofs, and
	// a directory this process has just created is not always visible there
	// yet: the daemon resolves the bind source, does not find it, and reports
	// it as missing when it demonstrably exists -- confirmed by stat'ing it
	// here at the moment the daemon said it was gone.
	//
	// A plain mkdir propagates promptly; it takes deleting a populated testbed
	// whose mounts the daemon still holds, then rebuilding it under the same
	// path, to open a window wide enough to hit. Which is exactly what runs
	// between two tests, and why this surfaced only under -shuffle.
	//
	// Retried rather than waited out, because there is no event to wait for --
	// and a second attempt is enough in practice. A genuinely missing path
	// fails both times and still reports.
	for attempt := 0; err != nil && attempt < 5 &&
		strings.Contains(out, "error while creating mount source path"); attempt++ {
		time.Sleep(500 * time.Millisecond)
		out, err = s.compose(args...)
	}

	if err != nil {
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

// danglingVolumes is the set of volumes no container references right now.
//
// Best effort: if docker cannot be asked, teardown carries on rather than
// failing a test over cleanup it could not perform.
func danglingVolumes() map[string]bool {
	out, err := exec.Command("docker", "volume", "ls", "-q", "--filter", "dangling=true").Output()
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, name := range strings.Fields(string(out)) {
		set[name] = true
	}
	return set
}

// stateDirs is every host path the fixture binds to a duva's /data, relative
// to the project.
//
// Read from the compose file rather than assumed, so a scenario with two
// agents gets both of their directories created and owned by this user. Falls
// back to "data" when the fixture declares none, which is every single-duva
// scenario.
func (s *Scenario) stateDirs() []string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "docker-compose.yml"))
	if err != nil {
		return []string{"data"}
	}
	var dirs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		src, dest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(dest) != "/data" {
			continue
		}
		src = strings.TrimPrefix(strings.TrimSpace(src), "./")
		if src != "" && !seen[src] {
			seen[src] = true
			dirs = append(dirs, src)
		}
	}
	if len(dirs) == 0 {
		return []string{"data"}
	}
	return dirs
}

// StartExpectingFailure brings a duva up and waits for it to exit.
//
// The mirror of Start, for a scenario about duva refusing to run: Start
// waits for a check, which a duva that correctly stopped will never do, so
// asserting on a refusal through it means waiting out the timeout.
func (s *Scenario) StartExpectingFailure(service string) {
	s.t.Helper()
	// up without --wait: the container is expected to exit, and --wait
	// would call that a failure to come up.
	if out, err := s.compose("up", "-d", "--no-deps", service); err != nil {
		s.t.Fatalf("starting %s: %v\n%s", service, err, out)
	}
	s.waitFor(service+" to stop", 60*time.Second,
		func() bool { return !s.running(service) },
		func() string { return s.Logs(service) })
}

// running reports whether a service's container is up.
func (s *Scenario) running(service string) bool {
	s.t.Helper()
	out, err := s.compose("ps", "-q", "--status", "running", service)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}
