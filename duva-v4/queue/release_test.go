package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"

	q "github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
	ociversion "github.com/Miista/homebrew-docker-pin/oci/version"
)

// startRecorder is an Applier that records what it was handed.
type startRecorder struct {
	started []q.Entry
	err     error
}

func (s *startRecorder) Start(e q.Entry) error { return s.StartUnattended(e) }
func (s *startRecorder) StartUnattended(e q.Entry) error {
	if s.err != nil {
		return s.err
	}
	s.started = append(s.started, e)
	return nil
}
func (s *startRecorder) Stream(string, http.ResponseWriter, *http.Request) {}

var soakNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// soaking queues a patch for app, released at the given time.
func soaking(queue *q.Pending, releaseAt time.Time) {
	queue.Put(q.Entry{Service: "app", Image: "example.com/app", From: "1.0.0", To: "1.0.1",
		Tag: "1.0.1", Kind: ociversion.KindPatch, Auto: q.AutoPatch,
		ReleaseAt: releaseAt.Format(time.RFC3339)}, soakNow)
}

func releaserOver(queue *q.Pending, app q.Applier) *reconciler {
	return &reconciler{queue: queue, applier: app, log: zerolog.Nop(),
		now: func() time.Time { return soakNow }}
}

func TestADueEntryIsReleased(t *testing.T) {
	queue, app := q.NewPending(), &startRecorder{}
	soaking(queue, soakNow.Add(-time.Minute))

	releaserOver(queue, app).release()

	if len(app.started) != 1 || app.started[0].To != "1.0.1" {
		t.Fatalf("started = %+v, want the due entry", app.started)
	}
	if app.started[0].ReleaseAt != "" {
		t.Error("handed over still soaking")
	}
	// Still queued: the applier removes it on success, as for any auto-apply.
	if queue.Len() != 1 {
		t.Errorf("the entry left the queue before it was applied: %+v", queue.List())
	}
}

func TestAnEntryStillSoakingIsNotReleased(t *testing.T) {
	queue, app := q.NewPending(), &startRecorder{}
	soaking(queue, soakNow.Add(time.Hour))

	releaserOver(queue, app).release()

	if len(app.started) != 0 {
		t.Errorf("released early: %+v", app.started)
	}
}

// Released once. A failed apply waits for a person, as any auto-apply's does,
// rather than being offered again on every tick.
func TestAReleaseThatCannotStartIsNotRetriedEveryTick(t *testing.T) {
	queue, app := q.NewPending(), &startRecorder{err: errString("busy")}
	soaking(queue, soakNow.Add(-time.Minute))
	r := releaserOver(queue, app)

	r.release()
	app.err = nil
	r.release()

	if len(app.started) != 0 {
		t.Errorf("released again on the next tick: %+v", app.started)
	}
	if queue.Len() != 1 {
		t.Error("the entry was lost")
	}
}

// A container stopped while its update soaked must not be started by
// applying it. It keeps soaking, and goes the first tick it is back.
func TestAnInapplicableServiceStaysSoaking(t *testing.T) {
	queue, app := q.NewPending(), &startRecorder{}
	soaking(queue, soakNow.Add(-time.Minute))
	r := releaserOver(queue, app)
	running := false
	r.applicable = func(string) (bool, string) { return running, "stopped" }

	r.release()
	if len(app.started) != 0 {
		t.Fatalf("released for a stopped service: %+v", app.started)
	}
	running = true
	r.release()
	if len(app.started) != 1 {
		t.Errorf("not released once the service was back: %+v", app.started)
	}
}

func TestWithoutAnUpdaterNothingIsReleased(t *testing.T) {
	queue := q.NewPending()
	soaking(queue, soakNow.Add(-time.Minute))

	releaserOver(queue, nil).release()

	if e, _ := queue.Get("app"); e.ReleaseAt == "" {
		t.Error("released with nothing to hand it to")
	}
}

// Reconciled before released, so nothing goes out that the compose file has
// already moved past.
func TestATickDoesNotReleaseWhatTheFileMovedPast(t *testing.T) {
	file := composeFile(t, `
services:
  app:
    image: example.com/app:1.0.1@sha256:aaaa
    container_name: app
`)
	queue, app := q.NewPending(), &startRecorder{}
	soaking(queue, soakNow.Add(-time.Minute))
	r := releaserOver(queue, app)
	r.root = file

	r.tick()

	if len(app.started) != 0 {
		t.Errorf("released an update the file already has: %+v", app.started)
	}
	if queue.Len() != 0 {
		t.Errorf("not reconciled away: %+v", queue.List())
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A soaking entry the file still agrees with survives the reconcile, clock
// and all, and is released in the same tick.
func TestATickReleasesWhatIsStillWanted(t *testing.T) {
	file := composeFile(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:aaaa
    container_name: app
`)
	queue, app := q.NewPending(), &startRecorder{}
	soaking(queue, soakNow.Add(-time.Minute))
	r := releaserOver(queue, app)
	r.root = file

	r.tick()

	if len(app.started) != 1 {
		t.Errorf("started = %+v, want the due entry", app.started)
	}
}
