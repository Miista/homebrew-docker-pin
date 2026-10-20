package dockerapi

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// lifecycle is what a container does that changes what duva should do about
// it: one appears or goes, starts or stops.
const lifecycle = `{"type":["container"],"event":["create","start","die","destroy"]}`

// eventsHTTP is dockerHTTP without its timeout. The events endpoint is a
// stream that stays open for as long as it is watched; a timeout would cut it
// every two minutes.
var eventsHTTP = &http.Client{Transport: dockerHTTP.Transport}

// ContainerEvents calls changed each time a container on this daemon is
// created, started, stopped or destroyed, until ctx is done or the stream
// breaks.
//
// It says that something changed, not what: the caller re-reads what it cares
// about, which is the same answer however many events arrived. A broken
// stream is returned rather than retried here, so the caller decides how hard
// to try -- and events missed in between are why a caller should also look on
// a timer.
func ContainerEvents(ctx context.Context, changed func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker"+apiVersion+"/events?filters="+url.QueryEscape(lifecycle), nil)
	if err != nil {
		return err
	}
	resp, err := eventsHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("watching the daemon's events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("watching the daemon's events: %s", resp.Status)
	}

	// One JSON object per line. Its content is not needed, only that it came.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		changed()
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading the daemon's events: %w", err)
	}
	return fmt.Errorf("the daemon closed its event stream")
}
