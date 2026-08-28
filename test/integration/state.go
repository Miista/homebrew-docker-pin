//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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

	for _, service := range services {
		cmd := exec.Command(dockerPinPath, "pin", service)
		cmd.Dir = s.Dir
		if out, err := cmd.CombinedOutput(); err != nil {
			s.t.Fatalf("pinning %s: %v\n%s", service, err, out)
		}
	}
	s.Commit("pin " + strings.Join(services, ", "))
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
