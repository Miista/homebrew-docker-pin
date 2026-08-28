//go:build integration

package integration

import (
	"slices"
	"strings"
	"testing"
)

// duva applying an update: pull, rewrite the pin, recreate the container,
// commit. Everything is real except the world it runs in -- a real registry,
// a real daemon, a container that is genuinely replaced, a real repository,
// all local.
//
// The unit tests fake docker and git, so what they cannot show is whether the
// container actually ends up running the image duva pinned, or whether the
// commit really lands.

func TestUpdateWithinPolicyIsApplied(t *testing.T) {
	s := Up(t, "duva-apply/within-policy")

	// A patch, which duva.auto: patch allows.
	s.PushAdditional()
	s.Pin("app")

	before := s.Container("app")
	if before == "" {
		t.Fatal("the service should be running before duva looks at it")
	}

	s.Start()

	if got := s.Image("app"); !strings.Contains(got, ":1.0.1@sha256:") {
		t.Errorf("the compose file was not repinned:\n  got %s", got)
	}

	after := s.Container("app")
	if after == "" {
		t.Fatal("the service is not running after the update")
	}
	if after == before {
		t.Error("the container was not replaced")
	}
	if got := s.RunningImage(after); !strings.Contains(got, "1.0.1") {
		t.Errorf("the container is not on the new image:\n  got %s", got)
	}

	if got := s.Log(); !strings.Contains(got, "app: 1.0.0 -> 1.0.1") {
		t.Errorf("the change was not committed with a readable subject:\n  got %s", got)
	}
	if got := s.Status(); got != "" {
		t.Errorf("the repository should be left clean, got:\n%s", got)
	}
}

func TestUpdateBeyondPolicyWaitsForAHuman(t *testing.T) {
	s := Up(t, "duva-apply/beyond-policy")

	// A major, which duva.auto: patch does not cover.
	s.PushAdditional()
	s.Pin("app")

	before := s.Image("app")
	s.Start()

	if got := s.Image("app"); got != before {
		t.Errorf("the compose file should be untouched:\n  was %s\n  now %s", before, got)
	}
	if got := s.Log(); strings.Contains(got, "2.0.0") {
		t.Errorf("nothing should have been committed:\n  got %s", got)
	}
	if got := s.Queued(); !slices.Contains(got, "app") {
		t.Errorf("the update should be queued for approval, queue is %v", got)
	}
}

// The one case duva undoes: the container refuses the new image, so it never
// took it, and the file must not keep claiming it did.
func TestRefusedImagePutsTheFileBack(t *testing.T) {
	s := Up(t, "duva-apply/refused-image")
	s.Pin("app")

	// 1.0.1 exists but cannot run: FROM scratch, no command. The recreate
	// fails after the pin has been written.
	s.PushUnrunnable("app", "1.0.1")

	before := s.Image("app")
	s.Start()

	if got := s.Image("app"); got != before {
		t.Errorf("the compose file was not put back:\n  was %s\n  now %s", before, got)
	}
	if got := s.Status(); got != "" {
		t.Errorf("no half-applied change should be left behind, got:\n%s", got)
	}
	if got := s.Log(); strings.Contains(got, "1.0.1") {
		t.Errorf("a failed update must not be committed:\n  got %s", got)
	}
}

// duva sees the compose file at its own mount point rather than the host
// path, so the question is whether a relative bind still resolves to the
// host's directory once duva recreates the container.
//
// It does, because duva recreates through the docker API: a running
// container's binds are already absolute, so nothing is re-resolved. Tested
// rather than assumed, since a data volume silently pointed at the wrong
// place loses data quietly.
func TestRelativeBindSurvivesARecreate(t *testing.T) {
	s := Up(t, "duva-apply/relative-bind")

	s.PushAdditional()
	s.Pin("app")

	want := s.Dir + "/conf"
	if got := s.BindSource(s.Container("app"), "/mnt"); got != want {
		t.Fatalf("the bind is wrong before duva touches it:\n  got  %s\n  want %s", got, want)
	}

	s.Start()

	if got := s.Image("app"); !strings.Contains(got, ":1.0.1@sha256:") {
		t.Fatalf("the update was not applied, so there is nothing to check:\n  got %s", got)
	}
	if got := s.BindSource(s.Container("app"), "/mnt"); got != want {
		t.Errorf("the relative bind no longer points at the host directory:\n  got  %s\n  want %s", got, want)
	}
}
