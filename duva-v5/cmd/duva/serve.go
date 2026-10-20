package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/dockerapi"
	"github.com/Miista/homebrew-docker-pin/oci/registry"

	"github.com/Miista/homebrew-docker-pin/duva-v5/gate"
	q "github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/ui"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
	"github.com/Miista/homebrew-docker-pin/duva-v5/updater"
	"github.com/Miista/homebrew-docker-pin/duva-v5/watcher"
	"github.com/Miista/homebrew-docker-pin/duva-v5/webui"
)

// serve runs watch, queue and updater in this process.
//
// They are the same components the subcommands run, connected by calls
// instead of HTTP: the watcher hands a finding to the queue's Handle, and the
// queue applies through the updater's Local. Only the queue's API still
// listens, because a UI on another host reads it.
//
// With DUVA_UI set it serves the page too, on uiAddr: this host's queue by
// call, other hosts' through DUVA_QUEUES as before. On its own port, not the
// queue API's, because the page is published to the internet and the API is
// not, and sharing a port would publish both.
//
// The project is the updater's -- DUVA_REPO, mounted at the path it has on
// the host -- and every component reads that one, rather than v4's read-only
// copy at /compose.
func serve() error {
	if err := settings.CheckRenamed(os.Getenv); err != nil {
		return err
	}
	// The store first: settings are read through it, the log level included.
	st, err := store.Open(dataFile)
	if err != nil {
		return err
	}
	defer st.Close()
	settings.Use(st)
	level, err := settings.String(settings.LogLevel, "")
	if err != nil {
		return err
	}
	log := newLogger(level)

	root, err := updater.ProjectFile()
	if err != nil {
		return err
	}
	if err := importJSON(st, filepath.Dir(dataFile), root, log); err != nil {
		return err
	}
	// The blocklist is the queue's, and the queue is built from the
	// updater, so the updater asks through this once both exist.
	var queue *gate.Gate
	blocked := func(image, digest string) (string, bool) { return queue.IsBlocked(image, digest) }
	up, err := updater.New(component(log, "update"), blocked)
	if err != nil {
		return err
	}
	watching := watcher.Options{
		Root:   root,
		Report: func(f watch.Finding) bool { return hand(queue, f) },
		Labels: dockerapi.Labels,
		Store:  st,
	}
	watchLog := component(log, "watch")

	queue, err = gate.New(component(log, "queue"), gate.Options{
		Updater: up.Local(),
		Root:    root,
		Store:   st,
		// Policy from the running container: what governs a service is what
		// it was started with, until it is recreated.
		Labels: dockerapi.Labels,
		// A person's "check now" and "rescan", and the lookup a block of a
		// version change needs: the watcher's, and its registry client's.
		Check:   func(service string) error { return watcher.Check(watchLog, service, watching) },
		Rescan:  func(service string) error { return watcher.Rescan(watchLog, service, watching) },
		Resolve: registry.RemoteDigest,
		// The service sync's view of this host. Translated here: the queue
		// knows nothing about docker.
		Containers: func() ([]q.Container, error) {
			found, err := dockerapi.ProjectContainers()
			out := make([]q.Container, 0, len(found))
			for _, c := range found {
				out = append(out, q.Container{Name: c.Name, Service: c.Service, Image: c.Image})
			}
			return out, err
		},
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	failed := make(chan error, 4)
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
		return watcher.Serve(ctx, watchLog, watching)
	})

	run("events", func(ctx context.Context) error {
		follow(ctx, component(log, "events"), dockerapi.ContainerEvents, queue.Poke)
		return nil
	})

	if withUI, err := boolEnv("DUVA_UI"); err != nil {
		return err
	} else if withUI {
		local := &ui.Upstream{Host: queue.Host(), Local: queue.API}
		run("ui", func(ctx context.Context) error {
			return webui.Run(ctx, component(log, "ui"), webui.Options{Local: local, Addr: uiAddr})
		})
	}

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

// uiAddr is where serve listens for the page when DUVA_UI is set.
const uiAddr = ":8081"

// boolEnv reads a boolean variable, false when unset.
func boolEnv(key string) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a boolean", key, raw)
	}
	return v, nil
}

// follow keeps watching the daemon's container events, poking the queue on
// each, until ctx is done.
//
// Never fatal. Events only make the queue look sooner: its reconcile tick
// looks anyway, so a daemon whose stream keeps breaking costs promptness and
// nothing else. A broken stream is opened again after a pause that doubles up
// to a minute, and is back to quick once one has stayed up.
func follow(ctx context.Context, log zerolog.Logger, watch func(context.Context, func()) error, poke func()) {
	const first, ceiling = time.Second, time.Minute
	wait := first
	for {
		began := time.Now()
		err := watch(ctx, poke)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > ceiling {
			wait = first
		}
		log.Warn().Msgf("%v; watching again in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, ceiling)
	}
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
