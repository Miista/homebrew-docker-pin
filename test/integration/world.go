//go:build integration

package integration

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The parts of a scenario's world that are not the compose file: the duva
// image under test, the registry's certificate, the git repository duva
// commits into, and the images the fixture refers to.

// buildDuvaImage builds the image the fixture names, from the shipped
// Dockerfile.
//
// The real one, deliberately: a suite that builds its own image tests an
// artifact nobody ships, and the two drift. That is not hypothetical -- while
// the test image was FROM scratch and the shipped one was not, duva gained a
// runtime dependency on git and only the shipped image would have shown it.
func (s *Scenario) buildDuvaImage() {
	s.t.Helper()
	args := []string{"build", "-q", "--label", label + "=" + s.suite(),
		"-f", filepath.Join("cmd", "duva", "Dockerfile"), "-t", s.Image}
	if dir := os.Getenv("GOCOVERDIR"); dir != "" {
		args = append(args, "--build-arg", "COVER=1")
	}
	args = append(args, s.root)

	cmd := exec.Command("docker", args...)
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		s.t.Fatalf("building %s: %v\n%s", s.Image, err, out)
	}
}

// label marks everything a scenario creates, so a run killed outright can
// still be swept by the next one.
const label = "io.github.miista.docker-pin.integration"

func (s *Scenario) suite() string {
	suite, _, _ := strings.Cut(s.Name, "/")
	return suite
}

// writeCerts puts a self-signed certificate where the fixture expects it.
//
// Inside the project, so the compose file can name it relatively and needs no
// variable only a test could expand. The registry serves TLS with it, and
// duva trusts it by mounting it over the image's CA bundle -- scaffolding
// that exists only because the registry is local. A real one needs neither.
func (s *Scenario) writeCerts() {
	s.t.Helper()
	dir := filepath.Join(s.Dir, "certs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		s.t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "testregistry"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Both names the registry answers to: testregistry from another
		// container, localhost from the host.
		DNSNames:    []string{"testregistry", "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		s.t.Fatal(err)
	}

	write := func(name string, block *pem.Block) {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			s.t.Fatal(err)
		}
		defer f.Close()
		if err := pem.Encode(f, block); err != nil {
			s.t.Fatal(err)
		}
	}
	write("cert.pem", &pem.Block{Type: "CERTIFICATE", Bytes: der})
	write("key.pem", &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

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
// The tag is baked into the binary, so two tags never produce the same digest
// by accident: duva compares digests, and identical content would make "the
// container was replaced" indistinguishable from "nothing happened".
func (s *Scenario) Push(repo, tag string) {
	s.t.Helper()
	dir := filepath.Join(s.Dir, ".build")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}

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
`, tag)
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
	s.docker("build", "-q", "--label", label+"="+s.suite(), "-t", ref, dir)
	s.docker("push", "-q", ref)
}

// PushUnrunnable publishes an image that exists but cannot start: FROM
// scratch with no command. For testing what duva does when a container
// refuses the image it was just pinned to.
func (s *Scenario) PushUnrunnable(repo, tag string) {
	s.t.Helper()
	dir := filepath.Join(s.Dir, ".broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	s.writeFile(filepath.Join(dir, "marker"), repo+":"+tag)
	s.writeFile(filepath.Join(dir, "Dockerfile"), "FROM scratch\nCOPY marker /marker\n")

	ref := fmt.Sprintf("%s/%s:%s", registryHost, repo, tag)
	s.docker("build", "-q", "--label", label+"="+s.suite(), "-t", ref, dir)
	s.docker("push", "-q", ref)
}

// registryHost is where images live. localhost, not the registry's network
// name: duva drives the HOST's daemon, which can only resolve names it knows.
const registryHost = "localhost:5555"

func (s *Scenario) writeFile(path, content string) {
	s.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}
