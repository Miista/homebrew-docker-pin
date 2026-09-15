package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/update"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
)

// applier carries out an approved decision, one at a time.
//
// The lock is the whole of its concurrency: applying replaces a container and
// rewrites a file, and two at once on one host is a way to lose both. A
// candidate arriving mid-apply waits rather than cancelling what is running --
// and is then decided again, against the file as it is afterwards, which may
// make it a different change or none at all.
type applier struct {
	client  *update.Client
	timeout time.Duration
	log     zerolog.Logger

	// mu serialises applies. Held for the whole transaction, not just the
	// start: the point is that nothing else begins until this one is done.
	mu sync.Mutex

	// watchers guards the per-service fan-out. Separate from mu so a UI
	// asking to watch never waits behind the work it is asking about.
	watchers sync.Mutex
	running  map[string]*live

	// onFailure is told when an apply did not finish. Nil means nobody is.
	//
	// Only the failure: a successful apply is the policy working as
	// configured, and announcing every routine patch is how a topic becomes
	// something nobody reads. A failed one may have left the container down
	// and the repository dirty, which is nobody's business but a person's.
	onFailure func(queue.Entry, string)
}

// live is one apply in flight, and everyone currently watching it.
type live struct {
	mu    sync.Mutex
	subs  map[chan string]struct{}
	done  bool
	lines []string // what has been said so far, for a watcher arriving late
}

func newApplier(c *update.Client, timeout time.Duration, log zerolog.Logger) *applier {
	return &applier{
		client:  c,
		timeout: timeout,
		log:     log,
		running: map[string]*live{},
	}
}

// Start begins applying an entry, returning once the updater has accepted it.
//
// It returns before the work finishes: an apply can take minutes, and a
// caller holding an HTTP request open for it would time out long before the
// updater did.
func (a *applier) Start(e queue.Entry) error {
	a.watchers.Lock()
	if _, already := a.running[e.Service]; already {
		a.watchers.Unlock()
		return fmt.Errorf("%s is already being updated", e.Service)
	}
	l := &live{subs: map[chan string]struct{}{}}
	a.running[e.Service] = l
	a.watchers.Unlock()

	go a.apply(e, l)
	return nil
}

func (a *applier) apply(e queue.Entry, l *live) {
	defer func() {
		l.finish()
		a.watchers.Lock()
		delete(a.running, e.Service)
		a.watchers.Unlock()
	}()

	// One at a time, across every service on this host.
	a.mu.Lock()
	defer a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()

	acc, err := a.client.Apply(ctx, update.Request{
		Service: e.Service,
		File:    e.File,
		Image:   e.Image,
		From:    e.From,
		To:      e.To,
		Tag:     e.Tag,
		Digest:  e.Digest,
	})
	if err != nil {
		a.log.Error().Msgf("%s: the actor would not take it — %v", e.Service, err)
		l.write(update.Terminal(update.Failed, err.Error()))
		a.failed(e, err.Error())
		return
	}

	status, reason, err := a.client.Watch(ctx, acc.Stream, l)
	switch {
	case err != nil:
		a.log.Error().Msgf("%s: lost the actor's stream — %v", e.Service, err)
		l.write(update.Terminal(update.Failed, err.Error()))
		a.failed(e, err.Error())
	case status == update.Completed:
		a.log.Info().Msgf("%s: %s -> %s applied", e.Service, e.From, e.To)
	default:
		a.log.Error().Msgf("%s: failed — %s", e.Service, reason)
		a.failed(e, reason)
	}
}

// failed tells whoever is listening that an apply did not finish.
func (a *applier) failed(e queue.Entry, reason string) {
	if a.onFailure != nil {
		a.onFailure(e, reason)
	}
}

// Stream relays an in-flight apply to a watcher.
//
// Nothing running means nothing to say: the response is empty and ends, which
// is the whole of this endpoint's error handling.
func (a *applier) Stream(service string, w http.ResponseWriter, r *http.Request) {
	a.watchers.Lock()
	l, ok := a.running[service]
	a.watchers.Unlock()
	if !ok {
		return
	}

	ch, backlog := l.subscribe()
	defer l.unsubscribe(ch)

	// What has already been said, so a watcher arriving mid-apply sees how
	// it got here rather than joining a conversation in progress.
	for _, line := range backlog {
		fmt.Fprintln(w, line)
	}
	flush(w)

	for {
		select {
		case line, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintln(w, line)
			flush(w)
		case <-r.Context().Done():
			// The watcher left. The apply carries on without them.
			return
		}
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// Write makes live an io.Writer, so the updater client can tee straight into it.
func (l *live) Write(p []byte) (int, error) {
	for _, line := range splitLines(p) {
		l.write(line)
	}
	return len(p), nil
}

func (l *live) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.lines = append(l.lines, line)
	for ch := range l.subs {
		// Never block on a watcher: one that has stopped reading must not
		// stall the apply everyone else is watching.
		select {
		case ch <- line:
		default:
		}
	}
}

func (l *live) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done = true
	for ch := range l.subs {
		close(ch)
	}
	l.subs = map[chan string]struct{}{}
}

func (l *live) subscribe() (chan string, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	backlog := append([]string(nil), l.lines...)
	if l.done {
		ch := make(chan string)
		close(ch)
		return ch, backlog
	}
	// Buffered so a slow watcher has room before lines start being dropped.
	ch := make(chan string, 256)
	l.subs[ch] = struct{}{}
	return ch, backlog
}

func (l *live) unsubscribe(ch chan string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.subs[ch]; ok {
		delete(l.subs, ch)
		close(ch)
	}
}

// splitLines breaks a write into lines, dropping the trailing empty one that
// a newline-terminated write produces.
func splitLines(p []byte) []string {
	var out []string
	start := 0
	for i, b := range p {
		if b == '\n' {
			out = append(out, string(trimCR(p[start:i])))
			start = i + 1
		}
	}
	if start < len(p) {
		out = append(out, string(trimCR(p[start:])))
	}
	return out
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

var _ io.Writer = (*live)(nil)

// Ready asks the updater whether work offered now would be attempted.
//
// Asked per snapshot rather than cached: what makes this updater unready is a
// dirty working tree, which becomes clean the moment someone commits, and a
// cached answer would leave the button grey after the thing that greyed it
// was fixed. It is one local request against a container on the same host.
func (a *applier) Ready() (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := a.client.Ready(ctx)
	return r.Ready, r.Reason
}
