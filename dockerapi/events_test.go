package dockerapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// overDaemon points the events client at a fake daemon for one test.
func overDaemon(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := eventsHTTP
	eventsHTTP = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(srv.URL, "http://"))
		},
	}}
	t.Cleanup(func() { eventsHTTP = old })
}

func TestEachEventIsReported(t *testing.T) {
	var asked string
	overDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("filters")
		w.Write([]byte(`{"status":"start","id":"a"}` + "\n" + `{"status":"die","id":"b"}` + "\n"))
	})
	n := 0
	err := ContainerEvents(context.Background(), func() { n++ })
	if n != 2 {
		t.Errorf("reported %d events, want 2", n)
	}
	// The stream ending is not the caller's doing, so it is an error: the
	// caller should start watching again.
	if err == nil {
		t.Error("a closed stream was not reported")
	}
	if !strings.Contains(asked, `"container"`) || !strings.Contains(asked, `"start"`) {
		t.Errorf("filters = %s, want container lifecycle events", asked)
	}
}

// Being told to stop is not a failure.
func TestStoppingIsNotAnError(t *testing.T) {
	overDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := ContainerEvents(ctx, func() {}); err != nil {
		t.Errorf("err = %v, want nil when ctx ends it", err)
	}
}
