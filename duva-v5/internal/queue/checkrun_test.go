package queue

import (
	"errors"
	"testing"
	"time"
)

// A check a person asked for tells them what became of each release, so each
// way a finding can end names its fate.
func TestHandleSaysWhatBecameOfARelease(t *testing.T) {
	t.Run("a patch the policy covers is applying", func(t *testing.T) {
		h, _ := handlerOver(t, pinnedProject, &fakeApplier{})
		res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.1", Digest: "sha256:new"})
		if res.Fate != FateApplying || res.Label != "1.0.1" {
			t.Errorf("fate = %q label = %q, want applying 1.0.1", res.Fate, res.Label)
		}
	})
	t.Run("a minor the policy does not cover waits for a person", func(t *testing.T) {
		h, _ := handlerOver(t, pinnedProject, &fakeApplier{})
		res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.1.0", Digest: "sha256:new"})
		if res.Fate != FateWaiting || res.Label != "1.1.0" {
			t.Errorf("fate = %q label = %q, want waiting 1.1.0", res.Fate, res.Label)
		}
	})
	t.Run("an apply that would not start waits for a person", func(t *testing.T) {
		h, _ := handlerOver(t, pinnedProject, &fakeApplier{err: errString("busy")})
		res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.1", Digest: "sha256:new"})
		if res.Fate != FateWaiting {
			t.Errorf("fate = %q, want waiting", res.Fate)
		}
	})
	t.Run("what is already current is skipped, with a reason", func(t *testing.T) {
		h, _ := handlerOver(t, pinnedProject, &fakeApplier{})
		res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.0", Digest: "sha256:old"})
		if res.Fate != FateSkipped || res.Message == "" {
			t.Errorf("fate = %q message = %q, want skipped with a reason", res.Fate, res.Message)
		}
	})
	t.Run("an unknown container is rejected", func(t *testing.T) {
		h, _ := handlerOver(t, pinnedProject, &fakeApplier{})
		res := h.Handle(Notice{Container: "nothing-like-this", Image: "example.com/app:1.1.0"})
		if res.Fate != FateRejected {
			t.Errorf("fate = %q, want rejected", res.Fate)
		}
	})
}

// A deferred release says when it is due, and whether it then applies itself
// or comes to a person.
func TestHandleSaysWhenADeferredReleaseIsDue(t *testing.T) {
	deferred := func(auto string) string {
		return `
services:
  app:
    image: example.com/app:1.0.0@sha256:old
    container_name: my-app
    labels:
      duva.auto: ` + auto + `
      duva.defer: 48h
`
	}
	n := Notice{Container: "my-app", Image: "example.com/app:1.0.1", Digest: "sha256:new", Published: now}

	h, _ := handlerOver(t, deferred("patch"), &fakeApplier{})
	res := h.Handle(n)
	if res.Fate != FateApplyLater || res.Until == "" {
		t.Errorf("covered: fate = %q until = %q, want apply-later with a time", res.Fate, res.Until)
	}

	h, _ = handlerOver(t, deferred("none"), &fakeApplier{})
	res = h.Handle(n)
	if res.Fate != FateHeld || res.Until == "" {
		t.Errorf("not covered: fate = %q until = %q, want held with a time", res.Fate, res.Until)
	}
}

// What a person asked for is on their screen already, so nobody is told as
// well. The release is still queued; only the announcement is dropped.
func TestAQuietNoticeIsQueuedButNotAnnounced(t *testing.T) {
	h, q := handlerOver(t, pinnedProject, &fakeApplier{})
	var told []string
	h.Announce = func(e Entry) { told = append(told, e.To) }

	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.1.0", Digest: "sha256:new", Quiet: true})
	if res.Fate != FateWaiting {
		t.Errorf("fate = %q, want waiting", res.Fate)
	}
	if len(told) != 0 {
		t.Errorf("announced %v for a quiet notice", told)
	}
	if q.Len() != 1 {
		t.Errorf("a quiet notice was not queued: %+v", q.List())
	}

	// The same release found by the schedule is not news any more: it is
	// already waiting. That is the cost of being quiet, and it is accepted.
	h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.1.0", Digest: "sha256:new"})
	if len(told) != 0 {
		t.Errorf("announced %v for a release already waiting", told)
	}

	// A different release, found by the schedule, is announced as ever.
	h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.2.0", Digest: "sha256:newer"})
	if len(told) != 1 {
		t.Errorf("announced %v, want one for a release nobody asked about", told)
	}
}

// An asked check is a numbered run on the snapshot, filled in as it goes and
// done when the check ends.
func TestAnAskedCheckReportsItsRun(t *testing.T) {
	release := make(chan struct{})
	s := &Server{
		Queue: NewPending(),
		Check: func(service string, _ time.Duration, rec *CheckRecorder) error {
			rec.Found(Result{Service: "app", Label: "1.0.1", Fate: FateApplying})
			rec.Checked("app", nil)
			rec.Checked("broken", errors.New("no such image"))
			<-release
			return nil
		},
	}
	if run := s.Snapshot().Check; run != nil {
		t.Fatalf("a run before any check was asked: %+v", run)
	}
	if msg := s.CheckAll("tester", 0); msg != "" {
		t.Fatalf("CheckAll: %s", msg)
	}

	var run *CheckRun
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if run = s.Snapshot().Check; run != nil && run.Checked == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if run == nil || run.Seq != 1 || run.Done {
		t.Fatalf("while running: %+v, want run 1, not done", run)
	}
	if len(run.Findings) != 1 || run.Findings[0].Fate != FateApplying {
		t.Errorf("findings = %+v", run.Findings)
	}
	if len(run.Failed) != 1 || run.Failed[0].Service != "broken" {
		t.Errorf("failed = %+v", run.Failed)
	}

	close(release)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if run = s.Snapshot().Check; run.Done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !run.Done {
		t.Errorf("the run never finished: %+v", run)
	}
}

// A check that cannot run at all says why, rather than ending silently.
func TestAnAskedCheckThatFailsSaysWhy(t *testing.T) {
	s := &Server{
		Queue: NewPending(),
		Check: func(string, time.Duration, *CheckRecorder) error { return errors.New("no compose file") },
	}
	s.CheckAll("tester", 0)
	var run *CheckRun
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if run = s.Snapshot().Check; run != nil && run.Done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if run == nil || !run.Done || run.Error != "no compose file" {
		t.Errorf("run = %+v, want done with the error", run)
	}
}

// A scheduled check has no recorder, and calls the same code.
func TestANilRecorderRecordsNothing(t *testing.T) {
	var rec *CheckRecorder
	rec.Found(Result{Service: "app"})
	rec.Checked("app", nil)
}
