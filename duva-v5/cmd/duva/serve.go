package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v5/gate"
	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
	"github.com/Miista/homebrew-docker-pin/duva-v5/updater"
	"github.com/Miista/homebrew-docker-pin/duva-v5/watcher"
)

// serve runs watch, queue and updater in this process.
//
// They are the same components the subcommands run, connected by calls
// instead of HTTP: the watcher hands a finding to the queue's Handle, and the
// queue applies through the updater's Local. Only the queue's API still
// listens, because a UI on another host reads it.
//
// The project is the updater's -- DUVA_REPO, mounted at the path it has on
// the host -- and every component reads that one, rather than v4's read-only
// copy at /compose.
func serve() error {
	log := newLogger(os.Getenv("DUVA_LOG_LEVEL"))

	root, err := updater.ProjectFile()
	if err != nil {
		return err
	}
	up, err := updater.New(component(log, "update"))
	if err != nil {
		return err
	}
	queue, err := gate.New(component(log, "queue"), gate.Options{Updater: up.Local(), Root: root})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	failed := make(chan error, 2)
	run := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := supervise(ctx, component(log, name), fn); err != nil {
				failed <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}
	run("queue", queue.Run)
	run("watch", func(ctx context.Context) error {
		return watcher.Serve(ctx, component(log, "watch"), watcher.Options{
			Root:   root,
			Report: func(f watch.Finding) bool { return hand(queue, f) },
		})
	})

	// One component giving up takes the process with it, and Docker's
	// restart policy brings all of them back. A duva that keeps watching
	// with nothing left to decide is quieter than one that is down, and
	// that is worse.
	var err2 error
	select {
	case <-ctx.Done():
	case err2 = <-failed:
		log.Error().Msgf("%v", err2)
		stop()
	}
	wg.Wait()
	return err2
}

// hand gives one finding to the queue, reporting whether it landed.
//
// What the webhook did, as a call. A finding the queue could not take -- its
// compose file unreadable for a moment -- is held by the watcher and offered
// again next run, exactly as a failed POST was.
func hand(g handler, f watch.Finding) bool {
	if f.Container == "" || f.Image == "" || f.Tag == "" {
		// Nothing to match it to. Taken, so it is not held forever.
		return true
	}
	res := g.Handle(q.Notice{
		Container: f.Container,
		Image:     f.Image + ":" + f.Tag,
		Digest:    f.Digest,
		Published: f.Published,
	})
	return res.Status < http.StatusInternalServerError
}

// handler is the part of the queue a watcher hands findings to.
type handler interface {
	Handle(q.Notice) q.Result
}

// supervise runs a component until ctx is done, restarting it after a panic.
//
// A panic in the watcher is a bug in the watcher; it must not stop the
// updater halfway through replacing a container. So each component recovers
// on its own and is started again. One that keeps panicking -- three times in
// ten minutes -- is not going to stop, and is returned as an error instead.
func supervise(ctx context.Context, log zerolog.Logger, fn func(context.Context) error) error {
	var panics []time.Time
	for {
		err, panicked := once(ctx, fn)
		if !panicked {
			return err
		}
		log.Error().Msgf("panicked: %v; starting it again", err)

		now := time.Now()
		recent := panics[:0]
		for _, at := range panics {
			if now.Sub(at) < 10*time.Minute {
				recent = append(recent, at)
			}
		}
		panics = append(recent, now)
		if len(panics) >= 3 {
			return fmt.Errorf("panicked %d times in ten minutes: %w", len(panics), err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

// once runs fn, turning a panic into an error.
func once(ctx context.Context, fn func(context.Context) error) (err error, panicked bool) {
	defer func() {
		if p := recover(); p != nil {
			err, panicked = fmt.Errorf("%v", p), true
		}
	}()
	return fn(ctx), false
}

// component tags a component's log lines, so one container's log still says
// which part of duva said what.
func component(log zerolog.Logger, name string) zerolog.Logger {
	return log.With().Str("component", name).Logger()
}

func newLogger(levelStr string) zerolog.Logger {
	level := zerolog.InfoLevel
	if levelStr != "" {
		if parsed, err := zerolog.ParseLevel(strings.ToLower(levelStr)); err == nil {
			level = parsed
		}
	}
	writer := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "2006-01-02 15:04:05"}
	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}
