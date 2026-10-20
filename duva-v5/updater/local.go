package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/update"
)

// Local is the updater answering a queue in the same process: the four
// questions update.Client asks over HTTP, answered by calling the server
// directly.
//
// Not a second implementation of anything. Apply is Start, Ready is
// Readiness, and Watch reads the same stream the HTTP endpoint relays, so a
// request takes the same path either way -- the self-update handoff included.
func (s *Server) Local() update.Updater { return local{s} }

type local struct{ s *Server }

func (l local) Apply(_ context.Context, r update.Request) (update.Accepted, error) {
	return l.s.Start(r)
}

func (l local) Ready(context.Context) update.Readiness { return l.s.Readiness() }

func (l local) Applicable(_ context.Context, service string) update.Applicable {
	if l.s.Applicable == nil {
		return update.Applicable{Applicable: true}
	}
	return l.s.Applicable(service)
}

// Watch follows a stream to its terminal line.
//
// A self-update is the exception that is not in this process: Start answers
// with the helper's own URL, because the helper is the one doing the work,
// so that is watched over HTTP as before.
func (l local) Watch(ctx context.Context, loc string, tee io.Writer) (update.Status, string, error) {
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return (&update.Client{}).Watch(ctx, loc, tee)
	}
	service := strings.TrimPrefix(loc, "/v1/stream/")
	l.s.mu.Lock()
	st, ok := l.s.running[service]
	l.s.mu.Unlock()
	if !ok {
		return update.Failed, "", fmt.Errorf("nothing is being applied to %s", service)
	}

	ch, backlog := st.subscribe()
	defer st.unsubscribe(ch)
	say := func(line string) {
		if tee != nil {
			fmt.Fprintln(tee, line)
			if f, ok := tee.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
	for _, line := range backlog {
		say(line)
		if status, reason, ok := update.ParseTerminal(line); ok {
			return status, reason, nil
		}
	}
	for {
		select {
		case line, open := <-ch:
			if !open {
				return terminalOf(st)
			}
			say(line)
			if status, reason, ok := update.ParseTerminal(line); ok {
				return status, reason, nil
			}
		case <-ctx.Done():
			return update.Failed, "", ctx.Err()
		}
	}
}

// terminalOf reads how a finished stream ended from its whole record.
//
// A subscriber that falls behind has lines dropped rather than stalling the
// apply, and the one it must not lose is the last. The record keeps every
// line, so the answer comes from there rather than from what this subscriber
// happened to receive.
func terminalOf(st *stream) (update.Status, string, error) {
	_, lines := st.subscribe()
	for i := len(lines) - 1; i >= 0; i-- {
		if status, reason, ok := update.ParseTerminal(lines[i]); ok {
			return status, reason, nil
		}
	}
	return update.Failed, "the updater's stream ended without a status", nil
}
