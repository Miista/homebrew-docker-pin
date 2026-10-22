//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// peerAPI is the fixture's duva API as a peer's page reaches it.
const peerAPI = "http://127.0.0.1:18080"

// peerSnapshot is what a page reads of a peer, as much as this test needs.
type peerSnapshot struct {
	Pending []struct {
		ID      int64  `json:"id"`
		Service string `json:"service"`
	} `json:"pending"`
	CanApply bool     `json:"can_apply"`
	Blocker  string   `json:"blocker"`
	WhyNot   string   `json:"why_not"`
	Applying []string `json:"applying"`
}

func peerGet(t *testing.T, path string) (peerSnapshot, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, peerAPI+path, nil)
	req.Header.Set("Authorization", "Bearer "+v5Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return peerSnapshot{}, err
	}
	defer resp.Body.Close()
	var s peerSnapshot
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return s, fmt.Errorf("%s: %s", resp.Status, body)
	}
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// A duva approved by a page on another host -- what pi is to the page on
// optiplex -- looks, through the whole apply, like a host that can update,
// and its entry ends completed, not dropped.
//
// The page knows a peer only by its API, so this test is that page: it
// approves over HTTP and reads the snapshot every 50ms while the apply runs,
// as often as anything could. Two things went wrong here on optiplex on
// 2026-10-21, both from the moment an apply has rewritten the compose file
// and not yet committed it:
//   - the readiness check saw the apply's own rewrite as an uncommitted
//     repository, and the page said the host could not update;
//   - a reconcile saw the file already pinning the new release and dropped
//     the entry, which recorded a completed update as dropped and took its
//     row off the page before it said how it went.
func TestV5APeerApprovedRemotelyStaysReadyAndCompletes(t *testing.T) {
	s := Up(t, "duva-v5/peer-apply")
	s.PushUpdates()
	s.Pin("app")
	s.Start()

	var entry int64
	s.waitFor("the patch to wait for approval", 2*time.Minute, func() bool {
		snap, err := peerGet(t, "/v1/snapshot")
		if err != nil {
			return false
		}
		for _, e := range snap.Pending {
			if e.Service == "app" {
				entry = e.ID
				return true
			}
		}
		return false
	}, func() string { return s.Logs("duva") })

	// The page, polling. Every snapshot from the approval until well after
	// the apply is over -- the reconcile runs each second, so several fall
	// inside it.
	var (
		mu      sync.Mutex
		polled  int
		refused []string
		sawBusy bool
	)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			snap, err := peerGet(t, "/v1/snapshot")
			if err != nil {
				continue
			}
			mu.Lock()
			polled++
			if len(snap.Applying) > 0 {
				sawBusy = true
			}
			if !snap.CanApply {
				refused = append(refused, snap.Blocker+": "+snap.WhyNot)
			}
			mu.Unlock()
		}
	}()

	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/v1/apply/app?entry=%d", peerAPI, entry), nil)
	req.Header.Set("Authorization", "Bearer "+v5Token)
	req.Header.Set("X-Duva-Actor", "the page on optiplex")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("approving over the API: %v", err)
	}
	var answer struct{ Message string }
	json.NewDecoder(resp.Body).Decode(&answer)
	resp.Body.Close()
	if answer.Message != "" {
		t.Fatalf("the approval was refused: %s", answer.Message)
	}

	s.waitFor("the patch to be applied", 3*time.Minute,
		func() bool { return strings.Contains(s.RunningImage("app"), "1.0.1") && strings.Contains(s.Log(), "pin app to 1.0.1") },
		func() string { return s.Logs("duva") })
	// Past the apply: a reconcile or two after it, when a dropped entry or a
	// stale readiness answer would also show.
	time.Sleep(3 * time.Second)
	close(stop)
	<-done

	logs := s.Logs("duva")
	mu.Lock()
	defer mu.Unlock()

	t.Run("the page saw the apply in flight", func(t *testing.T) {
		// Otherwise the polling missed the window it is here to watch, and
		// the rest proves nothing.
		if !sawBusy {
			t.Fatalf("no snapshot of %d showed the apply running", polled)
		}
	})

	t.Run("the host never looked unable to update", func(t *testing.T) {
		if len(refused) > 0 {
			t.Errorf("%d of %d snapshots said the host could not update, first: %s",
				len(refused), polled, refused[0])
		}
	})

	t.Run("the entry completed, not dropped", func(t *testing.T) {
		if strings.Contains(logs, "app: 1.0.0 -> 1.0.1 no longer applies") {
			t.Errorf("a reconcile dropped the entry being applied:\n%s", logs)
		}
		if !strings.Contains(logs, "app: 1.0.0 -> 1.0.1 applied") {
			t.Errorf("the entry was not recorded as applied:\n%s", logs)
		}
	})

	t.Run("and nothing is left waiting", func(t *testing.T) {
		snap, err := peerGet(t, "/v1/snapshot")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range snap.Pending {
			if e.Service == "app" {
				t.Errorf("app is still waiting after its update was applied")
			}
		}
	})
}
