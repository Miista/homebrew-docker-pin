package queue

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeApplier struct {
	started []Entry
	err     error
	lines   []string
	// ready and reason make this a ReadinessReporter. Unset means ready,
	// which is what an Applier that cannot answer is treated as.
	ready  *bool
	reason string
}

func (f *fakeApplier) Start(e Entry) error {
	if f.err != nil {
		return f.err
	}
	f.started = append(f.started, e)
	return nil
}

func (f *fakeApplier) Stream(service string, w http.ResponseWriter, r *http.Request) {
	for _, line := range f.lines {
		fmt.Fprintln(w, line)
	}
}

func serverWith(t *testing.T, q *Pending, app Applier, token string) *httptest.Server {
	t.Helper()
	s := &Server{
		Queue:   q,
		Applier: app,
		Host:    "testhost",
		Version: "test",
		Token:   token,
		Notify: func(body []byte) (int, string) {
			return http.StatusOK, "noted"
		},
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, token string, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body != "" {
		r, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		r, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestSnapshotCarriesTheQueue(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	srv := serverWith(t, q, &fakeApplier{}, "")

	resp := do(t, http.MethodGet, srv.URL+"/v1/snapshot", "", "")
	var snap Snapshot
	json.NewDecoder(resp.Body).Decode(&snap)

	if snap.Host != "testhost" || snap.Version != "test" {
		t.Errorf("host/version = %q/%q", snap.Host, snap.Version)
	}
	if len(snap.Pending) != 1 || snap.Pending[0].Service != "app" {
		t.Errorf("pending = %+v", snap.Pending)
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	srv := serverWith(t, NewPending(), &fakeApplier{}, "s3cret")
	if resp := do(t, http.MethodGet, srv.URL+"/healthz", "", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// A watcher reaches this over the host's own network and carries no
// credential, so it is open by design.
func TestNotifyNeedsNoToken(t *testing.T) {
	srv := serverWith(t, NewPending(), &fakeApplier{}, "s3cret")
	resp := do(t, http.MethodPost, srv.URL+"/v1/notify", "", `{"status":"update"}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestNotifyRejectsGet(t *testing.T) {
	srv := serverWith(t, NewPending(), &fakeApplier{}, "")
	resp := do(t, http.MethodGet, srv.URL+"/v1/notify", "", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// The UI's endpoints are the ones reached from off-host, so they hold the
// token.
func TestUIEndpointsRequireTheToken(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	srv := serverWith(t, q, &fakeApplier{}, "s3cret")

	for _, path := range []string{"/v1/snapshot", "/v1/stream/app"} {
		if resp := do(t, http.MethodGet, srv.URL+path, "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, resp.StatusCode)
		}
		if resp := do(t, http.MethodGet, srv.URL+path, "s3cret", ""); resp.StatusCode != http.StatusOK {
			t.Errorf("%s with the token = %d, want 200", path, resp.StatusCode)
		}
	}
	if resp := do(t, http.MethodPost, srv.URL+"/v1/apply/app", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("apply without a token = %d, want 401", resp.StatusCode)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	srv := serverWith(t, NewPending(), &fakeApplier{}, "s3cret")
	if resp := do(t, http.MethodGet, srv.URL+"/v1/snapshot", "wrong", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestNoTokenConfiguredMeansOpen(t *testing.T) {
	srv := serverWith(t, NewPending(), &fakeApplier{}, "")
	if resp := do(t, http.MethodGet, srv.URL+"/v1/snapshot", "", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// --- approving ---------------------------------------------------------------

func TestApplyStartsTheQueuedEntry(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	app := &fakeApplier{}
	srv := serverWith(t, q, app, "")

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/app", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(app.started) != 1 || app.started[0].Service != "app" {
		t.Errorf("started = %+v", app.started)
	}
}

// Approving something that is not queued is an answer about the queue, not a
// transport failure -- 200 with a message, so a UI renders it to a person.
func TestApplyingSomethingNotQueuedIsAMessageNotAnError(t *testing.T) {
	app := &fakeApplier{}
	srv := serverWith(t, NewPending(), app, "")

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/ghost", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]string
	json.NewDecoder(resp.Body).Decode(&got)
	if !strings.Contains(got["message"], "ghost") {
		t.Errorf("message = %q, want it to name the service", got["message"])
	}
	if len(app.started) != 0 {
		t.Error("it started something anyway")
	}
}

func TestApplierRefusalSurfacesAsAMessage(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	srv := serverWith(t, q, &fakeApplier{err: errString("already running")}, "")

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/app", "", "")
	var got map[string]string
	json.NewDecoder(resp.Body).Decode(&got)
	if got["message"] != "already running" {
		t.Errorf("message = %q", got["message"])
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A GET must not apply. An update is not a safe method, and a page
// prefetching links would otherwise recreate containers.
func TestApplyRejectsGet(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	app := &fakeApplier{}
	srv := serverWith(t, q, app, "")

	resp := do(t, http.MethodGet, srv.URL+"/v1/apply/app", "", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if len(app.started) != 0 {
		t.Error("a GET started an apply")
	}
}

func TestApplyWithNoServiceIsRejected(t *testing.T) {
	srv := serverWith(t, NewPending(), &fakeApplier{}, "")
	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/", "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// --- streaming ---------------------------------------------------------------

// The queue is a pipe: what the updater said is copied through unchanged, so
// the updater keeps owning the shape of its own progress.
func TestStreamRelaysVerbatim(t *testing.T) {
	app := &fakeApplier{lines: []string{
		"pulling example.com/app:1.1.0",
		"recreating app",
		"status: completed",
	}}
	srv := serverWith(t, NewPending(), app, "")

	resp := do(t, http.MethodGet, srv.URL+"/v1/stream/app", "", "")
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	for _, want := range []string{"pulling", "recreating app", "status: completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("the stream is missing %q:\n%s", want, body)
		}
	}
}

// Nothing in progress means nothing to say, which is the whole of the
// stream's error handling.
func TestStreamIsEmptyWhenNothingIsRunning(t *testing.T) {
	srv := serverWith(t, NewPending(), &fakeApplier{}, "")
	resp := do(t, http.MethodGet, srv.URL+"/v1/stream/app", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if n != 0 {
		t.Errorf("got %q, want nothing", buf[:n])
	}
}

func TestStreamWithNoServiceIsRejected(t *testing.T) {
	srv := serverWith(t, NewPending(), &fakeApplier{}, "")
	resp := do(t, http.MethodGet, srv.URL+"/v1/stream/", "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// An approved entry is no longer waiting, so it leaves the queue. Leaving it
// there would offer a person the chance to approve the same update twice.
func TestApprovingRemovesItFromTheQueue(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	srv := serverWith(t, q, &fakeApplier{}, "")

	do(t, http.MethodPost, srv.URL+"/v1/apply/app", "", "")

	if q.Len() != 0 {
		t.Errorf("the approved entry is still queued: %+v", q.List())
	}
}

// A refused start leaves it queued: failing to start is not a decision, and
// the entry still needs one.
func TestARefusedStartLeavesItQueued(t *testing.T) {
	q := NewPending()
	q.Put(entry("app", "1.0.0", "1.1.0"), now)
	srv := serverWith(t, q, &fakeApplier{err: errString("busy")}, "")

	do(t, http.MethodPost, srv.URL+"/v1/apply/app", "", "")

	if q.Len() != 1 {
		t.Error("a refused start dropped the entry")
	}
}

// Ready makes fakeApplier a ReadinessReporter. A nil `ready` means the fake
// does not implement readiness meaningfully and reports ready, matching what
// the queue assumes of an Applier that cannot answer.
func (f *fakeApplier) Ready() (bool, string) {
	if f.ready == nil {
		return true, ""
	}
	return *f.ready, f.reason
}
