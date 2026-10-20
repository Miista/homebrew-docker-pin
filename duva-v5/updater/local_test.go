package updater

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/update"
)

// A server whose work waits for release, so a test can look at it in flight.
func localServer(release <-chan struct{}, status update.Status, reason string) *Server {
	return &Server{
		Log: zerolog.Nop(),
		Apply: func(r update.Request, step func(string, ...any)) (update.Status, string) {
			step("pulling %s", r.To)
			<-release
			return status, reason
		},
	}
}

func TestLocalFollowsAnApplyToItsEnd(t *testing.T) {
	release := make(chan struct{})
	u := localServer(release, update.Completed, "").Local()

	acc, err := u.Apply(context.Background(), update.Request{Service: "app", To: "1.0.1"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	close(release)

	var tee bytes.Buffer
	status, reason, err := u.Watch(context.Background(), acc.Stream, &tee)
	if err != nil || status != update.Completed {
		t.Fatalf("Watch = %s %q %v, want completed", status, reason, err)
	}
	if !strings.Contains(tee.String(), "pulling 1.0.1") {
		t.Errorf("progress was not copied through:\n%s", tee.String())
	}
}

// The same status a queue would have read over HTTP, reason included.
func TestLocalReportsAFailureWithItsReason(t *testing.T) {
	release := make(chan struct{})
	close(release)
	u := localServer(release, update.Failed, "the container would not start").Local()

	acc, _ := u.Apply(context.Background(), update.Request{Service: "app"})
	status, reason, err := u.Watch(context.Background(), acc.Stream, nil)
	if err != nil || status != update.Failed || reason != "the container would not start" {
		t.Errorf("Watch = %s %q %v", status, reason, err)
	}
}

func TestLocalRefusesASecondApplyForTheSameService(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	u := localServer(release, update.Completed, "").Local()

	if _, err := u.Apply(context.Background(), update.Request{Service: "app"}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Apply(context.Background(), update.Request{Service: "app"}); err == nil {
		t.Error("started a second apply for a service already being updated")
	}
}

// A watcher arriving after the end still gets the end: it is read from the
// stream's record, not from lines it was there to receive.
func TestLocalWatchingAFinishedApply(t *testing.T) {
	release := make(chan struct{})
	close(release)
	s := localServer(release, update.Completed, "")
	u := s.Local()
	acc, _ := u.Apply(context.Background(), update.Request{Service: "app"})

	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		done := s.running["app"].finished()
		s.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status, _, err := u.Watch(context.Background(), acc.Stream, nil); err != nil || status != update.Completed {
		t.Errorf("Watch = %s %v, want completed", status, err)
	}
}

func TestLocalReadinessIsTheServers(t *testing.T) {
	s := &Server{Log: zerolog.Nop(), Ready: func() update.Readiness {
		return update.Readiness{Ready: false, Reason: "dirty"}
	}}
	if r := s.Local().Ready(context.Background()); r.Ready || r.Reason != "dirty" {
		t.Errorf("Ready = %+v", r)
	}
}
