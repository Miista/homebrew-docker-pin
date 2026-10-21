package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestActionsReachALocalQueueWithWho(t *testing.T) {
	local := &localQueue{}
	c := NewCollector([]Upstream{{Host: "optiplex", Local: local}})

	c.Discard("optiplex", 7, "soren")
	c.Block("optiplex", 8, "missing migrations", "soren")
	c.Check("optiplex", "app", "soren")
	c.Rescan("optiplex", "db", "soren")

	want := []string{"discard 7 by soren", "block 8 (missing migrations) by soren", "check app by soren", "rescan db by soren"}
	if strings.Join(local.acted, "|") != strings.Join(want, "|") {
		t.Errorf("acted = %v\nwant    %v", local.acted, want)
	}
}

// Another host's queue is reached over its API: the token it trusts, and
// whom the action is for.
func TestActionsReachARemoteQueueOverItsAPI(t *testing.T) {
	type call struct{ path, token, actor, reason string }
	var got []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = append(got, call{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Duva-Actor"), r.FormValue("reason")})
		json.NewEncoder(w).Encode(map[string]string{"message": ""})
	}))
	defer srv.Close()
	c := NewCollector([]Upstream{{Host: "pi", URL: srv.URL, Token: "t0k"}})

	if err := c.Block("pi", 3, "bad build", "soren"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rescan("pi", "app", "soren"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("calls = %+v", got)
	}
	if got[0] != (call{"/v1/block/3", "Bearer t0k", "soren", "bad build"}) {
		t.Errorf("block = %+v", got[0])
	}
	if got[1].path != "/v1/rescan/app" {
		t.Errorf("rescan path = %s", got[1].path)
	}
}

// The queue's refusal is the person's answer, carried through.
func TestARemoteRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"message": "app was asked for a moment ago"})
	}))
	defer srv.Close()
	c := NewCollector([]Upstream{{Host: "pi", URL: srv.URL}})
	if err := c.Check("pi", "app", ""); err == nil || !strings.Contains(err.Error(), "a moment ago") {
		t.Errorf("err = %v", err)
	}
}

type recordedActions struct{ calls []string }

func (r *recordedActions) Discard(host string, id int64, actor string) error {
	r.calls = append(r.calls, "discard "+host+" "+actor)
	return nil
}
func (r *recordedActions) Block(host string, id int64, reason, actor string) error {
	r.calls = append(r.calls, "block "+host+" "+reason+" "+actor)
	return nil
}
func (r *recordedActions) Check(host, service, actor string) error {
	r.calls = append(r.calls, "check "+host+" "+service+" "+actor)
	return nil
}
func (r *recordedActions) CheckAll(actor string) error {
	r.calls = append(r.calls, "check all "+actor)
	return nil
}
func (r *recordedActions) Unblock(host, image, digest, actor string) error {
	r.calls = append(r.calls, "unblock "+host+" "+image+"@"+digest+" "+actor)
	return nil
}
func (r *recordedActions) Rescan(host, service, actor string) error {
	r.calls = append(r.calls, "rescan "+host+" "+service+" "+actor)
	return nil
}

func TestThePagesActionRoutes(t *testing.T) {
	acts := &recordedActions{}
	s := &Server{Source: &fakeSource{}, Actions: acts, Actor: func(*http.Request) string { return "soren" }}
	h := s.Handler()

	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	post("/api/discard/optiplex/7", nil)
	post("/api/block/optiplex/8", url.Values{"reason": {" missing migrations "}})
	post("/api/check/pi/app", nil)
	post("/api/rescan/pi/app", nil)

	want := "discard optiplex soren|block optiplex missing migrations soren|check pi app soren|rescan pi app soren"
	if got := strings.Join(acts.calls, "|"); got != want {
		t.Errorf("calls = %s\nwant    %s", got, want)
	}
	if w := post("/api/discard/optiplex/notanumber", nil); !strings.Contains(w.Body.String(), "no such entry") {
		t.Errorf("a non-numeric entry = %s", w.Body.String())
	}
}

// Read-only: the actions are not there at all, not merely refusing.
func TestNoActionsMeansNoRoutes(t *testing.T) {
	h := (&Server{Source: &fakeSource{}}).Handler()
	r := httptest.NewRequest(http.MethodPost, "/api/block/optiplex/8", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "message") {
		t.Errorf("a read-only page answered an action: %d %s", w.Code, w.Body.String())
	}
}

func TestUnblockFromThePage(t *testing.T) {
	acts := &recordedActions{}
	h := (&Server{Source: &fakeSource{}, Actions: acts, Actor: func(*http.Request) string { return "soren" }}).Handler()
	form := url.Values{"image": {"postgres"}, "digest": {"sha256:bad"}}
	r := httptest.NewRequest(http.MethodPost, "/api/unblock/optiplex", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if len(acts.calls) != 1 || acts.calls[0] != "unblock optiplex postgres@sha256:bad soren" {
		t.Errorf("calls = %v", acts.calls)
	}
}

func TestUnblockReachesALocalQueue(t *testing.T) {
	local := &localQueue{}
	c := NewCollector([]Upstream{{Host: "optiplex", Local: local}})
	c.Unblock("optiplex", "postgres", "sha256:bad", "soren")
	if len(local.acted) != 1 || local.acted[0] != "unblock postgres@sha256:bad by soren" {
		t.Errorf("acted = %v", local.acted)
	}
}

// Every host is asked, whatever another answers, and a refusal says which.
func TestCheckAllAsksEveryHost(t *testing.T) {
	optiplex := &localQueue{}
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		json.NewEncoder(w).Encode(map[string]string{"message": "a check was asked for a moment ago"})
	}))
	defer srv.Close()
	c := NewCollector([]Upstream{{Host: "optiplex", Local: optiplex}, {Host: "pi", URL: srv.URL}})

	err := c.CheckAll("soren")
	if len(optiplex.acted) != 1 || optiplex.acted[0] != "check all by soren" {
		t.Errorf("optiplex acted = %v", optiplex.acted)
	}
	if asked != "/v1/check-all" {
		t.Errorf("pi was asked %q", asked)
	}
	if err == nil || !strings.Contains(err.Error(), "pi: ") {
		t.Errorf("err = %v, want pi's refusal, named", err)
	}
}

// The page is told when each host last looked, in the configured order.
func TestTheStateSaysWhenEachHostChecked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"last_check": "2026-10-21T03:00:00Z"})
	}))
	defer srv.Close()
	c := NewCollector([]Upstream{{Host: "pi", URL: srv.URL}})
	checks := c.LastChecks()
	if len(checks) != 1 || checks[0] != (HostCheck{Host: "pi", At: "2026-10-21T03:00:00Z"}) {
		t.Errorf("checks = %+v", checks)
	}
}
