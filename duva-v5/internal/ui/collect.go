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

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/jobs"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
)

// Collecting several queues into one queue.
//
// Lifted from duva's internal/hub, which did the same job over agents. What
// changed is only the protocol: a queue's snapshot is its queue, and there
// is no soak list or refresh to fan out because the watcher owns when a
// check happens.
//
// Queues are configured, not discovered. At this scale a registration
// protocol would only add an inbound path by which something could claim to
// be a queue, to solve a problem a one-line config already solves.

// Upstream is one queue this UI serves, and where to reach it.
type Upstream struct {
	// Host names it, and is what rows from it are labelled with. The
	// configured name wins over whatever the queue calls itself: the UI
	// routes approvals by it, so it must mean the same thing in the config an
	// operator edits and in the row they click.
	Host string
	// URL is where it is, e.g. http://duva-queue:8080.
	URL string
	// Token is the bearer token it requires.
	Token string
	// Local is the queue in this process, when it is. Set, it is asked
	// directly and URL and Token are not used: the queue on the UI's own
	// host is a call away, not a request.
	Local LocalQueue
}

// LocalQueue is a queue in the UI's own process: the three things the UI
// asks of a queue, answered without HTTP.
type LocalQueue interface {
	Snapshot() queue.Snapshot
	// Approve starts an apply on someone's behalf; the answer is a message,
	// empty on success. ApproveEntry is the same for one entry by its row.
	Approve(service, actor string) string
	ApproveEntry(id int64, actor string) string
	Stream(service string, w http.ResponseWriter, r *http.Request) error
	// The other actions, each answering with a message, empty on success.
	Discard(id int64, actor string) string
	Block(id int64, reason, actor string) string
	CheckNow(service, actor string, back time.Duration) string
	Unblock(image, digest, actor string) string
	CheckAll(actor string, back time.Duration) string
	// RunJob runs one of the host's jobs now; the answer is a message,
	// empty on success.
	RunJob(job, actor string) string
}

// Collector fans out over queues and implements what the page renders from.
type Collector struct {
	Upstreams []Upstream

	// HTTP is the transport. Nil uses a client with a timeout: a queue on
	// an unreachable host must fail collection in seconds, not hang the page
	// until someone notices it never loaded.
	HTTP *http.Client

	// MaxAge is how stale a cached collection may be before a read
	// re-collects. Zero means the package default.
	MaxAge time.Duration

	// OnUnreachable, when set, is called with each queue a collection could
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
// the next reload, or an operator refreshing after a watcher run concludes
// nothing was found.
const defaultMaxAge = 5 * time.Second

// snapshot is one queue's last answer, with whether it could be reached.
type snapshot struct {
	queue []queue.Entry
	// soaking is what that queue holds waiting out a soak.
	soaking []queue.Entry
	// upcoming is what it holds that comes to a person when the delay is over.
	upcoming []queue.Entry
	// blocks are the releases blocked on that host.
	blocks []queue.Block
	// lastCheck is when that host last looked for new tags, RFC 3339.
	lastCheck string
	// services is every service that host watches, for "Check…".
	services []string
	// canApply is whether this queue has an update. Per queue, not per
	// collector: one host can have an updater while another does not.
	canApply bool
	// blocker is which kind of thing is in the way, for styling; whyNot is
	// the words, which are the updater's and are never parsed.
	blocker string
	whyNot  string
	// applying is the services this queue is applying right now.
	applying map[string]bool
	// jobs is what that host runs on its own, with each one's last run.
	jobs []jobs.Status
	// ignored is that host's services labelled duva.ignore: true.
	ignored []queue.IgnoredService
	// run is the latest check asked of that host, nil before the first.
	run *queue.CheckRun
	// health is that host's own, as it reported it.
	health queue.Health
	// err is the transport failure, when the queue could not be asked.
	//
	// Kept rather than dropped because an unreachable queue must render as
	// unreachable. A page that silently omits a host it cannot reach shows a
	// queue indistinguishable from one where that host has nothing waiting --
	// the exact failure a queue exists to prevent.
	err error
}

// NewCollector returns a collector over the given queues.
func NewCollector(queues []Upstream) *Collector {
	return &Collector{Upstreams: queues, cache: map[string]snapshot{}}
}

func (c *Collector) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Collect asks every queue for its queue, concurrently, and caches the
// answers. Errors are recorded per queue rather than returned: one host
// being down is not a reason to have no queue.
func (c *Collector) Collect() {
	var wg sync.WaitGroup
	results := make([]snapshot, len(c.Upstreams))
	for i, d := range c.Upstreams {
		wg.Add(1)
		go func(i int, d Upstream) {
			defer wg.Done()
			snap, err := c.fetch(d)
			applying := map[string]bool{}
			for _, svc := range snap.Applying {
				applying[svc] = true
			}
			results[i] = snapshot{queue: snap.Pending, soaking: snap.Soaking, upcoming: snap.Upcoming, blocks: snap.Blocks,
				lastCheck: snap.LastCheck, services: snap.Services, canApply: snap.CanApply,
				blocker: string(snap.Blocker), whyNot: snap.WhyNot,
				applying: applying, jobs: snap.Jobs, ignored: snap.Ignored, run: snap.Check, health: snap.Health, err: err}
		}(i, d)
	}
	wg.Wait()

	c.mu.Lock()
	for i, d := range c.Upstreams {
		// A failed collection keeps the last good queue rather than replacing
		// it with nothing. What was waiting a minute ago is still waiting --
		// the queue being unreachable is a fact about the queue, not
		// about its queue -- and showing an empty page would say the opposite
		// of the truth. The rows come back marked stale and unapplyable, so
		// nothing acts on them while they cannot be confirmed.
		if results[i].err != nil {
			prev := c.cache[d.Host]
			results[i].queue = prev.queue
			results[i].soaking = prev.soaking
			results[i].upcoming = prev.upcoming
			results[i].blocks = prev.blocks
			results[i].lastCheck = prev.lastCheck
			results[i].services = prev.services
			results[i].jobs = prev.jobs
			results[i].ignored = prev.ignored
			results[i].run = prev.run
			// Never applyable while unreachable, whatever the last answer
			// said: the route to the updater is through the queue.
			results[i].canApply = false
		}
		c.cache[d.Host] = results[i]
	}
	c.collected = time.Now()
	c.mu.Unlock()

	// Outside the lock: the callback is the caller's code, and one that read
	// the collector would deadlock against a lock still held here.
	if c.OnUnreachable != nil {
		for i, d := range c.Upstreams {
			if results[i].err != nil {
				c.OnUnreachable(d.Host, results[i].err.Error())
			}
		}
	}
}

// fetch reads one upstream's queue.
func (c *Collector) fetch(d Upstream) (queue.Snapshot, error) {
	if d.Local != nil {
		return d.Local.Snapshot(), nil
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(d.URL, "/")+"/v1/snapshot", nil)
	if err != nil {
		return queue.Snapshot{}, err
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return queue.Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return queue.Snapshot{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var snap queue.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&snap); err != nil {
		return queue.Snapshot{}, fmt.Errorf("reading its queue: %w", err)
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

// stale makes the next read collect, whatever the cache's age.
func (c *Collector) stale() {
	c.mu.Lock()
	c.collected = time.Time{}
	c.mu.Unlock()
}

// Hosted is one entry, labelled with the host whose queue it came from.
//
// The host is here rather than on queue.Entry because a queue has no
// opinion about which host it is: it answers for the one project it was given,
// and it is the UI over several of them that needs to tell two services of the
// same name apart. Putting it in the queue would mean a queue serving a
// field only something else uses.
type Hosted struct {
	queue.Entry
	// Stale marks a row from a queue that has since stopped answering: it
	// is what was queued when it last did, which may no longer be true.
	Stale bool
	// CanApply is whether this row's queue has an update. Per row, because
	// one host can have an updater while another does not.
	CanApply bool
	// Applying is whether the queue has this service's apply in flight, so a
	// page that did not start it can still show it.
	Applying bool
	Host     string
}

// Pending returns every host's entries, labelled by host.
func (c *Collector) Pending() []Hosted {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []Hosted
	for _, d := range c.Upstreams {
		s, ok := c.cache[d.Host]
		if !ok {
			continue
		}
		for _, e := range s.queue {
			// The configured host, not the reported one: see Queue.Host.
			out = append(out, Hosted{
				Entry: e, Host: d.Host,
				CanApply: s.canApply,
				Applying: s.applying[e.Service] && s.err == nil,
				// Shown, but known to be from before the queue stopped
				// answering. Rendering it as current would invite approving
				// something that may already be gone.
				Stale: s.err != nil,
			})
		}
	}
	// By host then service, so a row keeps its place between reloads however
	// the queues happened to answer.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Service < out[j].Service
	})
	return out
}

// Problem is a queue the UI could not reach.
type Problem struct {
	Host string
	Err  string
}

// Unreachable lists the queues whose last collection failed, for the page
// to say so. Sorted, so the order does not change between reloads.
func (c *Collector) Unreachable() []Problem {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []Problem
	for _, d := range c.Upstreams {
		if s, ok := c.cache[d.Host]; ok && s.err != nil {
			out = append(out, Problem{Host: d.Host, Err: why(s.err)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Health is the fleet's, for the hub's /healthz: its own host's health as it
// is, and every other host's at most degraded, named.
//
// Only this host's own trouble is unhealthy. The 503 that says so is what a
// container runtime restarts this process on, and restarting the hub mends
// nothing on an agent -- an agent unreachable, or unhealthy itself, is worth
// a look from here, and its own /healthz is what restarts it.
func (c *Collector) Health() queue.Health {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()
	h := queue.Health{Status: queue.HealthOK}
	for _, d := range c.Upstreams {
		s, ok := c.cache[d.Host]
		if !ok {
			continue
		}
		if s.err != nil {
			h.Status = queue.Worse(h.Status, queue.HealthDegraded)
			h.Reasons = append(h.Reasons, d.Host+": not answering: "+why(s.err))
			continue
		}
		level := s.health.Status
		if d.Local == nil && level == queue.HealthUnhealthy {
			level = queue.HealthDegraded
		}
		h.Status = queue.Worse(h.Status, level)
		for _, r := range s.health.Reasons {
			h.Reasons = append(h.Reasons, d.Host+": "+r)
		}
	}
	return h
}

// Approve routes an approval to the queue that owns the service.
//
// Routed by an explicit host rather than by searching every queue: two hosts
// run services of the same name -- caddy, ofelia, cloudflared and restic are
// on both -- and a search would approve on whichever answered first.
func (c *Collector) Approve(key string, entry int64, actor string) error {
	host, service, ok := SplitKey(key)
	if !ok {
		return fmt.Errorf("%q is not a host/service key", key)
	}
	for _, d := range c.Upstreams {
		if d.Host != host {
			continue
		}
		if d.Local != nil {
			msg := ""
			if entry > 0 {
				msg = d.Local.ApproveEntry(entry, actor)
			} else {
				msg = d.Local.Approve(service, actor)
			}
			if msg != "" {
				return fmt.Errorf("%s", msg)
			}
			return nil
		}
		req, err := http.NewRequest(http.MethodPost,
			strings.TrimSuffix(d.URL, "/")+"/v1/apply/"+url.PathEscape(service)+entryQuery(entry), nil)
		if err != nil {
			return err
		}
		if d.Token != "" {
			req.Header.Set("Authorization", "Bearer "+d.Token)
		}
		// Who, for the other host's history. The token is what that queue
		// trusts; this is only the record of whom it was for.
		if actor != "" {
			req.Header.Set("X-Duva-Actor", actor)
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
	return fmt.Errorf("no queue named %q", host)
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

// Blocked names the queues that cannot apply right now, with why.
//
// Not only "has no updater": an updater that is there and would refuse -- the
// reference one refuses on an uncommitted repository, because applying
// commits -- blocks a click just as completely. The reason comes from the
// queue, which got it from the updater, and is relayed unchanged: a UI that
// composed this sentence would be one that had learned what applying means.
//
// Unreachable queues are not included. Nothing is known about them, and
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
	for _, d := range c.Upstreams {
		if s, ok := c.cache[d.Host]; ok && s.err == nil && !s.canApply {
			out = append(out, Blocked{Host: d.Host, Kind: s.blocker, Reason: s.whyNot})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Stream relays one in-flight apply's progress from the queue that owns it.
//
// A relay rather than pointing the browser at the queue: the token that
// reaches a queue is the UI's, held server-side. Handing it to a page would
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
	// A stream ends when the apply has settled its entry, and the page asks
	// again the moment it does. Served from a cache taken mid-apply, that
	// answer still had the row, which then sat there until the next poll.
	defer c.stale()
	for _, d := range c.Upstreams {
		if d.Host != host {
			continue
		}
		if d.Local != nil {
			return d.Local.Stream(service, w, r)
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
	return fmt.Errorf("no queue named %q", host)
}

// why turns a fetch failure into something worth reading.
//
// The raw error is a Go transport error -- the method, the full URL, the
// resolver's address and port -- none of which is what a person needs. What
// they need is which of four things went wrong, because the answer differs:
// the queue is down, the name does not resolve, the token is wrong, or it
// answered with something unexpected.
//
// The original is not discarded; it goes to the log, where someone debugging
// can have all of it. This is the sentence on the page.
func why(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "no such host"):
		// Almost always a queue that is not running, since these are container
		// names on a compose network: the name exists while the container
		// does.
		return "not running, or its name is wrong"
	case strings.Contains(s, "connection refused"):
		return "not listening on that port"
	case strings.Contains(s, "Client.Timeout") || strings.Contains(s, "context deadline exceeded"):
		return "did not answer in time"
	case strings.Contains(s, "401") || strings.Contains(s, "403"):
		return "refused the token"
	case strings.Contains(s, "no route to host") || strings.Contains(s, "network is unreachable"):
		return "cannot be reached from here"
	}
	// Anything else, trimmed of the URL prefix Go puts on transport errors:
	// `Get "http://...": the part that matters`.
	if i := strings.Index(s, `": `); i != -1 {
		return s[i+3:]
	}
	return s
}

// Discard drops an entry on host for now.
func (c *Collector) Discard(host string, id int64, actor string) error {
	return c.act(host, actor,
		func(l LocalQueue) string { return l.Discard(id, actor) },
		fmt.Sprintf("/v1/discard/%d", id), nil)
}

// Block blocks the release an entry on host offers. The reason is required:
// it is what the next person to see the block is told.
func (c *Collector) Block(host string, id int64, reason, actor string) error {
	return c.act(host, actor,
		func(l LocalQueue) string { return l.Block(id, reason, actor) },
		fmt.Sprintf("/v1/block/%d", id), url.Values{"reason": {reason}})
}

// Check asks host's watcher to look at a service now, at least back far back
// (zero: since its last check).
func (c *Collector) Check(host, service, actor string, back time.Duration) error {
	return c.act(host, actor,
		func(l LocalQueue) string { return l.CheckNow(service, actor, back) },
		"/v1/check/"+url.PathEscape(service), sinceForm(back))
}

// RunJob runs one of the jobs now on every host. Every host is asked,
// whatever another answers, and none is waited for: each runs it at its own
// pace. The answer is each refusal, by host.
func (c *Collector) RunJob(job, actor string) error {
	var refused []string
	for _, d := range c.Upstreams {
		if err := c.act(d.Host, actor,
			func(l LocalQueue) string { return l.RunJob(job, actor) },
			"/v1/run/"+url.PathEscape(job), nil); err != nil {
			refused = append(refused, d.Host+": "+err.Error())
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("%s", strings.Join(refused, "; "))
	}
	return nil
}

// sinceForm is a look-back as the queue API takes it: nothing for "since the
// last check".
func sinceForm(back time.Duration) url.Values {
	if back <= 0 {
		return nil
	}
	return url.Values{"since": {watch.FormatLookBack(back)}}
}

// act routes one action to the queue that owns it: a call for the queue in
// this process, the queue API for one on another host. Either way the answer
// is a message for a person, and an empty one is success.
func (c *Collector) act(host, actor string, local func(LocalQueue) string, path string, form url.Values) error {
	for _, d := range c.Upstreams {
		if d.Host != host {
			continue
		}
		if d.Local != nil {
			if msg := local(d.Local); msg != "" {
				return fmt.Errorf("%s", msg)
			}
			return nil
		}
		req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(d.URL, "/")+path,
			strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if d.Token != "" {
			req.Header.Set("Authorization", "Bearer "+d.Token)
		}
		if actor != "" {
			req.Header.Set("X-Duva-Actor", actor)
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
		var out struct {
			Message string `json:"message"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
		if out.Message != "" {
			return fmt.Errorf("%s", out.Message)
		}
		return nil
	}
	return fmt.Errorf("no queue named %q", host)
}

// entryQuery names one entry on an apply, when the page said which.
func entryQuery(entry int64) string {
	if entry <= 0 {
		return ""
	}
	return fmt.Sprintf("?entry=%d", entry)
}

// Soaking returns what every queue holds waiting out a soak, labelled by
// host, the soonest released first.
func (c *Collector) Soaking() []Hosted {
	return c.delayed(func(s snapshot) []queue.Entry { return s.soaking })
}

// Upcoming returns what every queue holds that comes to a person when its
// delay is over, labelled by host, the soonest first.
func (c *Collector) Upcoming() []Hosted {
	return c.delayed(func(s snapshot) []queue.Entry { return s.upcoming })
}

func (c *Collector) delayed(of func(snapshot) []queue.Entry) []Hosted {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Hosted
	for _, d := range c.Upstreams {
		s, ok := c.cache[d.Host]
		if !ok {
			continue
		}
		for _, e := range of(s) {
			out = append(out, Hosted{Entry: e, Host: d.Host, CanApply: s.canApply,
				Applying: s.applying[e.Service] && s.err == nil, Stale: s.err != nil})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ReleaseAt < out[j].ReleaseAt })
	return out
}

// HostedBlock is a standing block, labelled with the host it is on.
type HostedBlock struct {
	queue.Block
	Host string `json:"host"`
}

// Blocks returns every host's standing blocks.
func (c *Collector) Blocks() []HostedBlock {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := []HostedBlock{}
	for _, d := range c.Upstreams {
		for _, b := range c.cache[d.Host].blocks {
			out = append(out, HostedBlock{Block: b, Host: d.Host})
		}
	}
	return out
}

// Unblock lifts a block on host.
func (c *Collector) Unblock(host, image, digest, actor string) error {
	return c.act(host, actor,
		func(l LocalQueue) string { return l.Unblock(image, digest, actor) },
		"/v1/unblock", url.Values{"image": {image}, "digest": {digest}})
}

// HostCheck is when one host last looked for new tags.
type HostCheck struct {
	Host string `json:"host"`
	// At is RFC 3339; empty before that host's first check, or while it
	// has never answered.
	At string `json:"at"`
	// Services is every service that host watches, by compose name, for
	// "Check…" to offer: what is waiting or not.
	Services []string `json:"services"`
	// Jobs is what that host runs on its own, with each one's last run.
	Jobs []jobs.Status `json:"jobs"`
	// Ignored is that host's services labelled duva.ignore: true. Listed
	// only: the label is the one place to change it.
	Ignored []queue.IgnoredService `json:"ignored"`
	// Run is the latest check asked of that host, for the page to say what
	// it found. Null before the first.
	Run *queue.CheckRun `json:"run"`
}

// LastChecks is each host's last check, and what it watches, in the
// configured order.
func (c *Collector) LastChecks() []HostCheck {
	c.fresh()
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]HostCheck, 0, len(c.Upstreams))
	for _, d := range c.Upstreams {
		s := c.cache[d.Host]
		services := s.services
		if services == nil {
			services = []string{}
		}
		js := s.jobs
		if js == nil {
			js = []jobs.Status{}
		}
		ignored := s.ignored
		if ignored == nil {
			ignored = []queue.IgnoredService{}
		}
		out = append(out, HostCheck{Host: d.Host, At: s.lastCheck, Services: services, Jobs: js, Ignored: ignored, Run: s.run})
	}
	return out
}

// CheckAll asks every host to look for new tags now, at least back far back
// (zero: since each service's last check). Every host is asked, whatever
// another answers; the answer is each refusal, by host.
func (c *Collector) CheckAll(actor string, back time.Duration) error {
	var refused []string
	for _, d := range c.Upstreams {
		if err := c.act(d.Host, actor,
			func(l LocalQueue) string { return l.CheckAll(actor, back) },
			"/v1/check-all", sinceForm(back)); err != nil {
			refused = append(refused, d.Host+": "+err.Error())
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("%s", strings.Join(refused, "; "))
	}
	return nil
}
