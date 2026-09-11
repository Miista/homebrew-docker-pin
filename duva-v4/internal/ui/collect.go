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
			q, err := c.fetch(d)
			results[i] = snapshot{queue: q, err: err}
		}(i, d)
	}
	wg.Wait()

	c.mu.Lock()
	for i, d := range c.Deciders {
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
func (c *Collector) fetch(d Decider) ([]decide.Entry, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(d.URL, "/")+"/v1/snapshot", nil)
	if err != nil {
		return nil, err
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var snap decide.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&snap); err != nil {
		return nil, fmt.Errorf("reading its queue: %w", err)
	}
	return snap.Pending, nil
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
	Host string
}

// Pending returns every decider's queue, labelled by host.
func (c *Collector) Pending() []Hosted {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []Hosted
	for _, d := range c.Deciders {
		s, ok := c.cache[d.Host]
		if !ok || s.err != nil {
			continue
		}
		for _, e := range s.queue {
			// The configured host, not the reported one: see Decider.Host.
			out = append(out, Hosted{Entry: e, Host: d.Host})
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
