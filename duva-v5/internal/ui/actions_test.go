package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestActionsReachALocalQueueWithWho(t *testing.T) {
	local := &localQueue{}
	c := NewCollector([]Upstream{{Host: "optiplex", Local: local}})

	c.Discard("optiplex", 7, "soren")
	c.Block("optiplex", 8, "missing migrations", "soren")
	c.Check("optiplex", "app", "soren", 0)
	c.Check("optiplex", "db", "soren", 7*24*time.Hour)

	want := []string{"discard 7 by soren", "block 8 (missing migrations) by soren", "check app 0s by soren", "check db 168h0m0s by soren"}
	if strings.Join(local.acted, "|") != strings.Join(want, "|") {
		t.Errorf("acted = %v\nwant    %v", local.acted, want)
	}
}

// Another host's queue is reached over its API: the token it trusts, and
// whom the action is for.
func TestActionsReachARemoteQueueOverItsAPI(t *testing.T) {
	type call struct{ path, token, actor, reason, since string }
	var got []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = append(got, call{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Duva-Actor"),
			r.FormValue("reason"), r.FormValue("since")})
		json.NewEncoder(w).Encode(map[string]string{"message": ""})
	}))
	defer srv.Close()
	c := NewCollector([]Upstream{{Host: "pi", URL: srv.URL, Token: "t0k"}})

	if err := c.Block("pi", 3, "bad build", "soren"); err != nil {
		t.Fatal(err)
	}
	if err := c.Check("pi", "app", "soren", 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckAll("soren", 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("calls = %+v", got)
	}
	if got[0] != (call{"/v1/block/3", "Bearer t0k", "soren", "bad build", ""}) {
		t.Errorf("block = %+v", got[0])
	}
	// The look-back travels as the API reads it; none, for since the last
	// check, is no parameter rather than a sentence.
	if got[1].path != "/v1/check/app" || got[1].since != "7d" {
		t.Errorf("check = %+v, want /v1/check/app since 7d", got[1])
	}
	if got[2].path != "/v1/check-all" || got[2].since != "" {
		t.Errorf("check-all = %+v, want no since", got[2])
	}
}

// The queue's refusal is the person's answer, carried through.
func TestARemoteRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"message": "app was asked for a moment ago"})
	}))
	defer srv.Close()
	c := NewCollector([]Upstream{{Host: "pi", URL: srv.URL}})
	if err := c.Check("pi", "app", "", 0); err == nil || !strings.Contains(err.Error(), "a moment ago") {
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
func (r *recordedActions) Check(host, service, actor string, back time.Duration) error {
	r.calls = append(r.calls, "check "+host+" "+service+" "+back.String()+" "+actor)
	return nil
}
func (r *recordedActions) CheckAll(actor string, back time.Duration) error {
	r.calls = append(r.calls, "check all "+back.String()+" "+actor)
	return nil
}
func (r *recordedActions) RunJob(host, job, actor string) error {
	r.calls = append(r.calls, "run "+host+" "+job+" "+actor)
	return nil
}
func (r *recordedActions) Unblock(host, image, digest, actor string) error {
	r.calls = append(r.calls, "unblock "+host+" "+image+"@"+digest+" "+actor)
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
	post("/api/check/pi/db", url.Values{"since": {"7d"}})
	post("/api/check-all", url.Values{"since": {"30d"}})

	want := "discard optiplex soren|block optiplex missing migrations soren|check pi app 0s soren|check pi db 168h0m0s soren|check all 720h0m0s soren"
	if got := strings.Join(acts.calls, "|"); got != want {
		t.Errorf("calls = %s\nwant    %s", got, want)
	}
	// A look-back the page did not offer is refused here, before any host
	// is asked anything.
	if w := post("/api/check-all", url.Values{"since": {"a year"}}); !strings.Contains(w.Body.String(), "since") {
		t.Errorf("since=a year = %s, want a refusal", w.Body.String())
	}
	if w := post("/api/rescan/pi/app", nil); w.Code == http.StatusOK && strings.Contains(w.Body.String(), "message") {
		t.Errorf("/api/rescan still answers: %s", w.Body.String())
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

	err := c.CheckAll("soren", 0)
	if len(optiplex.acted) != 1 || optiplex.acted[0] != "check all 0s by soren" {
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
		json.NewEncoder(w).Encode(map[string]any{"last_check": "2026-10-21T03:00:00Z",
			"services": []string{"app", "db"}})
	}))
	defer srv.Close()
	c := NewCollector([]Upstream{{Host: "pi", URL: srv.URL}})
	checks := c.LastChecks()
	if len(checks) != 1 || checks[0].Host != "pi" || checks[0].At != "2026-10-21T03:00:00Z" ||
		strings.Join(checks[0].Services, ",") != "app,db" {
		t.Errorf("checks = %+v, want pi's time and both its services", checks)
	}
}
