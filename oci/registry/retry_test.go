package registry

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListTags_RetriesOn429(t *testing.T) {
	var waits []time.Duration
	retrySleep = func(d time.Duration) { waits = append(waits, d) }
	defer func() { retrySleep = time.Sleep }()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"tags":["1.0.0","1.0.1"]}`))
	}))
	defer srv.Close()

	tags, err := ociListTags(srv.Client(), srv.URL, "some/repo")
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if len(tags) != 2 || calls != 3 {
		t.Errorf("tags=%v calls=%d, want 2 tags after 3 calls", tags, calls)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] != 7*time.Second {
		t.Errorf("waits=%v, want two 7s waits (Retry-After honored)", waits)
	}
}

func TestListTags_GivesUpAfter429s(t *testing.T) {
	retrySleep = func(time.Duration) {}
	defer func() { retrySleep = time.Sleep }()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	if _, err := ociListTags(srv.Client(), srv.URL, "some/repo"); err == nil {
		t.Fatal("want error when the registry keeps throttling")
	}
	if calls != retryAttempts {
		t.Errorf("calls=%d, want %d", calls, retryAttempts)
	}
}
