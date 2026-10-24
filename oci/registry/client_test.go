package registry

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The whole point of the shared client: a run that makes many registry calls
// must not open a connection per call.
//
// This is what makes a scheduled check look like a flood to a resolver -- one
// connection is one dial is one DNS lookup, and diun tripping Pi-hole's
// 1000-queries-a-minute limit is what this exists to avoid repeating.
func TestConnectionsAreReusedAcrossCalls(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns[c.RemoteAddr().String()] = true
			mu.Unlock()
		}
	}
	defer srv.Close()

	const calls = 20
	for i := 0; i < calls; i++ {
		resp, err := httpClient().Get(srv.URL)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		// The body must be drained and closed or the connection is not
		// returned to the pool -- which would make this test pass for the
		// wrong reason if the production code ever stopped doing it.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	mu.Lock()
	n := len(conns)
	mu.Unlock()
	if n >= calls {
		t.Fatalf("%d calls opened %d connections: nothing is being reused", calls, n)
	}
	// Sequential calls against one host should need exactly one.
	if n != 1 {
		t.Errorf("%d calls opened %d connections, want 1", calls, n)
	}
}

// The transport must actually be configured, not left at defaults: the
// default MaxIdleConnsPerHost of 2 throttles the bursty-few-hosts case this
// package is entirely made of.
func TestTransportKeepsEnoughIdleConnections(t *testing.T) {
	tr, ok := sharedClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the shared client has no *http.Transport: %T", sharedClient.Transport)
	}
	if tr.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want more than the default %d",
			tr.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout == 0 {
		t.Error("IdleConnTimeout is unset, so idle connections never expire")
	}
}

// Every call site must share one client. A call site that builds its own gets
// its own connection pool, which is the bug this package just fixed.
func TestEveryCallSiteSharesOneClient(t *testing.T) {
	if httpClient() != sharedClient {
		t.Error("httpClient() does not return the shared client")
	}
	if httpClient() != httpClient() {
		t.Error("httpClient() returns a different client each call")
	}
}
