package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
)

// The container's health check restarts on unhealthy alone: degraded is a 200
// with reasons for a monitor, and restarting would mend none of them.
func TestDockersHealthCheckActsOnUnhealthyAlone(t *testing.T) {
	for status, want := range map[string]int{queue.HealthOK: 0, queue.HealthDegraded: 0, queue.HealthUnhealthy: 1} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			queue.WriteHealth(w, queue.Health{Status: status})
		}))
		if got := healthOf([]string{strings.TrimPrefix(srv.URL, "http://")}); got != want {
			t.Errorf("%s: exit %d, want %d", status, got, want)
		}
		srv.Close()
	}
}
