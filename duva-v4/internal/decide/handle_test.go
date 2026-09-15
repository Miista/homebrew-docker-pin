package decide

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// project writes a compose file and returns a Lookup over it. Real files
// rather than a fake, because what is being tested here is the reading: a
// mocked compose parser would agree with whatever this package assumed.
func project(t *testing.T, content string) Lookup {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Lookup{Root: file}
}

const pinnedProject = `
services:
  app:
    image: example.com/app:1.0.0@sha256:old
    container_name: my-app
    labels:
      duva.auto: patch
  loose:
    image: example.com/loose:latest
    container_name: my-loose
`

// --- Lookup ------------------------------------------------------------------

func TestLookupReadsAService(t *testing.T) {
	l := project(t, pinnedProject)
	svc, found, err := l.Service("my-app")
	if err != nil {
		t.Fatalf("Service: %v", err)
	}
	if !found {
		t.Fatal("the service was not found")
	}
	if svc.Name != "app" {
		t.Errorf("name = %q", svc.Name)
	}
	if svc.Image != "example.com/app" || svc.Tag != "1.0.0" {
		t.Errorf("image/tag = %q/%q", svc.Image, svc.Tag)
	}
	if svc.Digest != "sha256:old" {
		t.Errorf("digest = %q", svc.Digest)
	}
	if svc.Auto != AutoPatch {
		t.Errorf("auto = %q, want patch", svc.Auto)
	}
}

func TestLookupReportsAnUnknownContainer(t *testing.T) {
	l := project(t, pinnedProject)
	_, found, err := l.Service("nothing-like-this")
	if err != nil {
		t.Fatalf("Service: %v", err)
	}
	if found {
		t.Error("an unknown container was found")
	}
}

// An unpinned service is read, not refused -- being unpinned is a state to
// decide about, not an error.
func TestLookupReadsAnUnpinnedService(t *testing.T) {
	l := project(t, pinnedProject)
	svc, found, err := l.Service("my-loose")
	if err != nil || !found {
		t.Fatalf("Service: %v found=%v", err, found)
	}
	if svc.Pinned() {
		t.Error("a service with no digest reported as pinned")
	}
	if svc.Auto != AutoNone {
		t.Errorf("auto = %q, want the default none", svc.Auto)
	}
}

// A policy that cannot be read is not a policy to guess at.
func TestLookupRejectsAnUnreadablePolicy(t *testing.T) {
	l := project(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:old
    container_name: my-app
    labels:
      duva.auto: pathc
`)
	if _, _, err := l.Service("my-app"); err == nil {
		t.Fatal("want an error for a misspelled duva.auto")
	}
}

// A reference compose would substitute cannot be reasoned about as written.
func TestLookupRejectsAnUnexpandedVariable(t *testing.T) {
	l := project(t, `
services:
  app:
    image: example.com/app:${TAG}@sha256:old
    container_name: my-app
`)
	_, _, err := l.Service("my-app")
	if err == nil {
		t.Fatal("want an error for an unexpanded variable")
	}
	if !strings.Contains(err.Error(), "variable") {
		t.Errorf("error = %v", err)
	}
}

func TestLookupReportsAMissingFile(t *testing.T) {
	l := Lookup{Root: filepath.Join(t.TempDir(), "nope.yml")}
	if _, _, err := l.Service("anything"); err == nil {
		t.Fatal("want an error for a missing compose file")
	}
}

// --- Handler -----------------------------------------------------------------

func handlerOver(t *testing.T, content string, app Applier) (*Handler, *Pending) {
	t.Helper()
	q := NewPending()
	return &Handler{
		Lookup:  project(t, content),
		Queue:   q,
		Applier: app,
		Now:     func() time.Time { return now },
	}, q
}

func TestHandleQueuesWhenPolicySaysAsk(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{})
	// A minor, which duva.auto: patch does not cover.
	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.1.0", Digest: "sha256:new"})

	if res.Outcome != Queue {
		t.Errorf("outcome = %q, want queue (%s)", res.Outcome, res.Message)
	}
	e, ok := q.Get("app")
	if !ok {
		t.Fatal("nothing was queued")
	}
	if e.To != "1.1.0" || e.Kind != version.KindMinor {
		t.Errorf("entry = %+v", e)
	}
}

func TestHandleAppliesWhenPolicyAllows(t *testing.T) {
	app := &fakeApplier{}
	h, q := handlerOver(t, pinnedProject, app)
	// A patch, which duva.auto: patch covers.
	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.1", Digest: "sha256:new"})

	if res.Outcome != Apply {
		t.Fatalf("outcome = %q, want apply (%s)", res.Outcome, res.Message)
	}
	if len(app.started) != 1 || app.started[0].To != "1.0.1" {
		t.Errorf("started = %+v", app.started)
	}
	// Applying is not waiting, so it is not in the queue.
	if q.Len() != 0 {
		t.Errorf("an applied entry was left queued: %+v", q.List())
	}
}

// A decider that decided to apply and then dropped the decision would be
// worse than one that never decided.
func TestHandleQueuesWhenNothingCanApply(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, nil)
	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.1", Digest: "sha256:new"})

	if res.Outcome != Queue {
		t.Errorf("outcome = %q, want queue", res.Outcome)
	}
	if q.Len() != 1 {
		t.Error("the decision was lost")
	}
}

// Failing to start is not failing to decide: it stays queued so the next
// notice tries again and a person can see it waiting.
func TestHandleQueuesWhenStartingFails(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{err: errString("busy")})
	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.1", Digest: "sha256:new"})

	if res.Outcome != Queue {
		t.Errorf("outcome = %q, want queue", res.Outcome)
	}
	if !strings.Contains(res.Message, "busy") {
		t.Errorf("message = %q, want the reason", res.Message)
	}
	if q.Len() != 1 {
		t.Error("the decision was lost")
	}
}

func TestHandleRejectsAnUnknownContainer(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{})
	res := h.Handle(Notice{Container: "ghost", Image: "x:1", Digest: "sha256:a"})

	if res.Outcome != Reject {
		t.Errorf("outcome = %q, want reject", res.Outcome)
	}
	if res.Status != 422 {
		t.Errorf("status = %d, want 422", res.Status)
	}
	if q.Len() != 0 {
		t.Error("a rejected notice was queued")
	}
}

func TestHandleIgnoresWhatIsAlreadyCurrent(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{})
	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.0", Digest: "sha256:old"})

	if res.Outcome != Ignore {
		t.Errorf("outcome = %q, want ignore", res.Outcome)
	}
	if q.Len() != 0 {
		t.Error("something already current was queued")
	}
}

func TestHandleIgnoresAnUnpinnedService(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{})
	res := h.Handle(Notice{Container: "my-loose", Image: "example.com/loose:latest", Digest: "sha256:new"})

	if res.Outcome != Ignore {
		t.Errorf("outcome = %q, want ignore", res.Outcome)
	}
	if q.Len() != 0 {
		t.Error("an unpinned service was queued")
	}
}

// A digest move carries the digest through, because the detector said what
// the tag now points at. A version change does not -- resolving that is the
// actor's job, with more information than the decider has.
func TestDigestTravelsOnlyWithADigestMove(t *testing.T) {
	h, q := handlerOver(t, `
services:
  app:
    image: example.com/app:latest@sha256:old
    container_name: my-app
`, &fakeApplier{})

	h.Handle(Notice{Container: "my-app", Image: "example.com/app:latest", Digest: "sha256:new"})
	e, _ := q.Get("app")
	if e.Digest != "sha256:new" {
		t.Errorf("a digest move should carry the digest, got %q", e.Digest)
	}
	if e.Tag != "latest" {
		t.Errorf("a digest move should not change the tag, got %q", e.Tag)
	}
}

func TestVersionChangeCarriesNoDigest(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{})
	h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.1.0", Digest: "sha256:new"})

	e, _ := q.Get("app")
	if e.Digest != "" {
		t.Errorf("digest = %q, want empty -- the actor resolves it", e.Digest)
	}
	if e.Tag != "1.1.0" {
		t.Errorf("tag = %q, want the new version", e.Tag)
	}
}

// Re-notifying the same candidate must not log or report it as new every
// time, which is what Put's changed return is for.
func TestReNotifyingDoesNotDuplicate(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{})
	n := Notice{Container: "my-app", Image: "example.com/app:1.1.0", Digest: "sha256:new"}

	h.Handle(n)
	h.Handle(n)
	h.Handle(n)

	if q.Len() != 1 {
		t.Errorf("got %d entries, want 1", q.Len())
	}
}

func TestHandleReportsAnUnreadableProject(t *testing.T) {
	h := &Handler{
		Lookup: Lookup{Root: filepath.Join(t.TempDir(), "nope.yml")},
		Queue:  NewPending(),
	}
	res := h.Handle(Notice{Container: "anything"})
	if res.Outcome != Reject {
		t.Errorf("outcome = %q, want reject", res.Outcome)
	}
	if res.Status != 500 {
		t.Errorf("status = %d, want 500", res.Status)
	}
}

// The log is optional, and a handler without one must not panic.
func TestHandleWithoutALog(t *testing.T) {
	h := &Handler{Lookup: project(t, pinnedProject), Queue: NewPending()}
	h.Handle(Notice{Container: "ghost"})
}

// Someone is told once, when the candidate is new.
//
// Not the schedule -- a completed check advances that service's cutoff, so a
// tag is reported once. The delivery: the detector's outbox drops a finding
// only when the POST succeeded, so one received while the response was lost is
// re-sent next run. Put already tells a fresh candidate from a repeat, so the
// announcement rides on it.
func TestOnlyANewCandidateIsAnnounced(t *testing.T) {
	h, _ := handlerOver(t, pinnedProject, &fakeApplier{})
	var told []string
	h.Announce = func(e Entry) { told = append(told, e.To) }

	n := Notice{Container: "my-app", Image: "example.com/app:1.1.0", Digest: "sha256:new"}
	h.Handle(n)
	h.Handle(n) // the same candidate again, as the detector will send it
	h.Handle(n)

	if len(told) != 1 {
		t.Fatalf("announced %v, want exactly one for one candidate", told)
	}

	// A superseding candidate is news again.
	h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.2.0", Digest: "sha256:newer"})
	if len(told) != 2 {
		t.Errorf("announced %v, want a second for a different candidate", told)
	}
}

// A decider with nobody to tell still queues.
func TestNoAnnouncerStillQueues(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{})
	h.Announce = nil

	h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.1.0", Digest: "sha256:new"})
	if _, ok := q.Get("app"); !ok {
		t.Error("nothing was queued when no announcer was configured")
	}
}
