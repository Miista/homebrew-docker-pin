package integration

// What the package needs before any scenario can run: the image under test,
// the receiver that stands in for ntfy, and the certificate the registry
// serves.
//
// Here rather than in main_test.go because the sandbox uses them too --
// standing a scenario up by hand and testing one are the same work.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// The v4 pipeline: three images, because it is three processes. A scenario
// declares whichever of them it is about -- the watcher alone is a legitimate
// world, and so is a queue with nothing to apply with.
const (
	watchImage  = "duva-watch:integration"
	queueImage  = "duva-queue:integration"
	updateImage = "duva-update:integration"
)

// v4Images maps each image to the Dockerfile that builds it, so adding a
// stage is one line rather than a fourth near-identical function.
var v4Images = map[string]string{
	watchImage:  filepath.Join("duva-v4", "watch", "Dockerfile"),
	queueImage:  filepath.Join("duva-v4", "queue", "Dockerfile"),
	updateImage: filepath.Join("duva-v4", "update", "Dockerfile"),
}

// receiverImage stands in for ntfy, for the scenarios that assert on what
// duva announced.
const receiverImage = "duva-receiver:integration"

// suiteCerts holds the certificate the registry serves and duva trusts,
// generated once and copied into each scenario.
var suiteCerts string

// buildReceiver builds the notification receiver from its own source, rather
// than a heredoc: it is ordinary Go that compiles and vets with everything
// else.
func buildReceiver(root string) error {
	dir, err := os.MkdirTemp("", "duva-receiver")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	build := exec.Command("go", "build", "-ldflags", "-s -w", "-o",
		filepath.Join(dir, "receiver"), "./test/integration/receiver")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}

	// FROM scratch: nothing is pulled, so the suites work offline.
	dockerfile := "FROM scratch\nCOPY receiver /receiver\nCMD [\"/receiver\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return err
	}

	cmd := exec.Command("docker", "build", "-q",
		"--label", label+"=integration", "-t", receiverImage, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

// buildV4 builds the three pipeline images, from the shipped Dockerfiles for
// the same reason buildImage does: a suite that builds its own artifact tests
// one nobody ships.
func buildV4(root string) error {
	for image, dockerfile := range v4Images {
		args := []string{"build", "-q", "--label", label + "=integration",
			"-f", dockerfile, "-t", image}
		if os.Getenv("GOCOVERDIR") != "" {
			args = append(args, "--build-arg", "COVER=1")
		}
		args = append(args, root)

		cmd := exec.Command("docker", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("building %s: %w\n%s", image, err, out)
		}
	}
	return nil
}

func findRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("finding the repository root: %w", err)
	}
	return trimmed(out), nil
}

// Setup prepares what every scenario needs and returns the repository root.
//
// Building the image under test, building the receiver that stands in for
// ntfy, and starting the registry: all identical whatever a scenario
// declares, and none of it what any test is about. Rebuilding a
// byte-identical image per test is pure cost.
//
// Shared with the sandbox, which needs exactly the same world.
func Setup() (string, error) {
	root, err := findRoot()
	if err != nil {
		return "", err
	}
	if err := buildReceiver(root); err != nil {
		return "", fmt.Errorf("building %s: %w", receiverImage, err)
	}
	if err := buildV4(root); err != nil {
		return "", err
	}
	// Generates the certificate beside the registry's compose file, then
	// brings the registry up and waits for it to accept pushes.
	if err := startRegistry(root); err != nil {
		return "", err
	}
	return root, nil
}

// Teardown removes what Setup started.
//
// The images are left: they are the artifacts under test, rebuilt next run,
// and removing them would throw away the layer cache that makes a rerun
// quick.
func Teardown(root string) {
	stopRegistry(root)
}

// Down empties the testbed, removing whatever a sandbox left running.
func Down(t T, root string) {
	s := &Scenario{t: t, root: root, Dir: filepath.Join(root, "testbed")}
	s.sweep()
}

// Scenarios lists every fixture, as <suite>/<scenario>.
func Scenarios() []string {
	root, err := findRoot()
	if err != nil {
		return nil
	}
	fixtures := filepath.Join(root, "hack", "fixtures")
	suites, err := os.ReadDir(fixtures)
	if err != nil {
		return nil
	}

	var names []string
	for _, suite := range suites {
		if !suite.IsDir() || suite.Name() == "common" || suite.Name() == "infrastructure" {
			continue
		}
		scenarios, err := os.ReadDir(filepath.Join(fixtures, suite.Name()))
		if err != nil {
			continue
		}
		for _, scenario := range scenarios {
			if scenario.IsDir() {
				names = append(names, suite.Name()+"/"+scenario.Name())
			}
		}
	}
	sort.Strings(names)
	return names
}
