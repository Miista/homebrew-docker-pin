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

// The registry the test images live in: its own compose project, started once
// for the package and left up.
//
// See hack/fixtures/infrastructure/docker-compose.yml for why it is not part
// of a scenario's world.

const (
	// registryProject keeps the registry out of any scenario's project, so a
	// scenario's teardown cannot take it down.
	registryProject = "duva-integration"
	// registryName is what the compose file calls the container, for the exec
	// that empties it.
	registryName = "duva-integration-registry"
	// registryHost is where images live. localhost, not a container-network
	// name: duva drives the HOST's daemon, which can only resolve names it
	// knows.
	registryHost = "localhost:5555"
	// registryStore is where the registry keeps what has been pushed to it,
	// inside the container.
	registryStore = "/var/lib/registry"
)

// infraDir is where the registry's compose file lives, and where its
// certificate is generated beside it.
func infraDir(root string) string {
	return filepath.Join(root, "hack", "fixtures", "infrastructure")
}

func registryCompose(root string, args ...string) *exec.Cmd {
	cmd := exec.Command("docker", append([]string{"compose",
		"--project-name", registryProject}, args...)...)
	// Run from the directory holding the compose file, so its relative paths
	// -- the certificate -- mean what they say.
	cmd.Dir = infraDir(root)
	return cmd
}

// startRegistry brings the registry up and waits for it to accept pushes.
//
// --wait blocks on the healthcheck in the compose file, so nothing here has
// to probe: the registry answers /v2/ only once it is ready.
func startRegistry(root string) error {
	// The certificate the registry serves, beside its compose file.
	certs := filepath.Join(infraDir(root), "certs")
	os.RemoveAll(certs)
	if err := os.MkdirAll(certs, 0o755); err != nil {
		return err
	}
	if err := writeCertsTo(certs); err != nil {
		return fmt.Errorf("generating the registry's certificate: %w", err)
	}
	suiteCerts = certs

	// Anything already holding the port goes first. Its contents are unknown,
	// and a test that finds unexpected tags is worse than one that waits a
	// second for a fresh registry.
	_ = registryCompose(root, "down", "--remove-orphans", "--volumes", "--timeout", "3").Run()

	cmd := registryCompose(root, "up", "-d", "--wait")
	if out, err := cmd.CombinedOutput(); err != nil {
		// A container that would not start explains itself in its own log,
		// which compose's error does not include.
		logs, _ := registryCompose(root, "logs", "--no-log-prefix").CombinedOutput()
		return fmt.Errorf("starting the registry: %w\n%s\nregistry said:\n%s", err, out, logs)
	}
	return nil
}

// The registry's storage goes with it: registry:2 declares VOLUME
// /var/lib/registry, so every run of this suite otherwise left one more
// anonymous volume on the host. Its contents are throwaway -- each scenario
// empties the registry before it pushes anything.
func stopRegistry(root string) {
	_ = registryCompose(root, "down", "--remove-orphans", "--volumes", "--timeout", "3").Run()
	_ = os.RemoveAll(filepath.Join(infraDir(root), "certs"))
}

// emptyRegistry deletes everything pushed to the registry.
//
// Both halves are needed, and for a while this did only the first. registry:3
// keeps an in-memory cache of blob descriptors, which `rm -rf` on its storage
// does not touch: the registry then believes it still holds blobs that are no
// longer on disk, and a later push of identical content is deduped against
// them. The manifest is written, the blobs are not, and the tag is served as
// though it existed -- so a pull gets the PREVIOUS test's image.
//
// What that looked like: the v4 pipeline reported no update, because the
// 1.0.1 it pulled was byte-identical to the 1.0.0 already running. Only in
// the suite, only after a pin test pushed its own app:1.0.1, and never when
// the test ran alone -- which is the shape of every cache bug.
//
// Restarting drops the cache with the process. The race the previous approach
// avoided is handled by waiting for the healthcheck, which is the same gate
// `compose up --wait` uses at startup.
func emptyRegistry() error {
	out, err := exec.Command("docker", "exec", registryName,
		"sh", "-c", "rm -rf "+registryStore+"/docker && mkdir -p "+registryStore).CombinedOutput()
	if err != nil {
		return fmt.Errorf("emptying the registry: %w\n%s", err, out)
	}
	if out, err := exec.Command("docker", "restart", registryName).CombinedOutput(); err != nil {
		return fmt.Errorf("restarting the registry: %w\n%s", err, out)
	}
	return waitForRegistry()
}

// waitForRegistry blocks until the registry will accept pushes, which is what
// its own healthcheck reports.
func waitForRegistry() error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect", registryName,
			"--format", "{{.State.Health.Status}}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "healthy" {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("the registry did not become healthy within 30s of being restarted")
}

// writeCertsTo generates the certificate the registry serves and duva trusts.
//
// Called once per package: it is identical for every scenario, and a second
// RSA key is pure cost. Each scenario gets a copy inside its own project, so
// the compose file can name it relatively and needs no variable only a test
// could expand.
//
// Scaffolding that exists only because the registry is local: a real one is
// trusted by the image's own CA bundle.
func writeCertsTo(dir string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
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
		return err
	}

	write := func(name string, block *pem.Block) error {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		defer f.Close()
		return pem.Encode(f, block)
	}
	if err := write("cert.pem", &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return err
	}
	return write("key.pem", &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
