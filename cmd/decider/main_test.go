package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/internal/decide"
)

// --- config ------------------------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	t.Setenv("DECIDER_HOST", "")
	t.Setenv("DECIDER_APPLY_TIMEOUT", "")
	c := loadConfig()
	if c.ApplyTimeout != defaultApplyTimeout {
		t.Errorf("timeout = %v, want the default", c.ApplyTimeout)
	}
	// Unset must still name the host as something, or a UI over several
	// deciders cannot tell them apart.
	if c.Host == "" {
		t.Error("host is empty; it should fall back to the OS hostname")
	}
}

func TestConfigReadsTheEnvironment(t *testing.T) {
	t.Setenv("DECIDER_HOST", "pi")
	t.Setenv("DECIDER_TOKEN", "s3cret")
	t.Setenv("DECIDER_ACTOR_URL", "http://actor:8080")
	t.Setenv("DECIDER_ACTOR_TOKEN", "atoken")
	t.Setenv("DECIDER_APPLY_TIMEOUT", "5m")

	c := loadConfig()
	if c.Host != "pi" || c.Token != "s3cret" {
		t.Errorf("host/token = %q/%q", c.Host, c.Token)
	}
	if c.ActorURL != "http://actor:8080" || c.ActorToken != "atoken" {
		t.Errorf("actor = %q/%q", c.ActorURL, c.ActorToken)
	}
	if c.ApplyTimeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", c.ApplyTimeout)
	}
}

// --- the project path --------------------------------------------------------

func TestProjectFileFindsTheComposeFile(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(want, []byte("services: {}\n"), 0o644)

	old := composeDir
	composeDir = dir
	t.Cleanup(func() { composeDir = old })

	got, err := projectFile("")
	if err != nil {
		t.Fatalf("projectFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestProjectFileHonoursASubdirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pi")
	os.MkdirAll(sub, 0o755)
	want := filepath.Join(sub, "docker-compose.yml")
	os.WriteFile(want, []byte("services: {}\n"), 0o644)

	old := composeDir
	composeDir = dir
	t.Cleanup(func() { composeDir = old })

	got, err := projectFile("pi")
	if err != nil {
		t.Fatalf("projectFile: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A subdirectory that climbs out must be refused rather than silently
// resolving somewhere else -- filepath.Join cleans ".." away without
// complaining, which is exactly how it would go unnoticed.
func TestProjectFileRefusesToEscape(t *testing.T) {
	old := composeDir
	composeDir = t.TempDir()
	t.Cleanup(func() { composeDir = old })

	for _, sub := range []string{"../etc", "..", "/absolute"} {
		if _, err := projectFile(sub); err == nil {
			t.Errorf("projectFile(%q) was accepted", sub)
		}
	}
}

// --- the notify wiring -------------------------------------------------------

func projectWith(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

const oneService = `
services:
  app:
    image: example.com/app:1.0.0@sha256:old
    container_name: my-app
`

func TestNotifyQueuesAnUpdate(t *testing.T) {
	q := decide.NewPending()
	h := &decide.Handler{
		Lookup: decide.Lookup{Root: projectWith(t, oneService)},
		Queue:  q,
	}
	notify := notifyHandler(h, zerolog.Nop())

	status, _ := notify([]byte(`{
		"container":"my-app","image":"example.com/app","tag":"1.1.0"}`))

	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if q.Len() != 1 {
		t.Fatalf("nothing was queued")
	}
	e, _ := q.Get("app")
	if e.To != "1.1.0" {
		t.Errorf("entry = %+v", e)
	}
}

// A detector that only watches which tags exist has no reason to resolve
// digests. An event naming the tag already pinned therefore says nothing --
// and must not be read as a move to nowhere.
func TestNotifyIgnoresTheTagAlreadyFollowed(t *testing.T) {
	q := decide.NewPending()
	notify := notifyHandler(&decide.Handler{
		Lookup: decide.Lookup{Root: projectWith(t, oneService)},
		Queue:  q,
	}, zerolog.Nop())

	status, message := notify([]byte(`{
		"container":"my-app","image":"example.com/app","tag":"1.0.0"}`))

	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if q.Len() != 0 {
		t.Errorf("the tag already followed was queued: %s", message)
	}
}

func TestNotifyRejectsAnUnreadableBody(t *testing.T) {
	notify := notifyHandler(&decide.Handler{
		Lookup: decide.Lookup{Root: projectWith(t, oneService)},
		Queue:  decide.NewPending(),
	}, zerolog.Nop())
	status, _ := notify([]byte("not json"))
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// A notice naming a container nothing declares is reported, not dropped.
func TestNotifyRejectsAnUnknownContainer(t *testing.T) {
	notify := notifyHandler(&decide.Handler{
		Lookup: decide.Lookup{Root: projectWith(t, oneService)},
		Queue:  decide.NewPending(),
	}, zerolog.Nop())
	status, message := notify([]byte(`{
		"container":"ghost","image":"x","tag":"1"}`))

	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", status)
	}
	if !strings.Contains(message, "ghost") {
		t.Errorf("message = %q, want it to name the container", message)
	}
}

// End to end through the HTTP surface, which is how diun will actually reach
// it: an unauthenticated POST that ends up as a queued row.
func TestNotifyOverHTTP(t *testing.T) {
	q := decide.NewPending()
	h := &decide.Handler{
		Lookup: decide.Lookup{Root: projectWith(t, oneService)},
		Queue:  q,
	}
	srv := &decide.Server{
		Queue:  q,
		Host:   "testhost",
		Token:  "s3cret",
		Notify: notifyHandler(h, zerolog.Nop()),
	}

	rec := doRequest(t, srv.Handler(), http.MethodPost, "/v1/notify", "", `{
		"container":"my-app","image":"example.com/app","tag":"1.1.0"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if q.Len() != 1 {
		t.Error("the notification did not reach the queue")
	}

	// And the queue is readable through the UI's endpoint, with the token.
	snapRec := doRequest(t, srv.Handler(), http.MethodGet, "/v1/snapshot", "s3cret", "")
	var snap decide.Snapshot
	json.NewDecoder(snapRec.Body).Decode(&snap)
	if len(snap.Pending) != 1 || snap.Pending[0].Service != "app" {
		t.Errorf("snapshot = %+v", snap.Pending)
	}
}

// doRequest runs one request against a handler.
func doRequest(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
