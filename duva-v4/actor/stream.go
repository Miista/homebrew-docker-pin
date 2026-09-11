package main

import (
	"fmt"
	"sync"
)

// stream is one piece of work's output, and everyone watching it.
//
// Kept in memory and discarded when the work ends. There is nothing to
// persist: a stream is what is happening now, and what happened before is in
// the container's log and the git history.
type stream struct {
	mu    sync.Mutex
	subs  map[chan string]struct{}
	lines []string
	done  bool
}

func newStream() *stream {
	return &stream{subs: map[chan string]struct{}{}}
}

// write records a line and sends it to every watcher.
//
// Formatted here rather than by the caller so the transaction can be handed
// this directly as its step function.
func (s *stream) write(format string, args ...any) {
	line := format
	if len(args) > 0 {
		line = fmt.Sprintf(format, args...)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.lines = append(s.lines, line)
	for ch := range s.subs {
		// Never block on a watcher: one that has stopped reading must not
		// stall the work everyone else is waiting on.
		select {
		case ch <- line:
		default:
		}
	}
}

// finish closes the stream. Watchers see their channel close and stop.
func (s *stream) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	for ch := range s.subs {
		close(ch)
	}
	s.subs = map[chan string]struct{}{}
}

// subscribe returns a channel of future lines and everything said so far.
func (s *stream) subscribe() (chan string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	backlog := append([]string(nil), s.lines...)
	if s.done {
		ch := make(chan string)
		close(ch)
		return ch, backlog
	}
	// Buffered so a slow watcher has room before lines are dropped.
	ch := make(chan string, 256)
	s.subs[ch] = struct{}{}
	return ch, backlog
}

func (s *stream) unsubscribe(ch chan string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[ch]; ok {
		delete(s.subs, ch)
		close(ch)
	}
}

// finished reports whether the work this stream belongs to has ended.
func (s *stream) finished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}
