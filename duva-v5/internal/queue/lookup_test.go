package queue

import (
	"strings"
	"testing"
	"time"
)

// The file says one policy; the container was started under another and has
// not been recreated since. What governs the service is what it runs under.
func TestLookupReadsPolicyFromTheContainer(t *testing.T) {
	l := project(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:old
    container_name: my-app
    labels:
      duva.auto: major
`)
	var asked string
	l.Labels = func(container string) (map[string]string, bool, error) {
		asked = container
		return map[string]string{"duva.auto": "patch", "duva.defer": "3d"}, true, nil
	}

	svc, found, err := l.Service("my-app")
	if err != nil || !found {
		t.Fatalf("Service: %v found=%v", err, found)
	}
	if asked != "my-app" {
		t.Errorf("asked the container %q, want it by container_name", asked)
	}
	if svc.Auto != AutoPatch || svc.Soak != 72*time.Hour {
		t.Errorf("auto/soak = %s/%s, want the container's patch/72h, not the file's major", svc.Auto, svc.Soak)
	}
	// The image and pin still come from the file: that is what the updater
	// rewrites, and what the next apply starts from.
	if svc.Tag != "1.0.0" || svc.Digest != "sha256:old" {
		t.Errorf("tag/digest = %s/%s", svc.Tag, svc.Digest)
	}
}

// No container is no policy, not the file's: those labels describe nothing
// that runs.
func TestLookupWithoutAContainerIsAnError(t *testing.T) {
	l := project(t, pinnedProject)
	l.Labels = func(string) (map[string]string, bool, error) { return nil, false, nil }

	_, _, err := l.Service("my-app")
	if err == nil || !strings.Contains(err.Error(), "no container") {
		t.Errorf("err = %v, want one saying there is no container", err)
	}
}

// The earlier names are refused by name, not ignored: ignored, the service
// would lose its deferral without a word.
func TestTheOldDeferLabelsAreRefused(t *testing.T) {
	for _, old := range []string{"duva.soak", "duva.delay"} {
		l := project(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:old
    container_name: my-app
    labels:
      `+old+`: 3d
`)
		_, _, err := l.Service("my-app")
		if err == nil || !strings.Contains(err.Error(), old+" is now duva.defer") {
			t.Errorf("%s: err = %v, want one saying it is now duva.defer", old, err)
		}
	}
}
