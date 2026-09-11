package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Miista/homebrew-docker-pin/internal/ui"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

type fakeSource struct {
	pending []watch.Pending
	soaking []watch.Soaking
}

func (f fakeSource) Pending() []watch.Pending { return f.pending }
func (f fakeSource) Soaking() []watch.Soaking { return f.soaking }
func (f fakeSource) LastCheck() string        { return "5 minutes ago" }
func (f fakeSource) LastCheckExact() string   { return "2026-09-11T08:00:00Z" }

type fakeApplier struct {
	started []string
	err     error
	prog    ui.Progress
	known   bool
}

func (f *fakeApplier) Start(service string) error {
	f.started = append(f.started, service)
	return f.err
}
func (f *fakeApplier) Progress(service string) (ui.Progress, bool) { return f.prog, f.known }

type fakeRefresher struct {
	calls int
	err   error
}

func (f *fakeRefresher) Refresh() error { f.calls++; return f.err }
func (f *fakeRefresher) RefreshProgress() (bool, bool, string) {
	return false, true, ""
}

func serve(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, token string) *http.Response {
	t.Helper()
	return do(t, http.MethodGet, url, token)
}

func do(t *testing.T, method, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
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

func TestSnapshotCarriesTheQueue(t *testing.T) {
	s := &Server{
		Source:  fakeSource{pending: []watch.Pending{{Service: "caddy"}}},
		Host:    "pi",
		Version: "test",
	}
	srv := serve(t, s)

	resp := get(t, srv.URL+"/v1/snapshot", "")
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Host != "pi" || snap.Version != "test" {
		t.Errorf("host/version = %q/%q", snap.Host, snap.Version)
	}
	if len(snap.Pending) != 1 || snap.Pending[0].Service != "caddy" {
		t.Errorf("pending = %+v", snap.Pending)
	}
	if snap.LastCheck != "5 minutes ago" {
		t.Errorf("last check = %q", snap.LastCheck)
	}
}

// An agent with no Applier must not have an /apply endpoint at all. Not
// present, rather than present-and-refusing: an endpoint that can trigger
// updates should not exist on an agent not meant to act.
func TestApplyEndpointAbsentWithoutApplier(t *testing.T) {
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi"})
	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/caddy", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRefreshEndpointAbsentWithoutRefresher(t *testing.T) {
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi"})
	resp := do(t, http.MethodPost, srv.URL+"/v1/refresh", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestApplyStartsTheNamedService(t *testing.T) {
	app := &fakeApplier{}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app})

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/paperless-db", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(app.started) != 1 || app.started[0] != "paperless-db" {
		t.Errorf("started = %v, want [paperless-db]", app.started)
	}
}

// GET must not apply. An update is not a safe method, and a page prefetching
// links would otherwise recreate containers.
func TestApplyRejectsGet(t *testing.T) {
	app := &fakeApplier{}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app})

	resp := get(t, srv.URL+"/v1/apply/caddy", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if len(app.started) != 0 {
		t.Errorf("a GET applied %v", app.started)
	}
}

func TestRefreshRejectsGet(t *testing.T) {
	ref := &fakeRefresher{}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Refresher: ref})

	resp := get(t, srv.URL+"/v1/refresh", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if ref.calls != 0 {
		t.Errorf("a GET refreshed")
	}
}

// A refusal is an answer about the queue, not a transport failure: 200 with
// the reason in the body, so the hub renders it rather than reporting the
// agent as broken.
func TestRefusalIsTwoHundredWithReason(t *testing.T) {
	app := &fakeApplier{err: errString("already running")}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app})

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/caddy", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out StartResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Error != "already running" {
		t.Errorf("error = %q", out.Error)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// The token is what separates "the hub asked" from "anything on the LAN
// asked". /apply is root on this host, so an unauthenticated call must be
// refused outright.
func TestTokenRequiredWhenSet(t *testing.T) {
	app := &fakeApplier{}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app, Token: "s3cret"})

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/caddy", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if len(app.started) != 0 {
		t.Fatalf("an unauthenticated request applied %v", app.started)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	app := &fakeApplier{}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app, Token: "s3cret"})

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/caddy", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if len(app.started) != 0 {
		t.Fatalf("a wrong token applied %v", app.started)
	}
}

func TestRightTokenAccepted(t *testing.T) {
	app := &fakeApplier{}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app, Token: "s3cret"})

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/caddy", "s3cret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(app.started) != 1 {
		t.Errorf("started = %v", app.started)
	}
}

// Every endpoint that does or reveals anything is behind the token, not just
// /apply: the queue names what runs on the host and what is out of date.
func TestSnapshotRequiresToken(t *testing.T) {
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Token: "s3cret"})
	if resp := get(t, srv.URL+"/v1/snapshot", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/v1/snapshot", "s3cret"); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d with the token, want 200", resp.StatusCode)
	}
}

// Health is deliberately open: the thing most likely to call it is a
// container runtime, which carries no credential.
func TestHealthNeedsNoToken(t *testing.T) {
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Token: "s3cret"})
	if resp := get(t, srv.URL+"/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestNoTokenConfiguredMeansOpen(t *testing.T) {
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi"})
	if resp := get(t, srv.URL+"/v1/snapshot", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestProgressReportsKnown(t *testing.T) {
	app := &fakeApplier{known: true, prog: ui.Progress{Done: true, Message: "updated"}}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app})

	resp := get(t, srv.URL+"/v1/progress/caddy", "")
	var out ProgressResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if !out.Known || out.Progress.Message != "updated" {
		t.Errorf("got %+v", out)
	}
}

func TestProgressReportsUnknown(t *testing.T) {
	app := &fakeApplier{known: false}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app})

	resp := get(t, srv.URL+"/v1/progress/never-asked", "")
	var out ProgressResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Known {
		t.Error("want known=false for a service never started")
	}
}

// A service name with a slash cannot reach the handler as one path segment;
// compose forbids it, but the agent must not silently act on a truncated
// name if something sends one.
func TestApplyWithEmptyServiceIsRejected(t *testing.T) {
	app := &fakeApplier{}
	srv := serve(t, &Server{Source: fakeSource{}, Host: "pi", Applier: app})

	resp := do(t, http.MethodPost, srv.URL+"/v1/apply/", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if len(app.started) != 0 {
		t.Errorf("started %v for an empty name", app.started)
	}
}
