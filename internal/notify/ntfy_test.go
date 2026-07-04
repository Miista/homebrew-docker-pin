package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSend(t *testing.T) {
	var gotPath, gotTitle, gotPriority, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	n := Ntfy{URL: srv.URL, Topic: "pins", Token: "tk_x"}
	if err := n.Send("2 upgraded", "caddy: a -> b", PriorityDefault); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/pins" || gotTitle != "2 upgraded" || gotPriority != "3" ||
		gotAuth != "Bearer tk_x" || gotBody != "caddy: a -> b" {
		t.Errorf("got path=%q title=%q prio=%q auth=%q body=%q",
			gotPath, gotTitle, gotPriority, gotAuth, gotBody)
	}
}

func TestSend_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	n := Ntfy{URL: srv.URL, Topic: "pins"}
	if err := n.Send("t", "m", PriorityHigh); err == nil {
		t.Fatal("want error on HTTP 403")
	}
}
