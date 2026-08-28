package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// A scenario's state: what a test reads and changes between bringing the
// world up and asserting on it -- pinning a service, what the compose file
// says, which container is behind a service, what duva recorded in its queue.

// dockerPinOnce builds docker pin once per package, for the same reason the
// image is built once: it is the same binary for every test.
var (
	dockerPinOnce sync.Once
	dockerPinPath string
)

// Pin writes the digest for a service with the real docker pin, and commits.
//
// The fixture holds a plain tag, so the starting state is produced the way a
// real stack's is -- and the suite cannot disagree with docker pin about what
// a pinned line looks like. Committing matters too: duva will not act on a
// repository with uncommitted changes.
func (s *Scenario) Pin(services ...string) {
	s.t.Helper()
	for _, service := range services {
		if out, ok := s.Run("pin", service); !ok {
			s.t.Fatalf("pinning %s: %s", service, out)
		}
	}
	s.Commit("pin " + strings.Join(services, ", "))
}

// dockerPin is the path to a built docker pin, built once per package for the
// same reason the images are: it is the same binary for every test.
func (s *Scenario) dockerPin() string {
	s.t.Helper()
	dockerPinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "docker-pin")
		if err != nil {
			s.t.Fatal(err)
		}
		dockerPinPath = filepath.Join(dir, "docker-pin")
		args := []string{"build", "-o", dockerPinPath}
		if os.Getenv("GOCOVERDIR") != "" {
			args = append(args, "-cover", "-coverpkg=./...")
		}
		build := exec.Command("go", append(args, "./cmd/docker-pin")...)
		build.Dir = s.root
		if out, err := build.CombinedOutput(); err != nil {
			s.t.Fatalf("building docker pin: %v\n%s", err, out)
		}
	})
	return dockerPinPath
}

// imageLine matches a service's image, whatever indentation the fixture uses.
var imageLineRe = regexp.MustCompile(`(?m)^\s+image:\s*(\S+)`)

// Image is what the compose file currently pins for a service: the image line
// as written, digest and all.
func (s *Scenario) Image(service string) string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "docker-compose.yml"))
	if err != nil {
		s.t.Fatalf("reading the compose file: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != service+":" {
			continue
		}
		for _, rest := range lines[i+1:] {
			if m := imageLineRe.FindStringSubmatch("\n" + rest); m != nil {
				return m[1]
			}
			// A blank line or a dedent means the service ended without one.
			if trimmed := strings.TrimSpace(rest); trimmed == "" {
				break
			}
		}
	}
	s.t.Fatalf("no image line for service %q in %s", service, s.Dir)
	return ""
}

// Container is the id of the container running a service, or "" if none is.
//
// Asked of compose rather than derived: compose names a project after the
// directory holding the compose file, by rules that are its business, and
// reimplementing them here would be guessing at someone else's algorithm.
func (s *Scenario) Container(service string) string {
	s.t.Helper()
	out, err := s.compose("ps", "-q", service)
	if err != nil || out == "" {
		return ""
	}
	// One id per line; a service has one container in these scenarios.
	return strings.Fields(out)[0]
}

// RunningImage is the image a container was created from.
func (s *Scenario) RunningImage(container string) string {
	s.t.Helper()
	if container == "" {
		return ""
	}
	out, err := exec.Command("docker", "inspect", container,
		"--format", "{{.Config.Image}}").Output()
	if err != nil {
		return ""
	}
	return trimmed(out)
}

// BindSource is the host path a container has mounted at dest.
//
// Read from the daemon rather than by exec'ing into the container: the test
// images are FROM scratch with a static binary and have no shell.
func (s *Scenario) BindSource(container, dest string) string {
	s.t.Helper()
	if container == "" {
		return ""
	}
	format := `{{range .Mounts}}{{if eq .Destination "` + dest + `"}}{{.Source}}{{end}}{{end}}`
	out, err := exec.Command("docker", "inspect", container, "--format", format).Output()
	if err != nil {
		return ""
	}
	return trimmed(out)
}

// Queued is the services duva has recorded as needing approval.
//
// Read from the state file, which is the durable record: it is written by the
// check and survives it, so there is no window to guess at. Reading duva's
// output instead means deciding which run a line belonged to, which is what
// made these tests flaky.
func (s *Scenario) Queued() []string {
	s.t.Helper()
	pending := s.state().Pending
	names := make([]string, 0, len(pending))
	for name := range pending {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Notifications is what duva has announced, one entry per notification.
//
// Read from the file the receiver appends to, which is the durable record:
// counting them answers "did it fire once rather than every run", which is
// the thing that matters. A tool that repeats itself hourly gets muted, and
// then real news is missed too.
func (s *Scenario) Notifications() []string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "notifications", "requests.log"))
	if err != nil {
		return nil // nothing has been announced
	}
	var sent []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			sent = append(sent, line)
		}
	}
	return sent
}

// Notified is the tag duva last announced for a service, from its state.
//
// A constrained service records the tag it announced; a moving tag records
// the digest it is measured against. Both live under the same key, because
// what a service "remembers" is whichever of the two applies to it.
func (s *Scenario) Notified(service string) string {
	s.t.Helper()
	return s.state().Notified[service]
}

// Baseline is the digest duva measures a moving tag against.
func (s *Scenario) Baseline(service string) string {
	s.t.Helper()
	return s.state().Baseline[service]
}

// duvaState is what duva writes to /data/duva.json.
type duvaState struct {
	Baseline map[string]string `json:"baseline"`
	Notified map[string]string `json:"notified"`
	Pending  map[string]struct {
		Candidate string `json:"candidate"`
		Why       string `json:"why"`
		Bump      string `json:"bump"`
		Auto      string `json:"auto"`
	} `json:"pending"`
}

func (s *Scenario) state() duvaState {
	s.t.Helper()
	var st duvaState
	raw, err := os.ReadFile(filepath.Join(s.Dir, "data", "duva.json"))
	if err != nil {
		return st // duva has recorded nothing yet
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		s.t.Fatalf("reading duva's state: %v", err)
	}
	return st
}

// Pending is what duva recorded about a queued service: the candidate it
// found, why it needs a human, and how big the change is.
//
// Empty for a service that is not queued, which is what a test asserts when
// something should have been applied or ignored instead.
func (s *Scenario) Pending(service string) PendingRow {
	s.t.Helper()
	row, ok := s.state().Pending[service]
	if !ok {
		return PendingRow{}
	}
	return PendingRow{
		Candidate: row.Candidate,
		Why:       row.Why,
		Bump:      row.Bump,
		Auto:      row.Auto,
	}
}

// PendingRow is one entry in duva's approval queue.
type PendingRow struct {
	Candidate string
	Why       string
	Bump      string
	Auto      string
}

// Queue fetches duva's approval queue over HTTP.
//
// The page is what an operator actually sees, and it is served from the same
// state file the other assertions read -- so a difference between them is the
// UI lying about what duva recorded.
func (s *Scenario) Queue() string {
	s.t.Helper()
	var body string
	s.waitFor("duva's queue to answer", 30*time.Second,
		func() bool {
			resp, err := http.Get("http://localhost:" + queuePort + "/")
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				return false
			}
			body = string(raw)
			return resp.StatusCode == http.StatusOK
		},
		func() string { return s.Logs("duva") })
	return body
}

// queuePort is where a fixture publishes duva's queue when a test reads it.
const queuePort = "8098"

// Run invokes docker pin in the scenario's project and returns what it said,
// with whether it succeeded.
//
// The exit status matters as much as the output: a mistyped safety flag that
// exits zero has run for real, which is the bug rejectUnknownFlags exists to
// prevent.
func (s *Scenario) Run(args ...string) (string, bool) {
	s.t.Helper()
	cmd := exec.Command(s.dockerPin(), args...)
	cmd.Dir = s.Dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

// Digest is the digest the registry currently serves for a tag.
func (s *Scenario) Digest(repo, tag string) string {
	s.t.Helper()
	ref := registryHost + "/" + repo + ":" + tag
	out, err := exec.Command("docker", "image", "inspect", ref,
		"--format", "{{range .RepoDigests}}{{println .}}{{end}}").Output()
	if err != nil {
		s.t.Fatalf("no local image for %s: %v", ref, err)
	}
	// An image derived from another can carry several repo digests; only the
	// one for this repository names what was pushed here.
	for _, line := range strings.Fields(string(out)) {
		if strings.HasPrefix(line, registryHost+"/"+repo+"@") {
			return line[strings.Index(line, "@")+1:]
		}
	}
	s.t.Fatalf("no digest for %s in %s", ref, out)
	return ""
}

// WriteCompose replaces the scenario's compose file.
//
// For the scenarios that are about docker pin rather than duva: what matters
// there is the image line before and after, and a fixture per permutation
// would be four files differing by one word.
func (s *Scenario) WriteCompose(content string) {
	s.t.Helper()
	s.writeFile(filepath.Join(s.Dir, "docker-compose.yml"), content)
}

// PushAdditional publishes the tags a scenario's "additional-images" file
// names: the newer versions duva is meant to find.
//
// Separate from the images the compose file references, which Up pushes so
// the stack can start -- these are what makes an update available, so a test
// decides WHEN they appear. The refused-image scenario needs its broken tag
// to arrive after pinning, not before.
//
// The tags live with the fixture rather than in each test so that the sandbox
// stands up the same world a test does. Two lists of the same tags would
// drift the moment one changed.
func (s *Scenario) PushAdditional() {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "additional-images"))
	if err != nil {
		return // a scenario need not offer anything newer
	}
	for _, tag := range strings.Fields(string(raw)) {
		s.Push("app", tag)
	}
}

// Watched is every service carrying a duva.* label, which is what a fixture
// means for duva to act on.
//
// A test names the services it pins, because which ones it pins is part of
// what it is testing. The sandbox has no such opinion: it wants the scenario
// as it is meant to be seen.
func (s *Scenario) Watched() []string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "docker-compose.yml"))
	if err != nil {
		s.t.Fatalf("reading the compose file: %v", err)
	}

	var watched []string
	var service string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		// A service name is indented two spaces and ends in a colon.
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(trimmed, ":") && !strings.Contains(trimmed, " ") {
			service = strings.TrimSuffix(trimmed, ":")
			continue
		}
		if strings.HasPrefix(trimmed, "duva.") && service != "" &&
			!slices.Contains(watched, service) {
			watched = append(watched, service)
		}
	}
	return watched
}
