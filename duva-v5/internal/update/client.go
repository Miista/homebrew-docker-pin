package update

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
)

// WatchURL follows a stream served over HTTP to its terminal line, copying
// every line to tee, and returns how the work ended.
//
// Only a self-update's helper is watched this way: it runs in its own
// container, so its progress is a request away rather than a call.
func WatchURL(ctx context.Context, streamURL string, tee io.Writer) (Status, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return Failed, "", err
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return Failed, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Failed, "", fmt.Errorf("watching: %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	// Progress lines are short; a long one is a runaway rather than
	// something worth buffering megabytes for.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()

		// Relayed verbatim, terminal line included: a person watching should
		// see how it ended, not have the last line swallowed by the thing
		// that was looking for it.
		if tee != nil {
			fmt.Fprintln(tee, line)
			if f, ok := tee.(http.Flusher); ok {
				f.Flush()
			}
		}

		if status, reason, ok := ParseTerminal(line); ok {
			return status, reason, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return Failed, "", fmt.Errorf("reading the stream: %w", err)
	}

	// The updater stopped talking without saying how it went. That is a
	// failure whatever caused it -- a crash, a restart, a bug -- because the
	// alternative is calling an unfinished apply a success.
	return Failed, "the actor's stream ended without a status", nil
}

// Updater is everything the queue asks of an updater.
//
// In duva v5 the updater runs in the queue's process and answers these
// directly. The queue cannot tell how, which is the point of the contract:
// an updater that opens a pull request instead would answer the same four.
type Updater interface {
	// Apply starts a request and says where to watch it.
	Apply(ctx context.Context, r Request) (Accepted, error)
	// Watch follows a started request to its end, copying its progress to
	// tee, and returns how it ended.
	Watch(ctx context.Context, stream string, tee io.Writer) (Status, string, error)
	// Ready is whether work offered now would be attempted.
	Ready(ctx context.Context) Readiness
	// Applicable is whether a service should be acted on at all.
	Applicable(ctx context.Context, service string) Applicable
}
