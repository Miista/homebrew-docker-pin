package update

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeActor is an HTTP server streaming like a self-update helper, so
// WatchURL is tested through a real transport. What is worth catching here -- a stream
// that ends early, a terminal line that never comes -- only exists on a wire.
type fakeActor struct {
	// lines are streamed in order, then the connection closes.
	lines []string
	// applyStatus overrides the response to /v1/apply.
	applyStatus int
	// stream overrides what /v1/apply says to watch.
	stream string
	// got records the request the updater was given.
	got Request
	// hang keeps the stream open without writing, for timeout tests.
	hang bool
}

func (f *fakeActor) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/apply", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&f.got)
		if f.applyStatus != 0 {
			w.WriteHeader(f.applyStatus)
			w.Write([]byte("no"))
			return
		}
		stream := f.stream
		if stream == "" {
			stream = "/v1/stream/abc"
		}
		json.NewEncoder(w).Encode(Accepted{Stream: stream})
	})
	mux.HandleFunc("/v1/stream/", func(w http.ResponseWriter, r *http.Request) {
		if f.hang {
			<-r.Context().Done()
			return
		}
		for _, line := range f.lines {
			w.Write([]byte(line + "\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// streamFor serves f and returns the URL of its stream.
func streamFor(t *testing.T, f *fakeActor) string {
	t.Helper()
	return f.server(t).URL + "/v1/stream/abc"
}





// --- watching ---------------------------------------------------------------

func TestWatchReturnsTheTerminalStatus(t *testing.T) {
	f := &fakeActor{lines: []string{
		"pulling example.com/app:1.2.3",
		"recreating app",
		Terminal(Completed, ""),
	}}
	url := streamFor(t, f)

	status, reason, err := WatchURL(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if status != Completed || reason != "" {
		t.Errorf("got %q/%q, want completed", status, reason)
	}
}

func TestWatchReturnsAFailureAndItsReason(t *testing.T) {
	f := &fakeActor{lines: []string{
		"pulling",
		Terminal(Failed, "the container refused the image"),
	}}
	url := streamFor(t, f)

	status, reason, err := WatchURL(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if status != Failed {
		t.Errorf("status = %q, want failed", status)
	}
	if reason != "the container refused the image" {
		t.Errorf("reason = %q", reason)
	}
}

// The tee is the whole reason one connection serves two readers. Everything
// the updater said must reach the watcher, terminal line included -- a person
// should see how it ended, not have the last line swallowed by the thing that
// was looking for it.
func TestWatchTeesEveryLine(t *testing.T) {
	f := &fakeActor{lines: []string{
		"pulling example.com/app:1.2.3",
		"recreating app",
		Terminal(Completed, ""),
	}}
	url := streamFor(t, f)

	var seen bytes.Buffer
	if _, _, err := WatchURL(context.Background(), url, &seen); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	for _, want := range []string{"pulling", "recreating app", "status: completed"} {
		if !strings.Contains(seen.String(), want) {
			t.Errorf("the tee is missing %q:\n%s", want, seen.String())
		}
	}
}

// A stream that stops without saying how it went is a failure, whatever
// caused it. Silence is the one thing that must never read as success.
func TestWatchTreatsSilenceAsFailure(t *testing.T) {
	f := &fakeActor{lines: []string{"pulling", "recreating"}} // no terminal line
	url := streamFor(t, f)

	status, reason, err := WatchURL(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if status != Failed {
		t.Errorf("status = %q, want failed for a stream with no status", status)
	}
	if !strings.Contains(reason, "without a status") {
		t.Errorf("reason = %q, want it to say what was wrong", reason)
	}
}

// An empty stream is the same case, and is what the updater serves when nothing
// is running.
func TestWatchTreatsAnEmptyStreamAsFailure(t *testing.T) {
	url := streamFor(t, &fakeActor{})
	status, _, err := WatchURL(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if status != Failed {
		t.Errorf("status = %q, want failed", status)
	}
}

// The timeout is the queue's, expressed as a context -- it is what keeps an
// updater that died mid-apply from wedging the queue forever.
func TestWatchRespectsTheContext(t *testing.T) {
	url := streamFor(t, &fakeActor{hang: true})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		WatchURL(ctx, url, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch ignored its context and hung")
	}
}

func TestWatchReportsAnUnreachableStream(t *testing.T) {
	if _, _, err := WatchURL(context.Background(), "http://127.0.0.1:1/v1/stream/abc", nil); err == nil {
		t.Fatal("want an error when the stream cannot be reached")
	}
}



