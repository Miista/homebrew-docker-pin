package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/actor"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/decide"
)

// stubActor is an HTTP server standing in for an actor, so the applier is
// exercised through the real client and the real contract.
type stubActor struct {
	mu sync.Mutex
	// lines are streamed for each apply, with a pause between them so a
	// watcher has a chance to attach mid-flight.
	lines []string
	pause time.Duration
	// applies counts how many were started.
	applies int
	// inFlight is how many are running at once, and peak the most ever.
	inFlight, peak int
}

func (s *stubActor) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/apply", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.applies++
		s.mu.Unlock()
		json.NewEncoder(w).Encode(actor.Accepted{Stream: "/v1/stream/x"})
	})
	mux.HandleFunc("/v1/stream/", func(w http.ResponseWriter, r *http.Request) {
		// Overlap is counted here, but only up to the terminal line: the
		// client stops reading at it, so anything the stub writes afterwards
		// is not work the applier is still doing, and counting it would
		// report an overlap the lock is not responsible for.
		s.mu.Lock()
		s.inFlight++
		if s.inFlight > s.peak {
			s.peak = s.inFlight
		}
		s.mu.Unlock()
		counted := true
		release := func() {
			if counted {
				counted = false
				s.mu.Lock()
				s.inFlight--
				s.mu.Unlock()
			}
		}
		defer release()

		for _, line := range s.lines {
			fmt.Fprintln(w, line)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if _, _, terminal := actor.ParseTerminal(line); terminal {
				release()
			}
			if s.pause > 0 {
				time.Sleep(s.pause)
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestApplier(t *testing.T, s *stubActor) *applier {
	t.Helper()
	srv := s.server(t)
	// A logger writing nowhere: these tests assert on behaviour, not output.
	return newApplier(&actor.Client{BaseURL: srv.URL}, 10*time.Second, zerolog.Nop())
}

func entryFor(service string) decide.Entry {
	return decide.Entry{Service: service, From: "1.0.0", To: "1.0.1", Tag: "1.0.1"}
}

// waitIdle waits for every apply to finish.
func waitIdle(t *testing.T, a *applier) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.watchers.Lock()
		n := len(a.running)
		a.watchers.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("applies did not finish")
}

func TestStartRunsTheApply(t *testing.T) {
	s := &stubActor{lines: []string{"pulling", actor.Terminal(actor.Completed, "")}}
	a := newTestApplier(t, s)

	if err := a.Start(entryFor("app")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitIdle(t, a)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applies != 1 {
		t.Errorf("applies = %d, want 1", s.applies)
	}
}

// Start returns before the work finishes: an apply can take minutes, and a
// caller holding an HTTP request open for one would time out first.
func TestStartReturnsBeforeTheWorkEnds(t *testing.T) {
	s := &stubActor{
		lines: []string{"pulling", "recreating", actor.Terminal(actor.Completed, "")},
		pause: 200 * time.Millisecond,
	}
	a := newTestApplier(t, s)

	began := time.Now()
	if err := a.Start(entryFor("app")); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 150*time.Millisecond {
		t.Errorf("Start blocked for %v; it should return once the actor accepts", took)
	}
	waitIdle(t, a)
}

// One service cannot be applied twice at once.
func TestStartRefusesASecondApplyForTheSameService(t *testing.T) {
	s := &stubActor{lines: []string{actor.Terminal(actor.Completed, "")}, pause: 300 * time.Millisecond}
	a := newTestApplier(t, s)

	if err := a.Start(entryFor("app")); err != nil {
		t.Fatal(err)
	}
	err := a.Start(entryFor("app"))
	if err == nil {
		t.Fatal("a second apply for the same service was accepted")
	}
	if !strings.Contains(err.Error(), "already") {
		t.Errorf("error = %v, want it to say why", err)
	}
	waitIdle(t, a)
}

// Applying replaces a container and rewrites a file. Two at once on one host
// is a way to lose both, so the lock serialises across services too.
func TestAppliesAreSerialisedAcrossServices(t *testing.T) {
	s := &stubActor{
		lines: []string{"working", actor.Terminal(actor.Completed, "")},
		pause: 100 * time.Millisecond,
	}
	a := newTestApplier(t, s)

	for _, name := range []string{"one", "two", "three"} {
		if err := a.Start(entryFor(name)); err != nil {
			t.Fatalf("Start(%s): %v", name, err)
		}
	}
	waitIdle(t, a)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peak > 1 {
		t.Errorf("%d applies ran at once; they must be serialised", s.peak)
	}
	if s.applies != 3 {
		t.Errorf("applies = %d, want all 3 to have run", s.applies)
	}
}

// --- watching ----------------------------------------------------------------

func TestStreamRelaysAnInFlightApply(t *testing.T) {
	s := &stubActor{
		lines: []string{"pulling", "recreating", actor.Terminal(actor.Completed, "")},
		pause: 80 * time.Millisecond,
	}
	a := newTestApplier(t, s)

	if err := a.Start(entryFor("app")); err != nil {
		t.Fatal(err)
	}
	// Let the first line land so there is something in flight to watch.
	time.Sleep(120 * time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/stream/app", nil)
	a.Stream("app", rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "pulling") {
		t.Errorf("the backlog was not replayed:\n%s", body)
	}
	if !strings.Contains(body, "status: completed") {
		t.Errorf("the stream did not carry the end:\n%s", body)
	}
	waitIdle(t, a)
}

// A watcher arriving mid-apply sees how it got there, rather than joining a
// conversation in progress.
func TestStreamReplaysTheBacklog(t *testing.T) {
	s := &stubActor{
		lines: []string{"first", "second", "third", actor.Terminal(actor.Completed, "")},
		pause: 60 * time.Millisecond,
	}
	a := newTestApplier(t, s)

	if err := a.Start(entryFor("app")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // two lines in by now

	rec := httptest.NewRecorder()
	a.Stream("app", rec, httptest.NewRequest(http.MethodGet, "/v1/stream/app", nil))

	for _, want := range []string{"first", "second"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("missing %q from the backlog:\n%s", want, rec.Body.String())
		}
	}
	waitIdle(t, a)
}

// Nothing running means nothing to say. That is the whole of this endpoint's
// error handling.
func TestStreamIsEmptyWhenNothingIsRunning(t *testing.T) {
	a := newTestApplier(t, &stubActor{})
	rec := httptest.NewRecorder()
	a.Stream("idle", rec, httptest.NewRequest(http.MethodGet, "/v1/stream/idle", nil))
	if rec.Body.Len() != 0 {
		t.Errorf("got %q, want nothing", rec.Body.String())
	}
}

// Several watchers see the same apply.
func TestSeveralWatchersEachSeeTheStream(t *testing.T) {
	s := &stubActor{
		lines: []string{"a", "b", actor.Terminal(actor.Completed, "")},
		pause: 80 * time.Millisecond,
	}
	a := newTestApplier(t, s)

	if err := a.Start(entryFor("app")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	var wg sync.WaitGroup
	bodies := make([]string, 3)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			a.Stream("app", rec, httptest.NewRequest(http.MethodGet, "/v1/stream/app", nil))
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	for i, body := range bodies {
		if !strings.Contains(body, "status: completed") {
			t.Errorf("watcher %d did not see the end:\n%s", i, body)
		}
	}
	waitIdle(t, a)
}

// A watcher leaving must not affect the apply.
func TestApplyContinuesAfterAWatcherLeaves(t *testing.T) {
	s := &stubActor{
		lines: []string{"a", "b", "c", actor.Terminal(actor.Completed, "")},
		pause: 60 * time.Millisecond,
	}
	a := newTestApplier(t, s)

	if err := a.Start(entryFor("app")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/stream/app", nil).WithContext(ctx)
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	a.Stream("app", httptest.NewRecorder(), req)

	// The apply should still finish.
	waitIdle(t, a)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applies != 1 {
		t.Errorf("applies = %d", s.applies)
	}
}

// --- line splitting ----------------------------------------------------------

func TestSplitLines(t *testing.T) {
	for in, want := range map[string][]string{
		"one\ntwo\n":     {"one", "two"},
		"one\ntwo":       {"one", "two"},
		"one\r\ntwo\r\n": {"one", "two"},
		"single":         {"single"},
		"":               nil,
	} {
		got := splitLines([]byte(in))
		if len(got) != len(want) {
			t.Errorf("splitLines(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

// A trailing newline must not produce an empty final line, which would show
// as a blank row in the UI on every write.
func TestSplitLinesDropsTheTrailingEmpty(t *testing.T) {
	if got := splitLines([]byte("done\n")); len(got) != 1 {
		t.Errorf("got %v, want one line", got)
	}
}
