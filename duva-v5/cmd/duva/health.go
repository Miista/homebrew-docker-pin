package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
)

// health is the exit code for `duva health`, the image's HEALTHCHECK: 0 when
// everything serve listens with is answering, 1 otherwise.
//
// It asks the running process over loopback rather than re-deriving anything.
// Watch and update listen on nothing, and need no check of their own: one
// that keeps failing takes the whole process down (see supervise), which is
// what a health check would have reported.
func health() int {
	addrs := []string{"127.0.0.1:8080"}
	if on, _ := settings.Bool(settings.UI, false); on {
		addrs = append(addrs, "127.0.0.1"+uiAddr)
	}
	return healthOf(addrs)
}

func healthOf(addrs []string) int {
	c := &http.Client{Timeout: 5 * time.Second}
	for _, addr := range addrs {
		resp, err := c.Get("http://" + addr + "/healthz")
		if err != nil {
			fmt.Fprintf(os.Stderr, "nothing answering on %s: %v\n", addr, err)
			return 1
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "%s answered /healthz with %s\n", addr, resp.Status)
			return 1
		}
	}
	return 0
}
