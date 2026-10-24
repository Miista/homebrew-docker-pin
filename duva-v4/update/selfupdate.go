package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/dockerapi"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/update"
)

// An updater cannot replace its own container: Recreate stops the old one
// before creating the new, and when the old one is the process doing it, the
// stop is the last thing that happens. Both hosts lost their updater that way.
//
// So a request naming the updater is handed to a helper -- this binary, as
// `update self-update`, in a container copied from the updater's own. To the
// helper the updater is an ordinary container, and the ordinary transaction
// replaces it. The queue is given the helper's stream, so it watches the work
// and reads its outcome from the process that actually did it.
//
// The helper does one thing. It serves no /v1/apply, takes no other request,
// and refuses a job naming anything but the updater that started it. When the
// job ends it exits: a success removes the container, and anything else leaves
// it behind, because its log is then the only record of why.

// Handoff is a helper that has been started and is listening.
type Handoff struct {
	// Name is the helper's container name, for logs and readiness reasons.
	Name string
	// Stream is where the queue reads the work from: absolute, because it is
	// the helper's and not this updater's.
	Stream string
	// Wait blocks until the helper exits. An error means it cannot be told.
	Wait func() error
}

const (
	// helperIdle is how long a helper waits for the queue to start watching.
	// Nobody arriving means the updater that started it did not get as far as
	// answering, and a job nobody watches is one whose outcome nobody learns.
	helperIdle = time.Minute
	// helperDrain is how long a finished helper waits for its watcher to read
	// the end. Exiting at once could cut the terminal line off.
	helperDrain = 30 * time.Second
	// helperListen is how long the updater waits for a helper to answer
	// before giving up on it.
	helperListen = 30 * time.Second
)

// isSelf reports whether a service's container is the one this runs in.
//
// An error is "no", which is safe: the errors here are the same ones that make
// the transaction fail before it stops anything.
func isSelf(service string) bool {
	self, err := dockerapi.Self()
	if err != nil {
		return false
	}
	id, err := dockerapi.ServiceContainer(service)
	return err == nil && id == self
}

// handOff starts a helper for a request naming this updater.
func handOff(req update.Request) (Handoff, error) {
	self, err := dockerapi.Self()
	if err != nil {
		return Handoff{}, err
	}
	job, err := json.Marshal(req)
	if err != nil {
		return Handoff{}, err
	}
	// Unique, so a failed helper left behind for its log does not stop the
	// next attempt from starting.
	name := dockerapi.ContainerName(req.Service) + "-self-update-" + time.Now().Format("20060102-150405")

	id, err := dockerapi.StartHelper(name, []string{"self-update", self, string(job)})
	if err != nil {
		return Handoff{}, err
	}

	// Answered only once it listens: the queue connects the moment it is
	// told where, and a refused connection there reads as a failed update.
	base := "http://" + name + addr
	if err := awaitListening(base+"/healthz", helperListen); err != nil {
		return Handoff{}, fmt.Errorf("%s did not start listening: %w", name, err)
	}
	return Handoff{
		Name:   name,
		Stream: base + "/v1/stream/" + req.Service,
		Wait: func() error {
			_, err := dockerapi.Wait(id)
			return err
		},
	}, nil
}

func awaitListening(url string, within time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(within)
	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("%s", resp.Status)
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// helper is a self-update's one job and the one stream it serves.
type helper struct {
	Request update.Request
	Apply   work
	Idle    time.Duration
	Drain   time.Duration
	Log     zerolog.Logger

	st        *stream
	once      sync.Once
	connected chan struct{}

	mu       sync.Mutex
	watchers int
}

func newHelper(req update.Request, apply work, log zerolog.Logger) *helper {
	return &helper{
		Request:   req,
		Apply:     apply,
		Idle:      helperIdle,
		Drain:     helperDrain,
		Log:       log,
		st:        newStream(),
		connected: make(chan struct{}),
	}
}

// Handler serves the stream and nothing else that does anything.
func (h *helper) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/v1/stream/", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.URL.Path, "/v1/stream/") != h.Request.Service {
			http.NotFound(w, r)
			return
		}
		h.mu.Lock()
		h.watchers++
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			h.watchers--
			h.mu.Unlock()
		}()
		h.once.Do(func() { close(h.connected) })
		serveStream(w, r, h.st)
	})
	return mux
}

// Run waits for a watcher, does the job, and waits for the watcher to read how
// it ended. ran is false when nobody came and nothing was done.
func (h *helper) Run() (status update.Status, ran bool) {
	select {
	case <-h.connected:
	case <-time.After(h.Idle):
		h.Log.Error().Msgf("%s: nobody started watching within %s; doing nothing", h.Request.Service, h.Idle)
		return "", false
	}

	status, reason := h.apply()
	h.st.write("%s", update.Terminal(status, reason))
	h.st.finish()
	h.Log.Info().Msgf("%s: %s %s", h.Request.Service, status, reason)

	deadline := time.Now().Add(h.Drain)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := h.watchers
		h.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return status, true
}

func (h *helper) apply() (status update.Status, reason string) {
	defer func() {
		if p := recover(); p != nil {
			status, reason = update.Failed, fmt.Sprintf("the self-update panicked: %v", p)
		}
	}()
	h.Log.Info().Msgf("%s: applying %s -> %s", h.Request.Service, h.Request.From, h.Request.To)
	return h.Apply(h.Request, h.st.write)
}

// selfUpdate is the helper's whole life: `update self-update <updater> <job>`.
//
// <updater> is the id of the container that started it, and the job must name
// that container's service. Anything else is refused before it is looked at:
// this mode exists to replace one container, and a helper that would replace
// any it was told to is a second updater nobody configured.
func selfUpdate(log zerolog.Logger, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: update self-update <updater container id> <request>")
	}
	updater := args[0]
	var req update.Request
	if err := json.Unmarshal([]byte(args[1]), &req); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	id, err := dockerapi.ServiceContainer(req.Service)
	if err != nil {
		return fmt.Errorf("finding %s: %w", req.Service, err)
	}
	if id != updater {
		return fmt.Errorf("refusing: %s is not the updater that started this helper", req.Service)
	}

	push, _, tmpl, err := setup()
	if err != nil {
		return err
	}

	h := newHelper(req, func(r update.Request, step func(string, ...any)) (update.Status, string) {
		return transaction(r, step, realDocker, realGit, push, tmpl)
	}, log)
	srv := &http.Server{Addr: addr, Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error().Msgf("stopped serving: %v", err)
		}
	}()

	status, ran := h.Run()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)

	switch {
	case !ran:
		return fmt.Errorf("nobody watched, so nothing was done")
	case status == update.Completed, status == update.Deferred:
		// Nothing to keep: the outcome reached the queue, and a deferral
		// touched nothing. Removing this container ends this process.
		if err := dockerapi.RemoveSelf(); err != nil {
			log.Warn().Msgf("could not remove this helper: %v", err)
		}
		return nil
	default:
		return fmt.Errorf("the self-update did not complete; this container is kept for its log")
	}
}
