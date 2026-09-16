package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
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
	pending := s.state()
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

// queueEntry is one entry as the queue writes it to /data/queue.json.
//
// A subset of the real one: what a test asserts on, not everything the queue
// records. The field names are the wire names, so a rename in the queue shows
// up here as a test reading an empty string rather than as a compile error --
// which is the reason Queued() asserts on more than presence.
type queueEntry struct {
	Service   string `json:"service"`
	From      string `json:"from"`
	To        string `json:"to"`
	Tag       string `json:"tag"`
	Digest    string `json:"digest"`
	Kind      string `json:"kind"`
	Why       string `json:"why"`
	Auto      string `json:"auto"`
	FirstSeen string `json:"first_seen"`
}

// state is the queue, keyed by service.
//
// Read from the file the queue keeps rather than from its log: it is written
// on every change and survives the process, so there is no window to guess
// at. Reading output instead means deciding which run a line belonged to,
// which is what made the v3 suite flaky.
func (s *Scenario) state() map[string]queueEntry {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Dir, "queue-data", "queue.json"))
	if err != nil {
		return map[string]queueEntry{} // nothing queued yet
	}
	var st map[string]queueEntry
	if err := json.Unmarshal(raw, &st); err != nil {
		s.t.Fatalf("reading the queue: %v", err)
	}
	if st == nil {
		st = map[string]queueEntry{}
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
	row, ok := s.state()[service]
	if !ok {
		return PendingRow{}
	}
	// A digest move has no version pair, so what it is a candidate for is the
	// digest it moved to.
	candidate := row.To
	if row.Kind == "" && row.Digest != "" {
		candidate = row.Digest
	}
	return PendingRow{
		Candidate: candidate,
		Why:       row.Why,
		Bump:      row.Kind,
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

// PushUpdates publishes the tags a scenario's "updates" file names: the newer
// versions duva is meant to find.
//
// The one thing a fixture cannot say in its compose file, since no service
// references them -- everything the file DOES name is pushed by Up, read
// from the file itself.
//
// A test calls this rather than Up doing it, because when an update appears
// is often the point: the refused-image scenario needs its broken tag to
// arrive after pinning, not before.
func (s *Scenario) PushUpdates() {
	s.t.Helper()
	m := s.manifest()
	for repo, tags := range m.Updates {
		for _, tag := range tags {
			s.Push(repo, tag)
		}
	}
	for repo, tags := range m.Unrunnable {
		for _, tag := range tags {
			s.PushUnrunnable(repo, tag)
		}
	}
}

// manifest is what a scenario declares beyond its compose file.
//
// A file rather than more constants in the harness, so that standing a
// scenario up by hand produces the same world a test does -- and yaml rather
// than a bare list so it can say which of its parts is which, and explain
// itself in comments.
type manifest struct {
	// Updates are the newer tags duva should find, by the repository they
	// belong to. Nothing references them, which is why they cannot come from
	// the compose file the way the images a service runs do.
	//
	// Keyed by repo so a scenario can run more than one image and offer each
	// its own releases -- a stack with a database and a web service does not
	// version them together.
	Updates map[string][]string `yaml:"updates"`
	// Unrunnable are newer tags that pull but cannot start, by repository.
	//
	// Separate from Updates because they are built differently -- FROM scratch
	// with no entrypoint -- and because a scenario declaring one is saying
	// something about what it tests: that an apply gets as far as replacing a
	// container and then fails. Declared rather than pushed by the test, so
	// the sandbox can stand the scenario up exactly as a test sees it.
	Unrunnable map[string][]string `yaml:"unrunnable"`
}

func (s *Scenario) manifest() manifest {
	s.t.Helper()
	var m manifest
	raw, err := os.ReadFile(filepath.Join(s.Dir, "scenario.yaml"))
	if err != nil {
		return m // a scenario need not declare anything
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		s.t.Fatalf("reading %s's scenario.yaml: %v", s.Name, err)
	}
	return m
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

// UpdateNow presses the Update button for a service, as an operator would.
//
// A form POST rather than a call into duva: what is being tested is that the
// page's button reaches the container, and a test that skipped the HTTP round
// trip would not know whether it did.
func (s *Scenario) UpdateNow(service string) string {
	s.t.Helper()
	resp, err := http.PostForm("http://localhost:"+queuePort+"/apply",
		url.Values{"service": {service}})
	if err != nil {
		s.t.Fatalf("pressing Update for %s: %v", service, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// The handler redirects back to the page carrying the result.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		s.t.Fatalf("pressing Update for %s: %s\n%s", service, resp.Status, body)
	}
	return string(body)
}

// Network is the compose network this scenario's services share, for a test
// that needs to reach one of them as a peer rather than through a published
// port.
//
// Asked of a running container rather than derived from the project name:
// compose's naming rules are its own business, and a test that reimplemented
// them would break on a version that changed them.
func (s *Scenario) Network(service string) string {
	s.t.Helper()
	container := s.Container(service)
	if container == "" {
		s.t.Fatalf("%s is not running, so it has no network", service)
	}
	out, err := exec.Command("docker", "inspect", container,
		"--format", `{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{end}}`).Output()
	if err != nil {
		s.t.Fatalf("reading %s's network: %v", service, err)
	}
	return trimmed(out)
}

// QueueContaining reads the hub's queue until it contains want, or fails.
//
// A hub serves a cache it refreshes on a timer, and its agents check on their
// own schedule -- so "the agent has found it" and "the page shows it" are two
// moments, not one. A test that read once would be asserting on whichever
// happened to come first, which is how it passed alone and failed in a suite.
func (s *Scenario) QueueContaining(want string) string {
	s.t.Helper()
	var body string
	s.waitFor("the queue to show "+want, 60*time.Second,
		func() bool {
			body = s.Queue()
			return strings.Contains(body, want)
		},
		func() string { return body })
	return body
}

// RootOwn makes a path inside the project root-owned, as the daemon leaves a
// bind mount whose host path did not exist.
//
// Through a container because this user cannot chown to root. Used to undo
// the harness's own kindness: Up creates the state directories as the test
// user, which is correct for every other scenario and is exactly what hid
// the unwritable-state bug.
func (s *Scenario) RootOwn(rel string) {
	s.t.Helper()
	out, err := exec.Command("docker", "run", "--rm",
		"-v", s.Dir+":/w", "alpine:3.20",
		"sh", "-c", "chown -R root:root /w/"+rel+" && chmod 755 /w/"+rel).CombinedOutput()
	if err != nil {
		s.t.Fatalf("making %s root-owned: %v\n%s", rel, err, out)
	}
}
