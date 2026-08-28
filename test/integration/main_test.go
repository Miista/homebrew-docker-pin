//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Work that is the same for every scenario happens once per package: building
// the image under test, and generating the registry's certificate. Both are
// identical whatever a scenario declares, and neither is what any test is
// about -- rebuilding a byte-identical image and a second RSA key per test is
// pure cost.
//
// A package is the unit here, because a package is what TestMain wraps. Suites
// that need genuinely different setup want their own package rather than a
// hook inside this one.

// suiteImage is the duva image every scenario in this package refers to.
const suiteImage = "duva:integration"

// suiteCerts holds the certificate the registry serves and duva trusts,
// generated once and copied into each scenario.
var suiteCerts string

func TestMain(m *testing.M) {
	root, err := findRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: %v\n", err)
		os.Exit(1)
	}

	// No *testing.T here, so a setup failure is an exit rather than a test
	// failure. That is the right shape: nothing can run without these.
	if err := buildImage(root); err != nil {
		fmt.Fprintf(os.Stderr, "integration: building %s: %v\n", suiteImage, err)
		os.Exit(1)
	}

	dir, err := os.MkdirTemp("", "duva-certs")
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: %v\n", err)
		os.Exit(1)
	}
	if err := writeCertsTo(dir); err != nil {
		fmt.Fprintf(os.Stderr, "integration: generating certificates: %v\n", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	suiteCerts = dir

	code := m.Run()

	os.RemoveAll(suiteCerts)
	// The image is left: it is the artifact under test, rebuilt next run, and
	// removing it would throw away the layer cache that makes a rerun quick.
	os.Exit(code)
}

func findRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("finding the repository root: %w", err)
	}
	return trimmed(out), nil
}

// buildImage builds duva from the shipped Dockerfile.
//
// The real one, deliberately: a suite that builds its own image tests an
// artifact nobody ships, and the two drift. That is not hypothetical -- while
// the test image was FROM scratch and the shipped one was not, duva gained a
// runtime dependency on git and only the shipped image would have shown it.
func buildImage(root string) error {
	args := []string{"build", "-q", "--label", label + "=integration",
		"-f", filepath.Join("cmd", "duva", "Dockerfile"), "-t", suiteImage}
	if os.Getenv("GOCOVERDIR") != "" {
		args = append(args, "--build-arg", "COVER=1")
	}
	args = append(args, root)

	cmd := exec.Command("docker", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}
