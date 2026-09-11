// Package hub collects several agents into one queue.
//
// A hub holds no state and watches nothing. It asks each configured agent
// what it has found, labels the answers with which agent gave them, and
// serves the same page a single-host duva serves. Applying routes back to
// whichever agent owns the service, because that is the only place the docker
// socket and the compose file are.
//
// Agents are configured, not discovered. At the scale this runs at -- a
// handful of hosts on fixed addresses that change approximately never -- a
// registration protocol would solve a problem nobody has while adding the one
// thing that matters: an inbound path by which something could claim to be an
// agent. A list the hub is given cannot be joined by a stranger.
package hub

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/agent"
	"github.com/Miista/homebrew-docker-pin/internal/ui"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
)

// Agent is one configured agent.
type Agent struct {
	// Host names the agent, and is what rows from it are labelled with. The
	// configured name wins over whatever the agent calls itself: the hub
	// routes by it, so it must mean the same thing in the config the
	// operator edits and in the row they click.
	Host string
	// Client talks to it.
	Client *agent.Client
}

// Hub fans out over agents. It implements ui.Source, ui.Applier and
// ui.Refresher, so the page cannot tell it is not talking to a local duva.
type Hub struct {
	Agents []Agent

	// MaxAge is how stale a cached collection may be before a read
	// re-collects. Zero means the package default.
	MaxAge time.Duration

	// OnUnreachable, when set, is called with each agent a collection could
	// not reach, every time one runs.
	//
	// On the collection rather than on a timer because a read can trigger a
	// collection too: an operator loading the page is exactly when a host
	// being down should be said out loud, and a hub whose only report came
	// from its own ticker stayed silent for as long as a poll interval
	// while showing a queue with a host missing from it.
	OnUnreachable func(host, err string)

	// mu guards the cached snapshots. The page renders from whatever the
	// last collection found, rather than calling every agent on every
	// request: a hub serving a page should not be as slow as its slowest
	// host, and a reload is not a reason to re-ask a machine that answered a
	// second ago.
	mu    sync.RWMutex
	cache map[string]snapshot
	// collected is when the cache was last filled, for MaxAge.
	collected time.Time
}

// defaultMaxAge bounds how old the page's data can be.
//
// Short, because the cache exists to stop one render fanning out to every
// host, not to batch reads: an agent that has just finished a check must show
// up on the next reload, or an operator refreshing after an update sees the
// state before it and concludes nothing happened.
const defaultMaxAge = 2 * time.Second

// fresh collects if the cache is older than MaxAge. Called by every read, so
// the page is never served from data older than one MaxAge.
func (h *Hub) fresh() {
	h.mu.RLock()
	age := time.Since(h.collected)
	empty := h.collected.IsZero()
	h.mu.RUnlock()

	max := h.MaxAge
	if max == 0 {
		max = defaultMaxAge
	}
	if empty || age > max {
		h.Collect()
	}
}

// snapshot is one agent's last answer, with whether it could be reached.
type snapshot struct {
	snap agent.Snapshot
	// err is the transport failure, when the agent could not be asked.
	//
	// Kept rather than dropped because an unreachable agent must render as
	// unreachable. A hub that silently omits a host it cannot reach shows a
	// queue indistinguishable from one where that host simply has no
	// updates -- which is the exact failure a queue exists to prevent.
	err error
}

// New returns a hub over the given agents.
func New(agents []Agent) *Hub {
	return &Hub{Agents: agents, cache: map[string]snapshot{}}
}

// Collect asks every agent for its snapshot, concurrently, and caches the
// answers. Errors are recorded per agent rather than returned: one host being
// down is not a reason to have no queue.
func (h *Hub) Collect() {
	var wg sync.WaitGroup
	results := make([]snapshot, len(h.Agents))
	for i, a := range h.Agents {
		wg.Add(1)
		go func(i int, a Agent) {
			defer wg.Done()
			s, err := a.Client.Snapshot()
			results[i] = snapshot{snap: s, err: err}
		}(i, a)
	}
	wg.Wait()

	h.mu.Lock()
	for i, a := range h.Agents {
		h.cache[a.Host] = results[i]
	}
	h.collected = time.Now()
	h.mu.Unlock()

	// Outside the lock: the callback is the caller's code, and one that
	// read the hub -- to render the problem next to the queue, say -- would
	// deadlock against a lock still held here.
	if h.OnUnreachable != nil {
		for i, a := range h.Agents {
			if results[i].err != nil {
				h.OnUnreachable(a.Host, results[i].err.Error())
			}
		}
	}
}

// Unreachable lists the agents whose last collection failed, for the page to
// say so. Sorted, so the order does not change between reloads.
func (h *Hub) Unreachable() []Problem {
	h.fresh()
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []Problem
	for _, a := range h.Agents {
		if s, ok := h.cache[a.Host]; ok && s.err != nil {
			out = append(out, Problem{Host: a.Host, Err: s.err.Error()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Problem is an agent the hub could not reach.
type Problem struct {
	Host string
	Err  string
}

// Pending returns every agent's queue, labelled by host.
func (h *Hub) Pending() []watch.Pending {
	h.fresh()
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []watch.Pending
	for _, a := range h.Agents {
		s, ok := h.cache[a.Host]
		if !ok || s.err != nil {
			continue
		}
		for _, p := range s.snap.Pending {
			// The configured host, not the reported one: see Agent.Host.
			p.Host = a.Host
			out = append(out, p)
		}
	}
	sortRows(out, func(p watch.Pending) (string, string) { return p.Host, p.Service })
	return out
}

// Soaking returns every agent's soak list, labelled by host.
func (h *Hub) Soaking() []watch.Soaking {
	h.fresh()
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []watch.Soaking
	for _, a := range h.Agents {
		s, ok := h.cache[a.Host]
		if !ok || s.err != nil {
			continue
		}
		for _, sk := range s.snap.Soaking {
			sk.Host = a.Host
			out = append(out, sk)
		}
	}
	sortRows(out, func(s watch.Soaking) (string, string) { return s.Host, s.Service })
	return out
}

// sortRows orders by host then service, so a row keeps its place between
// reloads however the agents happened to answer.
func sortRows[T any](rows []T, key func(T) (string, string)) {
	sort.Slice(rows, func(i, j int) bool {
		hi, si := key(rows[i])
		hj, sj := key(rows[j])
		if hi != hj {
			return hi < hj
		}
		return si < sj
	})
}

// LastCheck is the oldest of the agents' last checks.
//
// The oldest rather than the newest: the footer answers "is what I am looking
// at recent", and a queue is only as current as its stalest source. Reporting
// the newest would let one agent checking on time hide another that has not
// checked in a week.
func (h *Hub) LastCheck() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	oldest, age := "", ""
	for _, a := range h.Agents {
		s, ok := h.cache[a.Host]
		if !ok || s.err != nil || s.snap.LastCheck == "" {
			continue
		}
		// Ordered by the exact timestamp, not the rendered age: "2 hours
		// ago" and "3 days ago" do not compare as strings. RFC 3339 does.
		if oldest == "" || s.snap.LastCheckExact < oldest {
			oldest, age = s.snap.LastCheckExact, s.snap.LastCheck
		}
	}
	if age == "" {
		return "never"
	}
	return age
}

// LastCheckExact is the same moment as a timestamp.
func (h *Hub) LastCheckExact() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	worst := ""
	for _, a := range h.Agents {
		s, ok := h.cache[a.Host]
		if !ok || s.err != nil || s.snap.LastCheckExact == "" {
			continue
		}
		if worst == "" || s.snap.LastCheckExact < worst {
			worst = s.snap.LastCheckExact
		}
	}
	return worst
}

// find returns the agent owning a service, by the "host/service" key the page
// sends back. Routing by an explicit host rather than by searching every
// agent's queue: two hosts run services of the same name -- caddy, ofelia,
// cloudflared, restic are on both -- and a search would apply to whichever
// answered first.
func (h *Hub) find(key string) (Agent, string, error) {
	host, service, ok := SplitKey(key)
	if !ok {
		return Agent{}, "", fmt.Errorf("%q is not a host/service key", key)
	}
	for _, a := range h.Agents {
		if a.Host == host {
			return a, service, nil
		}
	}
	return Agent{}, "", fmt.Errorf("no agent named %q", host)
}

// Start routes an apply to the agent that owns the service.
func (h *Hub) Start(key string) error {
	a, service, err := h.find(key)
	if err != nil {
		return err
	}
	return a.Client.Apply(service)
}

// Progress polls the owning agent.
func (h *Hub) Progress(key string) (ui.Progress, bool) {
	a, service, err := h.find(key)
	if err != nil {
		return ui.Progress{}, false
	}
	resp, ok := a.Client.Progress(service)
	return resp.Progress, ok
}

// Refresh asks every agent to check now.
//
// All of them, not one: the button means "make what I am looking at current",
// and a hub's page is every host at once. An error from any agent is
// returned, but the others are still asked -- one unreachable host should not
// stop the rest from refreshing.
func (h *Hub) Refresh() error {
	var wg sync.WaitGroup
	errs := make([]error, len(h.Agents))
	for i, a := range h.Agents {
		wg.Add(1)
		go func(i int, a Agent) {
			defer wg.Done()
			errs[i] = a.Client.Refresh()
		}(i, a)
	}
	wg.Wait()

	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", h.Agents[i].Host, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s", joinLines(failed))
	}
	return nil
}

// RefreshProgress reports the fan-out as one operation: still running while
// any agent is, done when all are, and carrying whatever failed.
func (h *Hub) RefreshProgress() (running, done bool, errMsg string) {
	var wg sync.WaitGroup
	type res struct {
		running, done bool
		err           string
	}
	out := make([]res, len(h.Agents))
	for i, a := range h.Agents {
		wg.Add(1)
		go func(i int, a Agent) {
			defer wg.Done()
			r, d, e := a.Client.RefreshProgress()
			out[i] = res{r, d, e}
		}(i, a)
	}
	wg.Wait()

	anyRunning, allDone := false, true
	var errs []string
	for i, r := range out {
		if r.running {
			anyRunning = true
		}
		if !r.done {
			allDone = false
		}
		if r.err != "" {
			errs = append(errs, fmt.Sprintf("%s: %s", h.Agents[i].Host, r.err))
		}
	}
	if len(h.Agents) == 0 {
		return false, false, ""
	}
	// Once every agent has finished, their findings are newer than the
	// hub's cache -- they just checked, and the hub has not asked since.
	// Re-collect before reporting done, so the page the poller reloads on
	// shows the result of the refresh rather than the state before it.
	// Collect takes the write lock itself; nothing is held here.
	if allDone && !anyRunning {
		h.Collect()
	}
	return anyRunning, allDone, joinLines(errs)
}

func joinLines(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += "; "
		}
		out += v
	}
	return out
}

// Key identifies a service across hosts, as "host/service". The page sends it
// back on a click, and it is what Start and Progress route on.
func Key(host, service string) string { return host + "/" + service }

// SplitKey reverses Key. A service name cannot contain "/" (compose forbids
// it), so the first separator is the only one.
func SplitKey(key string) (host, service string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], i > 0 && i < len(key)-1
		}
	}
	return "", "", false
}
