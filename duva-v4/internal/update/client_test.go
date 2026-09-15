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

// fakeUpdater is an HTTP server answering like an updater, so the client is
// tested through a real transport. What is worth catching here -- a stream
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

func clientFor(t *testing.T, f *fakeActor) *Client {
	t.Helper()
	return &Client{BaseURL: f.server(t).URL}
}

func TestApplySendsTheRequestAndReturnsTheStream(t *testing.T) {
	f := &fakeActor{}
	c := clientFor(t, f)

	acc, err := c.Apply(context.Background(), Request{
		Service: "authelia", File: "/compose/pi/docker-compose.yml",
		From: "4.39.20", To: "4.39.25", Tag: "4.39.25",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if acc.Stream != "/v1/stream/abc" {
		t.Errorf("stream = %q", acc.Stream)
	}
	if f.got.Service != "authelia" || f.got.To != "4.39.25" {
		t.Errorf("the actor received %+v", f.got)
	}
}

// An updater that accepts work but says nowhere to watch it has broken the
// contract, and pretending otherwise means a queue waiting on nothing.
func TestApplyRejectsAnEmptyStream(t *testing.T) {
	// An updater answering with an explicitly empty stream field.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"stream":""}`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}

	if _, err := c.Apply(context.Background(), Request{}); err == nil {
		t.Fatal("want an error when the actor names no stream")
	}
}

func TestApplyReportsARefusal(t *testing.T) {
	c := clientFor(t, &fakeActor{applyStatus: http.StatusConflict})
	if _, err := c.Apply(context.Background(), Request{}); err == nil {
		t.Fatal("want an error for a non-OK response")
	}
}

func TestApplyReportsAnUnreachableActor(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1"}
	if _, err := c.Apply(context.Background(), Request{}); err == nil {
		t.Fatal("want an error when the actor cannot be reached")
	}
}

// --- watching ---------------------------------------------------------------

func TestWatchReturnsTheTerminalStatus(t *testing.T) {
	f := &fakeActor{lines: []string{
		"pulling example.com/app:1.2.3",
		"recreating app",
		Terminal(Completed, ""),
	}}
	c := clientFor(t, f)

	status, reason, err := c.Watch(context.Background(), "/v1/stream/abc", nil)
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
	c := clientFor(t, f)

	status, reason, err := c.Watch(context.Background(), "/v1/stream/abc", nil)
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
	c := clientFor(t, f)

	var seen bytes.Buffer
	if _, _, err := c.Watch(context.Background(), "/v1/stream/abc", &seen); err != nil {
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
	c := clientFor(t, f)

	status, reason, err := c.Watch(context.Background(), "/v1/stream/abc", nil)
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
	c := clientFor(t, &fakeActor{})
	status, _, err := c.Watch(context.Background(), "/v1/stream/abc", nil)
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
	c := clientFor(t, &fakeActor{hang: true})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		c.Watch(ctx, "/v1/stream/abc", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch ignored its context and hung")
	}
}

func TestWatchReportsAnUnreachableStream(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1"}
	if _, _, err := c.Watch(context.Background(), "/v1/stream/abc", nil); err == nil {
		t.Fatal("want an error when the stream cannot be reached")
	}
}

// The updater may answer with a path or a full URL, and neither is wrong: a
// path is what it knows for certain, an absolute URL is what an updater behind
// something else might need to say.
func TestAbsoluteAcceptsBothForms(t *testing.T) {
	c := &Client{BaseURL: "http://actor:8080"}
	for in, want := range map[string]string{
		"/v1/stream/abc":                  "http://actor:8080/v1/stream/abc",
		"v1/stream/abc":                   "http://actor:8080/v1/stream/abc",
		"http://elsewhere/v1/stream/abc":  "http://elsewhere/v1/stream/abc",
		"https://elsewhere/v1/stream/abc": "https://elsewhere/v1/stream/abc",
	} {
		if got := c.absolute(in); got != want {
			t.Errorf("absolute(%q) = %q, want %q", in, got, want)
		}
	}
}

// A trailing slash on the base must not produce a double slash.
func TestAbsoluteToleratesATrailingSlash(t *testing.T) {
	c := &Client{BaseURL: "http://actor:8080/"}
	if got := c.absolute("/v1/stream/abc"); got != "http://actor:8080/v1/stream/abc" {
		t.Errorf("absolute = %q", got)
	}
}

func TestTokenIsSent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(Accepted{Stream: "/v1/stream/a"})
	}))
	t.Cleanup(srv.Close)

	c := &Client{BaseURL: srv.URL, Token: "s3cret"}
	if _, err := c.Apply(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if seen != "Bearer s3cret" {
		t.Errorf("authorization = %q", seen)
	}
}
