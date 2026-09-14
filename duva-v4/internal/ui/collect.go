package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/decide"
)

// Collecting several deciders into one queue.
//
// Lifted from duva's internal/hub, which did the same job over agents. What
// changed is only the protocol: a decider's snapshot is its queue, and there
// is no soak list or refresh to fan out because the detector owns when a
// check happens.
//
// Deciders are configured, not discovered. At this scale a registration
// protocol would only add an inbound path by which something could claim to
// be a decider, to solve a problem a one-line config already solves.

// Decider is one configured decider.
type Decider struct {
	// Host names it, and is what rows from it are labelled with. The
	// configured name wins over whatever the decider calls itself: the UI
	// routes approvals by it, so it must mean the same thing in the config an
	// operator edits and in the row they click.
	Host string
	// URL is where it is, e.g. http://duva-decider:8080.
	URL string
	// Token is the bearer token it requires.
	Token string
}

// Collector fans out over deciders and implements what the page renders from.
type Collector struct {
	Deciders []Decider

	// HTTP is the transport. Nil uses a client with a timeout: a decider on
	// an unreachable host must fail collection in seconds, not hang the page
	// until someone notices it never loaded.
	HTTP *http.Client

	// MaxAge is how stale a cached collection may be before a read
	// re-collects. Zero means the package default.
	MaxAge time.Duration

	// OnUnreachable, when set, is called with each decider a collection could
	// not reach, every time one runs.
	//
	// On the collection rather than on a timer, because a read triggers one
	// too: an operator loading the page is exactly when a host being down
	// should be said out loud. A queue missing a host looks like good news.
	OnUnreachable func(host, err string)

	mu        sync.RWMutex
	cache     map[string]snapshot
	collected time.Time
}

// defaultMaxAge bounds how old the page's data can be.
//
// Short, because the cache exists to stop one render fanning out to every
// host, not to batch reads: a decision queued a moment ago must show up on
// the next reload, or an operator refreshing after a detector run concludes
// nothing was found.
const defaultMaxAge = 5 * time.Second

// snapshot is one decider's last answer, with whether it could be reached.
type snapshot struct {
	queue []decide.Entry
	// canApply is whether this decider has an actor. Per decider, not per
	// collector: one host can have an actor while another does not.
	canApply bool
	// blocker is which kind of thing is in the way, for styling; whyNot is
	// the words, which are the actor's and are never parsed.
	blocker string
	whyNot  string
	// err is the transport failure, when the decider could not be asked.
	//
	// Kept rather than dropped because an unreachable decider must render as
	// unreachable. A page that silently omits a host it cannot reach shows a
	// queue indistinguishable from one where that host has nothing waiting --
	// the exact failure a queue exists to prevent.
	err error
}

// NewCollector returns a collector over the given deciders.
func NewCollector(deciders []Decider) *Collector {
	return &Collector{Deciders: deciders, cache: map[string]snapshot{}}
}

func (c *Collector) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Collect asks every decider for its queue, concurrently, and caches the
// answers. Errors are recorded per decider rather than returned: one host
// being down is not a reason to have no queue.
func (c *Collector) Collect() {
	var wg sync.WaitGroup
	results := make([]snapshot, len(c.Deciders))
	for i, d := range c.Deciders {
		wg.Add(1)
		go func(i int, d Decider) {
			defer wg.Done()
			snap, err := c.fetch(d)
			results[i] = snapshot{queue: snap.Pending, canApply: snap.CanApply,
				blocker: string(snap.Blocker), whyNot: snap.WhyNot, err: err}
		}(i, d)
	}
	wg.Wait()

	c.mu.Lock()
	for i, d := range c.Deciders {
		// A failed collection keeps the last good queue rather than replacing
		// it with nothing. What was waiting a minute ago is still waiting --
		// the decider being unreachable is a fact about the decider, not
		// about its queue -- and showing an empty page would say the opposite
		// of the truth. The rows come back marked stale and unapplyable, so
		// nothing acts on them while they cannot be confirmed.
		if results[i].err != nil {
			prev := c.cache[d.Host]
			results[i].queue = prev.queue
			// Never applyable while unreachable, whatever the last answer
			// said: the route to the actor is through the decider.
			results[i].canApply = false
		}
		c.cache[d.Host] = results[i]
	}
	c.collected = time.Now()
	c.mu.Unlock()

	// Outside the lock: the callback is the caller's code, and one that read
	// the collector would deadlock against a lock still held here.
	if c.OnUnreachable != nil {
		for i, d := range c.Deciders {
			if results[i].err != nil {
				c.OnUnreachable(d.Host, results[i].err.Error())
			}
		}
	}
}

// fetch reads one decider's queue.
func (c *Collector) fetch(d Decider) (decide.Snapshot, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(d.URL, "/")+"/v1/snapshot", nil)
	if err != nil {
		return decide.Snapshot{}, err
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return decide.Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return decide.Snapshot{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var snap decide.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&snap); err != nil {
		return decide.Snapshot{}, fmt.Errorf("reading its queue: %w", err)
	}
	return snap, nil
}

// fresh collects if the cache is older than MaxAge. Called by every read, so
// the page is never served from data older than one MaxAge.
func (c *Collector) fresh() {
	c.mu.RLock()
	age := time.Since(c.collected)
	empty := c.collected.IsZero()
	c.mu.RUnlock()

	max := c.MaxAge
	if max == 0 {
		max = defaultMaxAge
	}
	if empty || age > max {
		c.Collect()
	}
}

// Hosted is one decider's queue entry, labelled with where it came from.
//
// The host is here rather than on decide.Entry because a decider has no
// opinion about which host it is: it answers for the one project it was given,
// and it is the UI over several of them that needs to tell two services of the
// same name apart. Putting it in the queue would mean a decider serving a
// field only something else uses.
type Hosted struct {
	decide.Entry
	// Stale marks a row from a decider that has since stopped answering: it
	// is what was queued when it last did, which may no longer be true.
	Stale bool
	// CanApply is whether this row's decider has an actor. Per row, because
	// one host can have an actor while another does not.
	CanApply bool
	Host     string
}

// Pending returns every decider's queue, labelled by host.
func (c *Collector) Pending() []Hosted {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []Hosted
	for _, d := range c.Deciders {
		s, ok := c.cache[d.Host]
		if !ok {
			continue
		}
		for _, e := range s.queue {
			// The configured host, not the reported one: see Decider.Host.
			out = append(out, Hosted{
				Entry: e, Host: d.Host,
				CanApply: s.canApply,
				// Shown, but known to be from before the decider stopped
				// answering. Rendering it as current would invite approving
				// something that may already be gone.
				Stale: s.err != nil,
			})
		}
	}
	// By host then service, so a row keeps its place between reloads however
	// the deciders happened to answer.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// Problem is a decider the UI could not reach.
type Problem struct {
	Host string
	Err  string
}

// Unreachable lists the deciders whose last collection failed, for the page
// to say so. Sorted, so the order does not change between reloads.
func (c *Collector) Unreachable() []Problem {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []Problem
	for _, d := range c.Deciders {
		if s, ok := c.cache[d.Host]; ok && s.err != nil {
			out = append(out, Problem{Host: d.Host, Err: s.err.Error()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Approve routes an approval to the decider that owns the service.
//
// Routed by an explicit host rather than by searching every queue: two hosts
// run services of the same name -- caddy, ofelia, cloudflared and restic are
// on both -- and a search would approve on whichever answered first.
func (c *Collector) Approve(key string) error {
	host, service, ok := SplitKey(key)
	if !ok {
		return fmt.Errorf("%q is not a host/service key", key)
	}
	for _, d := range c.Deciders {
		if d.Host != host {
			continue
		}
		req, err := http.NewRequest(http.MethodPost,
			strings.TrimSuffix(d.URL, "/")+"/v1/apply/"+url.PathEscape(service), nil)
		if err != nil {
			return err
		}
		if d.Token != "" {
			req.Header.Set("Authorization", "Bearer "+d.Token)
		}
		resp, err := c.client().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		// A refusal is 200 with a message: "nothing is queued for x" is an
		// answer about the queue, not a transport failure.
		var out struct {
			Message string `json:"message"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
		if out.Message != "" {
			return fmt.Errorf("%s", out.Message)
		}
		return nil
	}
	return fmt.Errorf("no decider named %q", host)
}

// Key identifies a service across hosts, as "host/service". The page sends it
// back on a click, and it is what Approve routes on.
func Key(host, service string) string { return host + "/" + service }

// SplitKey reverses Key. A service name cannot contain "/" (compose forbids
// it), so the first separator is the only one.
func SplitKey(key string) (host, service string, ok bool) {
	i := strings.IndexByte(key, '/')
	if i <= 0 || i >= len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// Blocked names the deciders that cannot apply right now, with why.
//
// Not only "has no actor": an actor that is there and would refuse -- the
// reference one refuses on an uncommitted repository, because applying
// commits -- blocks a click just as completely. The reason comes from the
// decider, which got it from the actor, and is relayed unchanged: a UI that
// composed this sentence would be one that had learned what applying means.
//
// Unreachable deciders are not included. Nothing is known about them, and
// listing one here would claim a reason where the truth is that it was not
// asked -- which Unreachable already says.
type Blocked struct {
	Host string `json:"host"`
	// Kind is "no-actor" or "actor-not-ready", for styling. A page tells
	// them apart by this and never by reading Reason.
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

func (c *Collector) Blocked() []Blocked {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []Blocked
	for _, d := range c.Deciders {
		if s, ok := c.cache[d.Host]; ok && s.err == nil && !s.canApply {
			out = append(out, Blocked{Host: d.Host, Kind: s.blocker, Reason: s.whyNot})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Stream relays one in-flight apply's progress from the decider that owns it.
//
// A relay rather than pointing the browser at the decider: the token that
// reaches a decider is the UI's, held server-side. Handing it to a page would
// put the authority to replace containers in every browser that loads the
// queue.
//
// Lines are copied through and flushed as they arrive, because that is the
// whole value of a stream -- buffering it would turn four steps arriving over
// a minute into one silent minute and then four lines at once.
func (c *Collector) Stream(key string, w http.ResponseWriter, r *http.Request) error {
	host, service, ok := SplitKey(key)
	if !ok {
		return fmt.Errorf("%q is not a host/service key", key)
	}
	for _, d := range c.Deciders {
		if d.Host != host {
			continue
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
			strings.TrimSuffix(d.URL, "/")+"/v1/stream/"+url.PathEscape(service), nil)
		if err != nil {
			return err
		}
		if d.Token != "" {
			req.Header.Set("Authorization", "Bearer "+d.Token)
		}
		// No timeout on this client: a stream is long-lived by design, and
		// the default 10s would cut every apply off mid-pull.
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s", resp.Status)
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 4<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return nil // the browser went away, which is not an error
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				return nil // including io.EOF: the stream ended, as it should
			}
		}
	}
	return fmt.Errorf("no decider named %q", host)
}
