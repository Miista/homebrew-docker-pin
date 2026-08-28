//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
)

// docker pin against a real registry.
//
// In `image: <name>:<tag>@sha256:<digest>` the tag is an INSTRUCTION -- which
// stream of releases this service follows -- and the digest is the RECORD of
// what actually runs. One field cannot be both, so the tag is never rewritten
// to describe what was pinned.
//
// The unit tests fake the digest lookups, so they cannot catch a tag being
// rewritten. That is not hypothetical: pinning used to resolve a moving tag
// to whatever concrete version carried the same digest, which silently turned
// "follow latest" into "pinned to 1.26 forever" -- a service stopped
// receiving updates the moment it was pinned.

// composeWith is a one-service compose file, which is all these need.
func composeWith(image string) string {
	return fmt.Sprintf("services:\n  web:\n    image: %s\n", image)
}

func TestPinKeepsAMovingTag(t *testing.T) {
	s := Bare(t)
	s.Push("app", "latest")
	s.WriteCompose(composeWith(registryHost + "/app:latest"))

	if out, ok := s.Run("pin", "web"); !ok {
		t.Fatalf("pin failed: %s", out)
	}

	got := s.Image("web")
	if !strings.Contains(got, ":latest@sha256:") {
		t.Errorf("the followed tag was not kept:\n  got %s", got)
	}
	// The bug this guards: any concrete version tag in place of latest.
	if strings.Contains(got, ":1.0.") {
		t.Errorf("latest was resolved to a concrete version tag:\n  got %s", got)
	}
}

func TestPinKeepsAConcreteTag(t *testing.T) {
	s := Bare(t)
	s.Push("app", "1.0.0")
	s.WriteCompose(composeWith(registryHost + "/app:1.0.0"))

	if out, ok := s.Run("pin", "web"); !ok {
		t.Fatalf("pin failed: %s", out)
	}

	if got := s.Image("web"); !strings.Contains(got, ":1.0.0@sha256:") {
		t.Errorf("a concrete tag was not preserved:\n  got %s", got)
	}
}

// upgrade re-resolves the digest for the tag already in the file. The tag is
// already meaningful -- whatever the service was tracking -- so there is
// nothing to resolve.
func TestUpgradeKeepsTheFollowedTag(t *testing.T) {
	s := Bare(t)
	s.Push("app", "latest")
	stale := s.Digest("app", "latest")
	s.WriteCompose(composeWith(registryHost + "/app:latest@" + stale))

	// The tag now points somewhere else.
	s.Push("app", "latest", "second")

	if out, ok := s.Run("upgrade", "web"); !ok {
		t.Fatalf("upgrade failed: %s", out)
	}

	got := s.Image("web")
	if !strings.Contains(got, ":latest@sha256:") {
		t.Errorf("upgrade did not keep the followed tag:\n  got %s", got)
	}
	if strings.HasSuffix(got, "@"+stale) {
		t.Errorf("upgrade did not move the digest:\n  still %s", got)
	}
}

// The one operation that does change the tag, because changing it is what was
// asked for.
func TestUpgradeToAnExplicitVersionChangesTheTag(t *testing.T) {
	s := Bare(t)
	s.Push("app", "latest")
	s.Push("app", "1.0.1")
	s.WriteCompose(composeWith(registryHost + "/app:latest@" + s.Digest("app", "latest")))

	if out, ok := s.Run("upgrade", "web", "1.0.1"); !ok {
		t.Fatalf("upgrade failed: %s", out)
	}

	if got := s.Image("web"); !strings.Contains(got, ":1.0.1@sha256:") {
		t.Errorf("an explicit version should become the new followed tag:\n  got %s", got)
	}
}

// The real scenario: bring a stack up, live with it, then pin. Meanwhile
// something else re-pulls the moving tag, so the local image for that tag is
// newer than what the container is running. Pinning must record what RUNS --
// otherwise the file claims a digest that never ran here.
func TestPinRecordsWhatIsRunningNotWhatTheTagPointsAt(t *testing.T) {
	s := Bare(t)

	s.Push("runnable", "v1")
	running := s.Digest("runnable", "v1")

	// Pulled by digest, because that is what gives the image a durable
	// name@digest identity: when a tag is repointed, docker strips the repo
	// digest from the old image, but one pulled by digest keeps it. That is
	// exactly the state a host is in when a tag has moved upstream and the
	// container has not been recreated.
	ref := registryHost + "/runnable@" + running
	s.docker("rmi", "-f", registryHost+"/runnable:v1")
	s.docker("pull", "-q", ref)
	s.docker("tag", ref, registryHost+"/runnable:v1")

	s.WriteCompose(composeWith(registryHost + "/runnable:v1"))
	s.composeUp()

	// The tag now points at different content, but the container is untouched.
	s.Push("runnable", "v1", "second")
	moved := s.Digest("runnable", "v1")

	if out, ok := s.Run("pin", "web"); !ok {
		t.Fatalf("pin failed: %s", out)
	}

	got := s.Image("web")
	if !strings.HasSuffix(got, "@"+running) {
		t.Errorf("pin should record the digest the container is running:\n  got  %s\n  want ...@%s", got, running)
	}
	if strings.HasSuffix(got, "@"+moved) {
		t.Errorf("pin recorded the newer digest sitting on the tag:\n  got %s", got)
	}
}

// --all is exactly "pin every service", by the same rules: the running one
// from its container, the idle one from the local image.
func TestPinAllMixesRunningAndIdleServices(t *testing.T) {
	s := Bare(t)

	s.Push("runnable", "v1")
	running := s.Digest("runnable", "v1")
	ref := registryHost + "/runnable@" + running
	s.docker("rmi", "-f", registryHost+"/runnable:v1")
	s.docker("pull", "-q", ref)
	s.docker("tag", ref, registryHost+"/runnable:v1")
	s.Push("app", "1.0.0")

	s.WriteCompose(fmt.Sprintf(
		"services:\n  web:\n    image: %s/runnable:v1\n  idle:\n    image: %s/app:1.0.0\n",
		registryHost, registryHost))
	s.composeUp("web")

	if out, ok := s.Run("pin", "--all"); !ok {
		t.Fatalf("pin --all failed: %s", out)
	}

	if got := s.Image("web"); !strings.HasSuffix(got, "@"+running) {
		t.Errorf("--all should pin the running service from its container:\n  got %s", got)
	}
	if got := s.Image("idle"); !strings.Contains(got, ":1.0.0@sha256:") {
		t.Errorf("--all should pin the stopped service from the local image:\n  got %s", got)
	}
}

// The bug this guards: `pin --all --dry-riun` matched --all before checking
// the argument count, dropped the mistyped flag, and rewrote every compose
// file for real. A typo in a safety flag must never become a live run.
func TestUnknownFlagsAreRejected(t *testing.T) {
	s := Bare(t)
	s.Push("app", "1.0.0")
	s.WriteCompose(composeWith(registryHost + "/app:1.0.0"))
	before := s.Image("web")

	for _, args := range [][]string{
		{"pin", "--all", "--dry-riun"},
		{"pin", "web", "--dry-riun"},
		{"pin", "--typo"},
		{"upgrade", "--all", "--dry-riun"},
		{"upgrade", "web", "--dry-riun"},
	} {
		if out, ok := s.Run(args...); ok {
			t.Errorf("`%s` should have failed, but succeeded:\n%s", strings.Join(args, " "), out)
		}
	}

	if got := s.Image("web"); got != before {
		t.Errorf("a rejected command still touched the compose file:\n  was %s\n  now %s", before, got)
	}
}

// A built image's repo digest is local to this daemon, so pinning it would
// produce a reference no other host can pull.
func TestLocallyBuiltServiceIsLeftUnpinned(t *testing.T) {
	s := Bare(t)
	s.writeFile(s.Dir+"/app/Dockerfile", "FROM scratch\n")
	s.WriteCompose("services:\n  app:\n    build: ./app\n    image: integration-built:local\n")

	if out, ok := s.Run("pin", "app"); !ok {
		t.Fatalf("pin failed: %s", out)
	}

	if got := s.Image("app"); got != "integration-built:local" {
		t.Errorf("a built service should be left unpinned:\n  got %s", got)
	}
}
