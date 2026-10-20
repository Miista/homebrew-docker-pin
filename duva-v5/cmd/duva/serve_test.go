package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
)

type recordingQueue struct {
	got    []q.Notice
	status int
}

func (r *recordingQueue) Handle(n q.Notice) q.Result {
	r.got = append(r.got, n)
	return q.Result{Status: r.status}
}

func TestAFindingReachesTheQueueAsANotice(t *testing.T) {
	queue := &recordingQueue{status: http.StatusOK}
	published := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)

	ok := hand(queue, watch.Finding{Service: "app", Container: "app", Image: "example.com/app",
		Tag: "1.0.1", Digest: "sha256:abc", Published: published})

	if !ok {
		t.Error("a finding the queue took was reported as not landing")
	}
	if len(queue.got) != 1 {
		t.Fatalf("handed %d notice(s), want 1", len(queue.got))
	}
	n := queue.got[0]
	if n.Container != "app" || n.Image != "example.com/app:1.0.1" || n.Digest != "sha256:abc" || !n.Published.Equal(published) {
		t.Errorf("notice = %+v", n)
	}
}

// A queue that could not decide -- its compose file unreadable for a moment --
// leaves the finding with the watcher, which offers it again next run.
func TestAFindingTheQueueCouldNotTakeIsHeld(t *testing.T) {
	queue := &recordingQueue{status: http.StatusInternalServerError}
	if hand(queue, watch.Finding{Container: "app", Image: "app", Tag: "1"}) {
		t.Error("a finding the queue failed on was reported as landed")
	}
}

// Nothing to match it to, so nothing to hold it for: held, it would be
// offered again every run forever.
func TestAnIncompleteFindingIsDropped(t *testing.T) {
	queue := &recordingQueue{status: http.StatusOK}
	if !hand(queue, watch.Finding{Image: "app", Tag: "1"}) {
		t.Error("an unmatchable finding was held")
	}
	if len(queue.got) != 0 {
		t.Errorf("handed an unmatchable finding to the queue: %+v", queue.got)
	}
}

func TestAPanickingComponentIsStartedAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := 0
	err := supervise(ctx, zerolog.Nop(), func(context.Context) error {
		runs++
		if runs == 1 {
			panic("a bug")
		}
		return nil
	})
	if err != nil {
		t.Errorf("err = %v, want the second run's nil", err)
	}
	if runs != 2 {
		t.Errorf("ran %d time(s), want 2", runs)
	}
}

// One that keeps panicking is not going to stop, and takes the process down
// rather than looping.
func TestAComponentThatKeepsPanickingIsGivenUpOn(t *testing.T) {
	runs := 0
	err := supervise(context.Background(), zerolog.Nop(), func(context.Context) error {
		runs++
		panic("always")
	})
	if err == nil {
		t.Fatal("kept restarting a component that always panics")
	}
	if runs != 3 {
		t.Errorf("ran %d time(s), want 3", runs)
	}
}

// An error is a component's answer, not a crash: it is returned, not retried.
func TestAComponentErrorIsReturned(t *testing.T) {
	want := errors.New("cannot read the project")
	runs := 0
	err := supervise(context.Background(), zerolog.Nop(), func(context.Context) error {
		runs++
		return want
	})
	if !errors.Is(err, want) || runs != 1 {
		t.Errorf("err = %v after %d run(s), want %v after 1", err, runs, want)
	}
}

func TestHealthIsEveryListenerAnswering(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer sick.Close()
	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

	if got := healthOf([]string{host(ok)}); got != 0 {
		t.Errorf("healthy = %d, want 0", got)
	}
	if got := healthOf([]string{host(ok), host(sick)}); got != 1 {
		t.Errorf("one listener unhealthy = %d, want 1", got)
	}
	if got := healthOf([]string{"127.0.0.1:1"}); got != 1 {
		t.Errorf("nothing listening = %d, want 1", got)
	}
}
