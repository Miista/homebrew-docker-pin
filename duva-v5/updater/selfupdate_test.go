package updater

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/update"
)

const selfRequest = `{"service":"duva-update","image":"ghcr.io/miista/duva-update",
	"from":"v7.8","to":"v7.9","tag":"v7.9"}`

// selfServer is an updater whose own service is duva-update, with a hand-off
// the test controls: exited is closed to make the helper exit.
func selfServer(t *testing.T, apply work, handOffErr error) (s *Server, exited chan error, handed *int) {
	t.Helper()
	exited = make(chan error, 1)
	handed = new(int)
	s = &Server{
		Version: "test",
		Apply:   apply,
		IsSelf:  func(service string) bool { return service == "duva-update" },
		HandOff: func(update.Request) (Handoff, error) {
			*handed++
			if handOffErr != nil {
				return Handoff{}, handOffErr
			}
			return Handoff{
				Name:   "duva-update-self-update-1",
				Stream: "http://duva-update-self-update-1:8080/v1/stream/duva-update",
				Wait:   func() error { return <-exited },
			}, nil
		},
	}
	return s, exited, handed
}

// The queue follows the stream it is given, so handing over is answering with
// the helper's.
func TestUpdatingItselfAnswersWithTheHelpersStream(t *testing.T) {
	srv, _, handed := selfServer(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		t.Error("the updater ran its own update itself")
		return update.Completed, ""
	}, nil)

	acc := start(t, srv, selfRequest)

	if acc.Stream != "http://duva-update-self-update-1:8080/v1/stream/duva-update" {
		t.Errorf("stream = %q, want the helper's", acc.Stream)
	}
	if *handed != 1 {
		t.Errorf("handed off %d times", *handed)
	}
}

// Not ready while the helper lives: the repository is clean for most of a
// self-update, and a click in that window would start work the helper's
// recreate then kills.
func TestNotReadyWhileTheHelperLives(t *testing.T) {
	srv, exited, _ := selfServer(t, nil, nil)
	start(t, srv, selfRequest)

	r := srv.Readiness()
	if r.Ready {
		t.Fatal("ready while a helper is replacing it")
	}
	if !strings.Contains(r.Reason, "duva-update-self-update-1") {
		t.Errorf("reason = %q, want it to name the helper", r.Reason)
	}

	// The helper exited and this process is still here: it failed before
	// replacing anything, and this updater is the one running.
	exited <- nil
	deadline := time.Now().Add(time.Second)
	for !srv.Readiness().Ready {
		if time.Now().After(deadline) {
			t.Fatal("still not ready after the helper exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Work offered while handed off is deferred, not run: the helper is about to
// stop this process, and work in it would be killed partway.
func TestWorkWhileHandedOffIsDeferred(t *testing.T) {
	srv, _, _ := selfServer(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		t.Error("work ran on an updater that had handed off")
		return update.Completed, ""
	}, nil)
	start(t, srv, selfRequest)

	start(t, srv, oneRequest)
	body := streamOf(t, srv, "app")
	if !strings.Contains(body, "status: deferred") || !strings.Contains(body, "duva-update-self-update-1") {
		t.Errorf("want a deferral naming the helper:\n%s", body)
	}
}

// Losing track of the helper is not a reason to guess it is gone.
func TestALostHelperKeepsItNotReady(t *testing.T) {
	srv, exited, _ := selfServer(t, nil, nil)
	start(t, srv, selfRequest)

	exited <- errors.New("the daemon hung up")
	time.Sleep(50 * time.Millisecond)
	if srv.Readiness().Ready {
		t.Error("ready again without knowing whether the helper is still running")
	}
}

// The helper stops this container, and with it whatever it is applying.
func TestItDoesNotHandOffWhileApplyingSomethingElse(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv, _, handed := selfServer(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		<-release
		return update.Completed, ""
	}, nil)

	start(t, srv, oneRequest)
	start(t, srv, selfRequest)

	body := streamOf(t, srv, "duva-update")
	if !strings.Contains(body, "status: deferred") || !strings.Contains(body, "app") {
		t.Errorf("want a deferral naming what it is busy with:\n%s", body)
	}
	if *handed != 0 {
		t.Error("it handed off while applying something else")
	}
	if !srv.Readiness().Ready {
		t.Error("a deferred self-update left it not ready")
	}
}

// A helper that would not start is a failure of this request, and nothing has
// been handed to anyone.
func TestAHelperThatWillNotStartFails(t *testing.T) {
	srv, _, _ := selfServer(t, nil, errors.New("no such image"))

	acc := start(t, srv, selfRequest)
	if acc.Stream != "/v1/stream/duva-update" {
		t.Fatalf("stream = %q, want its own", acc.Stream)
	}
	body := streamOf(t, srv, strings.TrimPrefix(acc.Stream, "/v1/stream/"))
	if !strings.Contains(body, "status: failed") || !strings.Contains(body, "no such image") {
		t.Errorf("want the failure on the stream:\n%s", body)
	}
	if !srv.Readiness().Ready {
		t.Error("a hand-off that never happened left it not ready")
	}
}

// --- the helper --------------------------------------------------------------

func helperUnder(t *testing.T, apply work) (*helper, *httptest.Server) {
	t.Helper()
	var req update.Request
	json.Unmarshal([]byte(selfRequest), &req)
	h := newHelper(req, apply, zerolog.Nop())
	h.Idle, h.Drain = 200*time.Millisecond, time.Second
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return h, srv
}

// A job nobody watches is one whose outcome nobody learns, so it is not done.
func TestAHelperNobodyWatchesDoesNothing(t *testing.T) {
	h, _ := helperUnder(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		t.Error("the job ran with nobody watching")
		return update.Completed, ""
	})
	if _, ran := h.Run(); ran {
		t.Error("Run reported doing the job")
	}
}

// The job starts when the queue watches, and the queue sees it to the end.
func TestAHelperRunsItsJobForItsWatcher(t *testing.T) {
	h, srv := helperUnder(t, func(r update.Request, step func(string, ...any)) (update.Status, string) {
		step("recreating %s", r.Service)
		return update.Completed, ""
	})

	got := make(chan string)
	go func() {
		resp, err := http.Get(srv.URL + "/v1/stream/duva-update")
		if err != nil {
			got <- err.Error()
			return
		}
		defer resp.Body.Close()
		got <- readAllQuiet(resp)
	}()

	status, ran := h.Run()
	if !ran || status != update.Completed {
		t.Errorf("Run = %q, %v", status, ran)
	}
	body := <-got
	for _, want := range []string{"recreating duva-update", "status: completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("the watcher missed %q:\n%s", want, body)
		}
	}
}

// One stream, for the one job. There is no /v1/apply to give it another.
func TestAHelperServesOnlyItsOwnJob(t *testing.T) {
	_, srv := helperUnder(t, nil)

	if resp := get(t, srv.URL+"/v1/stream/app"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another service's stream = %d, want 404", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/v1/apply", "", oneRequest); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/v1/apply = %d, want 404", resp.StatusCode)
	}
}

// A panic is a failure on the stream, not a helper that dies silently.
func TestAHelperThatPanicsReportsIt(t *testing.T) {
	h, srv := helperUnder(t, func(update.Request, func(string, ...any)) (update.Status, string) {
		panic("boom")
	})
	go func() {
		if resp, err := http.Get(srv.URL + "/v1/stream/duva-update"); err == nil {
			readAllQuiet(resp)
			resp.Body.Close()
		}
	}()

	if status, _ := h.Run(); status != update.Failed {
		t.Errorf("status = %q, want failed", status)
	}
}

// Refused before anything is looked at: a helper is for the updater that
// started it, and nothing else.
func TestSelfUpdateRefusesAMalformedJob(t *testing.T) {
	log := zerolog.Nop()
	for _, args := range [][]string{nil, {"abc"}, {"abc", "not json"}} {
		if err := selfUpdate(log, args); err == nil {
			t.Errorf("selfUpdate(%q) ran", args)
		}
	}
}
