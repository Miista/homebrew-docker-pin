package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/update"
)

// serverWith builds a Server whose transaction is whatever a test wants, so
// the updater is exercised without a daemon or a repository -- through Start
// and Local, the way the queue in the same process reaches it.
func serverWith(t *testing.T, apply work) *Server {
	t.Helper()
	return &Server{Version: "test", Log: zerolog.Nop(), Apply: apply}
}

const oneRequest = `{"service":"app","image":"example.com/app","from":"1.0.0","to":"1.1.0","tag":"1.1.0"}`

// start offers a request, as JSON so the requests read as they always have.
func start(t *testing.T, s *Server, body string) update.Accepted {
	t.Helper()
	var req update.Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	acc, err := s.Start(req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return acc
}

// streamOf follows a service's stream to its end and returns everything said.
// Empty when nothing is running for it.
func streamOf(t *testing.T, s *Server, service string) string {
	t.Helper()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := s.Local().Watch(ctx, "/v1/stream/"+service, &out); err != nil {
		return ""
	}
	return out.String()
}

func TestApplyAcceptsWorkAndNamesAStream(t *testing.T) {
	done := make(chan struct{})
	s := serverWith(t, func(r update.Request, step func(string, ...any)) (update.Status, string) {
		defer close(done)
		step("working on %s", r.Service)
		return update.Completed, ""
	})

	if acc := start(t, s, oneRequest); acc.Stream != "/v1/stream/app" {
		t.Errorf("stream = %q", acc.Stream)
	}
	<-done
}

// An updater that relied on its caller to serialise would be one a different
// caller could break.
func TestASecondApplyForTheSameServiceIsRefused(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := serverWith(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		<-release
		return update.Completed, ""
	})

	start(t, s, oneRequest)
	var req update.Request
	json.Unmarshal([]byte(oneRequest), &req)
	if _, err := s.Start(req); err == nil {
		t.Error("a second apply for the same service was started")
	}
}

// --- the stream --------------------------------------------------------------

func TestStreamCarriesProgressAndTheTerminalLine(t *testing.T) {
	s := serverWith(t, func(r update.Request, step func(string, ...any)) (update.Status, string) {
		step("pulling %s", r.Image)
		time.Sleep(30 * time.Millisecond)
		step("recreating %s", r.Service)
		return update.Completed, ""
	})
	start(t, s, oneRequest)

	body := streamOf(t, s, "app")
	for _, want := range []string{"pulling example.com/app", "recreating app", "status: completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("the stream is missing %q:\n%s", want, body)
		}
	}
}

// The terminal line is the last thing said, always: a queue reading it is
// how the work is known to have ended, and an updater that finished without
// saying so is indistinguishable from one that died.
func TestAFailureStillEndsWithAStatus(t *testing.T) {
	s := serverWith(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		return update.Failed, "the container refused the image"
	})
	start(t, s, oneRequest)

	status, reason, err := s.Local().Watch(context.Background(), "/v1/stream/app", nil)
	if err != nil || status != update.Failed || reason != "the container refused the image" {
		t.Errorf("Watch = %s %q %v", status, reason, err)
	}
}

// A panic must still end the stream, or the queue waits out its whole
// timeout on work that stopped.
func TestAPanicStillEndsTheStream(t *testing.T) {
	s := serverWith(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		panic("something went very wrong")
	})
	start(t, s, oneRequest)

	body := streamOf(t, s, "app")
	if !strings.Contains(body, "status: failed") {
		t.Errorf("a panic left the stream without a status:\n%s", body)
	}
	if !strings.Contains(body, "panicked") {
		t.Errorf("the reason should say what happened:\n%s", body)
	}
}

func TestWatchingNothingRunningIsAnError(t *testing.T) {
	s := serverWith(t, nil)
	if _, _, err := s.Local().Watch(context.Background(), "/v1/stream/idle", nil); err == nil {
		t.Error("watched a service with nothing running")
	}
}

// Several watchers see the same work, each with the backlog.
func TestSeveralWatchersEachSeeTheStream(t *testing.T) {
	release := make(chan struct{})
	s := serverWith(t, func(r update.Request, step func(string, ...any)) (update.Status, string) {
		step("started")
		<-release
		step("finishing")
		return update.Completed, ""
	})
	start(t, s, oneRequest)
	time.Sleep(20 * time.Millisecond)

	var wg sync.WaitGroup
	bodies := make([]string, 3)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bodies[i] = streamOf(t, s, "app")
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, body := range bodies {
		if !strings.Contains(body, "status: completed") {
			t.Errorf("watcher %d did not see the end:\n%s", i, body)
		}
		if !strings.Contains(body, "started") {
			t.Errorf("watcher %d did not get the backlog:\n%s", i, body)
		}
	}
}

// A watcher arriving after the end still gets the end: it is read from the
// stream's record, not from lines it was there to receive.
func TestWatchingAFinishedApply(t *testing.T) {
	s := serverWith(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		return update.Completed, ""
	})
	start(t, s, oneRequest)

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
	if body := streamOf(t, s, "app"); !strings.Contains(body, "status: completed") {
		t.Errorf("a late watcher missed the end:\n%s", body)
	}
}

// --- what the queue asks before applying -------------------------------------

func TestApplicableAnswersPerService(t *testing.T) {
	s := &Server{
		Applicable: func(service string) update.Applicable {
			if service == "stopped" {
				return update.Applicable{Applicable: false, Reason: "its container is not running"}
			}
			return update.Applicable{Applicable: true}
		},
	}
	u := s.Local()
	if got := u.Applicable(context.Background(), "running"); !got.Applicable {
		t.Error("a running service was refused")
	}
	got := u.Applicable(context.Background(), "stopped")
	if got.Applicable || got.Reason == "" {
		t.Errorf("stopped = %+v, want refused with a reason", got)
	}
}

// An updater that cannot tell must not refuse work: nil means every service is
// applicable, the same rule Ready follows.
func TestApplicableDefaultsToYes(t *testing.T) {
	if !(&Server{}).Local().Applicable(context.Background(), "anything").Applicable {
		t.Error("an updater with no check refused a service")
	}
}

func TestReadinessIsTheServers(t *testing.T) {
	s := &Server{Ready: func() update.Readiness { return update.Readiness{Ready: false, Reason: "dirty"} }}
	if r := s.Local().Ready(context.Background()); r.Ready || r.Reason != "dirty" {
		t.Errorf("Ready = %+v", r)
	}
}

// --- HTTP, for the self-update helper's own surface --------------------------

func post(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readAllQuiet(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
