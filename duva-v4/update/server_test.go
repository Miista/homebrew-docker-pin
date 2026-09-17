package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/update"
)

// applyFunc builds a Server whose transaction is whatever a test wants, so
// the surface is exercised without a daemon or a repository.
func serverWith(t *testing.T, token string, apply func(update.Request, func(string, ...any)) (update.Status, string)) *httptest.Server {
	t.Helper()
	s := &Server{Token: token, Version: "test", Apply: apply}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

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

const oneRequest = `{"service":"app","file":"/compose/docker-compose.yml",
	"image":"example.com/app","from":"1.0.0","to":"1.1.0","tag":"1.1.0"}`

func TestApplyAcceptsWorkAndNamesAStream(t *testing.T) {
	done := make(chan struct{})
	srv := serverWith(t, "", func(r update.Request, step func(string, ...any)) (update.Status, string) {
		defer close(done)
		step("working on %s", r.Service)
		return update.Completed, ""
	})

	resp := post(t, srv.URL+"/v1/apply", "", oneRequest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var acc update.Accepted
	json.NewDecoder(resp.Body).Decode(&acc)
	if acc.Stream != "/v1/stream/app" {
		t.Errorf("stream = %q", acc.Stream)
	}
	<-done
}

// The request carries what should become true; an updater that took work
// without it would be guessing.
func TestApplyRefusesAnIncompleteRequest(t *testing.T) {
	srv := serverWith(t, "", func(update.Request, func(string, ...any)) (update.Status, string) {
		t.Error("the transaction ran on an incomplete request")
		return update.Completed, ""
	})
	// A service is the whole of it now: which file declares it is this stage's
	// own lookup, since a path from the queue is rooted at the queue's mount.
	for _, body := range []string{`{}`, `{"service":""}`, `{"image":"example.com/app"}`} {
		if resp := post(t, srv.URL+"/v1/apply", "", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestApplyRefusesGarbage(t *testing.T) {
	srv := serverWith(t, "", func(update.Request, func(string, ...any)) (update.Status, string) {
		return update.Completed, ""
	})
	if resp := post(t, srv.URL+"/v1/apply", "", "not json"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestApplyRejectsGet(t *testing.T) {
	srv := serverWith(t, "", func(update.Request, func(string, ...any)) (update.Status, string) {
		t.Error("a GET ran the transaction")
		return update.Completed, ""
	})
	resp, err := http.Get(srv.URL + "/v1/apply")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// --- auth --------------------------------------------------------------------

// /v1/apply is the authority to replace a container on this host.
func TestApplyRequiresTheToken(t *testing.T) {
	ran := false
	srv := serverWith(t, "s3cret", func(update.Request, func(string, ...any)) (update.Status, string) {
		ran = true
		return update.Completed, ""
	})

	if resp := post(t, srv.URL+"/v1/apply", "", oneRequest); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("untokened = %d, want 401", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/v1/apply", "wrong", oneRequest); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", resp.StatusCode)
	}
	if ran {
		t.Fatal("the transaction ran for an unauthorised caller")
	}
	if resp := post(t, srv.URL+"/v1/apply", "s3cret", oneRequest); resp.StatusCode != http.StatusOK {
		t.Errorf("with the token = %d, want 200", resp.StatusCode)
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	srv := serverWith(t, "s3cret", nil)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// The stream returns nothing when nothing is running, so there is no session
// to guess at and nothing to leak.
func TestStreamNeedsNoToken(t *testing.T) {
	srv := serverWith(t, "s3cret", nil)
	resp, err := http.Get(srv.URL + "/v1/stream/app")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// --- one at a time -----------------------------------------------------------

// An updater that relied on its caller to serialise would be one a different
// caller could break.
func TestASecondApplyForTheSameServiceIsRefused(t *testing.T) {
	release := make(chan struct{})
	srv := serverWith(t, "", func(update.Request, func(string, ...any)) (update.Status, string) {
		<-release
		return update.Completed, ""
	})

	if resp := post(t, srv.URL+"/v1/apply", "", oneRequest); resp.StatusCode != http.StatusOK {
		t.Fatalf("the first apply was refused: %d", resp.StatusCode)
	}
	resp := post(t, srv.URL+"/v1/apply", "", oneRequest)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("the second apply = %d, want 409", resp.StatusCode)
	}
	close(release)
}

// --- the stream --------------------------------------------------------------

func TestStreamCarriesProgressAndTheTerminalLine(t *testing.T) {
	srv := serverWith(t, "", func(r update.Request, step func(string, ...any)) (update.Status, string) {
		step("pulling %s", r.Image)
		time.Sleep(30 * time.Millisecond)
		step("recreating %s", r.Service)
		return update.Completed, ""
	})

	post(t, srv.URL+"/v1/apply", "", oneRequest)

	resp, err := http.Get(srv.URL + "/v1/stream/app")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

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
	srv := serverWith(t, "", func(update.Request, func(string, ...any)) (update.Status, string) {
		return update.Failed, "the container refused the image"
	})
	post(t, srv.URL+"/v1/apply", "", oneRequest)

	resp := get(t, srv.URL+"/v1/stream/app")
	body := readAll(t, resp)

	if !strings.Contains(body, "status: failed: the container refused the image") {
		t.Errorf("the failure was not reported on the stream:\n%s", body)
	}
}

// A panic must still end the stream, or the queue waits out its whole
// timeout on work that stopped.
func TestAPanicStillEndsTheStream(t *testing.T) {
	srv := serverWith(t, "", func(update.Request, func(string, ...any)) (update.Status, string) {
		panic("something went very wrong")
	})
	post(t, srv.URL+"/v1/apply", "", oneRequest)

	resp := get(t, srv.URL+"/v1/stream/app")
	body := readAll(t, resp)

	if !strings.Contains(body, "status: failed") {
		t.Errorf("a panic left the stream without a status:\n%s", body)
	}
	if !strings.Contains(body, "panicked") {
		t.Errorf("the reason should say what happened:\n%s", body)
	}
}

func TestStreamIsEmptyWhenNothingIsRunning(t *testing.T) {
	srv := serverWith(t, "", nil)
	resp := get(t, srv.URL+"/v1/stream/idle")
	if body := readAll(t, resp); body != "" {
		t.Errorf("got %q, want nothing", body)
	}
}

// Several watchers see the same work.
func TestSeveralWatchersEachSeeTheStream(t *testing.T) {
	release := make(chan struct{})
	srv := serverWith(t, "", func(r update.Request, step func(string, ...any)) (update.Status, string) {
		step("started")
		<-release
		step("finishing")
		return update.Completed, ""
	})
	post(t, srv.URL+"/v1/apply", "", oneRequest)
	time.Sleep(20 * time.Millisecond)

	var wg sync.WaitGroup
	bodies := make([]string, 3)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/v1/stream/app")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			bodies[i] = readAllQuiet(resp)
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, body := range bodies {
		if !strings.Contains(body, "status: completed") {
			t.Errorf("watcher %d did not see the end:\n%s", i, body)
		}
		// And the backlog, so it saw how the work got there.
		if !strings.Contains(body, "started") {
			t.Errorf("watcher %d did not get the backlog:\n%s", i, body)
		}
	}
}

// get fetches a URL, failing the test rather than the next line.
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

// The per-service question: would this updater act on it now.
func TestApplicableAnswersPerService(t *testing.T) {
	s := &Server{
		Applicable: func(service string) (r update.Applicable) {
			if service == "stopped" {
				return update.Applicable{Applicable: false, Reason: "its container is not running"}
			}
			return update.Applicable{Applicable: true}
		},
	}
	h := s.Handler()

	for _, tc := range []struct {
		service string
		want    bool
	}{{"stopped", false}, {"running", true}} {
		t.Run(tc.service, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/applicable/"+tc.service, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			var got update.Applicable
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if got.Applicable != tc.want {
				t.Errorf("applicable = %v, want %v", got.Applicable, tc.want)
			}
			if !got.Applicable && got.Reason == "" {
				t.Error("refused without a reason")
			}
		})
	}
}

// An updater that cannot tell must not refuse work: nil means every service is
// applicable, the same rule Ready follows.
func TestApplicableDefaultsToYes(t *testing.T) {
	h := (&Server{}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/applicable/anything", nil))
	var got update.Applicable
	json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Applicable {
		t.Error("an updater with no check refused a service")
	}
}
