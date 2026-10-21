package queue

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Applier is whatever carries out a decision.
//
// An interface, not the updater client, so the queue can be tested without
// one and so a caller can wrap it -- with a lock, with logging, with nothing.
// The queue does not know what applying means; see internal/update.
type Applier interface {
	// Start begins applying an entry. An error means it never began.
	Start(e Entry) error
	// StartUnattended is Start for work the queue decided on itself rather
	// than a person asked for. The difference is what a deferral does: an
	// unattended apply is offered again later, a manual one is not.
	StartUnattended(e Entry) error
	// Stream writes an in-flight apply's progress to w, for as long as it
	// runs. It writes nothing when the service has nothing in progress,
	// which is the whole of its error handling.
	Stream(service string, w http.ResponseWriter, r *http.Request)
}

// Server is the queue's HTTP surface.
//
// One audience: a UI, possibly on another host, reading the queue and
// approving from it. The watcher is in the same process and calls the
// handler directly; v4's open /v1/notify webhook is gone, and with it the
// one route that answered without a token.
type Server struct {
	// Queue is what is waiting.
	Queue *Pending
	// Applier carries out an approval.
	Applier Applier
	// Host is what this queue calls itself, for a UI showing several.
	Host string
	// Version is the build.
	Version string
	// Token guards the endpoints a UI uses. Empty leaves them open.
	Token string

	// Check and Rescan ask the watcher to look at one service now; Rescan
	// forgets its cutoff first. Nil: the action is not offered.
	Check, Rescan func(service string) error
	// Resolve is what a tag points at now, for blocking a version change
	// whose digest the queue has not learned. Nil: only entries that carry
	// a digest can be blocked.
	Resolve func(image, tag string) (string, error)
	// LastChecked is when this host last looked for new tags. Nil: not
	// reported.
	LastChecked func() time.Time

	cooldownMu sync.Mutex
	lastAsked  map[string]time.Time
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Open: the thing most likely to call it is a container runtime, which
	// carries no credential.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/v1/snapshot", s.guard(s.snapshot))
	mux.HandleFunc("/v1/apply/", s.guard(s.apply))
	mux.HandleFunc("/v1/stream/", s.guard(s.stream))
	mux.HandleFunc("/v1/discard/", s.guard(s.post("/v1/discard/", s.discardBy)))
	mux.HandleFunc("/v1/block/", s.guard(s.post("/v1/block/", s.blockBy)))
	mux.HandleFunc("/v1/unblock", s.guard(s.post("/v1/unblock", s.unblockBy)))
	mux.HandleFunc("/v1/check/", s.guard(s.post("/v1/check/", s.checkBy)))
	mux.HandleFunc("/v1/rescan/", s.guard(s.post("/v1/rescan/", s.rescanBy)))
	mux.HandleFunc("/v1/check-all", s.guard(s.post("/v1/check-all", func(_ string, r *http.Request) string {
		return s.CheckAll(actorOf(r))
	})))
	return mux
}

func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			// Constant time: the queue API is reachable from another host,
			// and a comparison that returns at the first wrong byte says how
			// many were right.
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// Snapshot is what a UI renders.
type Snapshot struct {
	Host    string `json:"host"`
	Version string `json:"version"`
	// Pending is what waits for a person, newest per service.
	Pending []Entry `json:"pending"`
	// Soaking is what applies itself when its soak runs out, every entry.
	Soaking []Entry `json:"soaking"`
	// Blocks are the releases a person blocked, still standing.
	Blocks []Block `json:"blocks"`
	// LastCheck is when this host last looked for new tags, RFC 3339; empty
	// before its first check.
	LastCheck string `json:"last_check,omitempty"`
	// CanApply is whether an update approved now would actually be attempted.
	//
	// Not merely "an updater is configured". An updater that is there and would
	// refuse the work -- this one refuses on an uncommitted repository,
	// because applying commits -- is the same to a person clicking the
	// button: they have already decided by the time they find out. Reported
	// so a UI can grey it beforehand.
	CanApply bool `json:"can_apply"`
	// Blocker says which kind of thing is in the way. Empty when CanApply.
	//
	// The two are mutually exclusive, so this carries no information the
	// reason does not -- it exists so a page can *style* them apart without
	// matching on prose the updater is free to reword. They read differently
	// enough to be worth it: one says nothing will change until somebody
	// configures something, the other says wait a moment.
	Blocker Blocker `json:"blocker,omitempty"`
	// WhyNot is what to say when CanApply is false, in the updater's own words.
	//
	// Relayed, not composed: why an updater would refuse is its business, and
	// a queue that wrote this sentence itself would be a queue that had
	// learned what applying involves.
	WhyNot string `json:"why_not,omitempty"`
	// Applying is the services with an apply in flight right now.
	//
	// Reported so a page that was reloaded mid-apply can say so. Whether
	// something is running is a fact about the queue, not about the browser
	// that happened to start it; without this a fresh page offers the same
	// update again, and the second click is refused.
	Applying []string `json:"applying,omitempty"`
}

// Blocker is which kind of thing stops an approval landing.
//
// Two, and they differ in what a person should do about them. No updater is a
// standing configuration: it will not change until a compose file is edited.
// An updater that is not ready is transient -- the reference one is unready on
// an uncommitted repository -- and clears with nothing deployed.
type Blocker string

const (
	// NoUpdater: this queue has nothing to hand work to.
	NoActor Blocker = "no-actor"
	// UpdaterNotReady: there is an updater and it would refuse right now.
	ActorNotReady Blocker = "actor-not-ready"
)

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.Snapshot())
}

// Snapshot is what /v1/snapshot answers, for a UI in the same process.
func (s *Server) Snapshot() Snapshot {
	canApply, blocker, whyNot := s.actorReadiness()
	lastCheck := ""
	if s.LastChecked != nil {
		if at := s.LastChecked(); !at.IsZero() {
			lastCheck = at.UTC().Format(time.RFC3339)
		}
	}
	return Snapshot{
		Host:      s.Host,
		Version:   s.Version,
		Pending:   s.Queue.Waiting(),
		Soaking:   s.Queue.Soaking(),
		Blocks:    s.Queue.Blocks(),
		LastCheck: lastCheck,
		CanApply:  canApply,
		Blocker:   blocker,
		WhyNot:    whyNot,
		Applying:  s.applying(),
	}
}

// ApplyingReporter is an Applier that can say what it is working on.
//
// Separate from Applier for the same reason as ReadinessReporter: one that
// cannot answer must stay usable.
type ApplyingReporter interface {
	Applying() []string
}

func (s *Server) applying() []string {
	if ar, ok := s.Applier.(ApplyingReporter); ok {
		return ar.Applying()
	}
	return nil
}

// apply approves a queued entry.
//
// A refusal is 200 with a message rather than a 4xx: "nothing queued under
// that name" is an answer about the queue's state, which a UI renders to a
// person. An HTTP error would make its client report a transport failure for
// something that is not one.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	service := strings.TrimPrefix(r.URL.Path, "/v1/apply/")
	if service == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}

	if raw := r.URL.Query().Get("entry"); raw != "" {
		id, ok := entryID(raw)
		if !ok {
			writeMessage(w, "no such entry")
			return
		}
		writeMessage(w, s.ApproveEntry(id, actorOf(r)))
		return
	}
	writeMessage(w, s.Approve(service, actorOf(r)))
}

// Approve starts applying what is queued for a service: what /v1/apply
// does, for a UI in the same process. The answer is a message for a person,
// empty when it started.
func (s *Server) Approve(service, actor string) string {
	// No updater is a configuration, not a fault: a queue without one queues
	// every decision and applies nothing, which is how this runs before an
	// updater is trusted with the socket. Said plainly, and before the queue is
	// touched -- an entry must not leave the queue for an apply that cannot
	// happen.
	if s.Applier == nil {
		return "no actor is configured, so nothing can be applied from here"
	}

	e, ok := s.Queue.Get(service)
	if !ok {
		return "nothing is queued for " + service
	}
	if err := s.Applier.Start(e); err != nil {
		return err.Error()
	}
	s.Queue.Approved(e, actor, time.Now())
	// Approved is no longer waiting on a person, but it is not applied either
	// until the updater says so. The entry stays in the queue until then --
	// removing it here meant a failed apply left no trace anywhere that the
	// service still needed the update. The applier removes it once the
	// updater reports success.
	return ""
}

// stream relays an in-flight apply's progress.
//
// The queue is a pipe here, not a participant: what the updater says is
// copied through without being interpreted, so the updater keeps owning the
// shape of its own progress.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	service := strings.TrimPrefix(r.URL.Path, "/v1/stream/")
	if service == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}
	if s.Applier == nil {
		http.Error(w, "no actor is configured, so nothing is ever in flight",
			http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.Applier.Stream(service, w, r)
}

// Stream relays an in-flight apply's progress, for a UI in the same process.
func (s *Server) Stream(service string, w http.ResponseWriter, r *http.Request) error {
	if s.Applier == nil {
		return fmt.Errorf("no actor is configured, so nothing is ever in flight")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.Applier.Stream(service, w, r)
	return nil
}

func writeMessage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// ReadinessReporter is an Applier that can say whether it would take work.
//
// Separate from Applier because not every one can answer -- and an Applier
// that cannot must stay usable rather than be excluded by the interface. The
// queue reports what it is told and adds nothing: why an updater is not ready
// is the updater's business, and the queue knowing would mean learning what
// applying involves.
type ReadinessReporter interface {
	Ready() (ready bool, reason string)
}

// updaterReadiness asks the applier, when it can answer.
//
// No updater at all is not-ready with the reason already used elsewhere, so a
// page has one thing to render rather than two shapes of the same fact.
func (s *Server) actorReadiness() (bool, Blocker, string) {
	if s.Applier == nil {
		return false, NoActor, "no actor is configured, so nothing can be applied from here"
	}
	if rr, ok := s.Applier.(ReadinessReporter); ok {
		ready, reason := rr.Ready()
		if ready {
			return true, "", ""
		}
		return false, ActorNotReady, reason
	}
	return true, "", ""
}

// actorOf is who a request is on behalf of. The queue API is called by a UI
// holding this queue's token, which signs its person in and names them here;
// the token is the trust, and the name is the record.
func actorOf(r *http.Request) string {
	if a := strings.TrimSpace(r.Header.Get("X-Duva-Actor")); a != "" {
		return a
	}
	return "the queue API"
}

// post is the shape every action route shares: POST only, the rest of the
// path as the subject, an answer that is a message for a person.
func (s *Server) post(prefix string, do func(subject string, r *http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeMessage(w, do(strings.TrimPrefix(r.URL.Path, prefix), r))
	}
}

func entryID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) discardBy(subject string, r *http.Request) string {
	id, ok := entryID(subject)
	if !ok {
		return "no such entry"
	}
	return s.Discard(id, actorOf(r))
}

func (s *Server) blockBy(subject string, r *http.Request) string {
	id, ok := entryID(subject)
	if !ok {
		return "no such entry"
	}
	return s.Block(id, r.FormValue("reason"), actorOf(r))
}

func (s *Server) unblockBy(_ string, r *http.Request) string {
	return s.Unblock(r.FormValue("image"), r.FormValue("digest"), actorOf(r))
}

func (s *Server) checkBy(service string, r *http.Request) string {
	return s.CheckNow(service, actorOf(r))
}

func (s *Server) rescanBy(service string, r *http.Request) string {
	return s.RescanNow(service, actorOf(r))
}

// Discard drops an entry for now. The answer is a message, empty on success.
func (s *Server) Discard(id int64, actor string) string {
	if err := s.Queue.Discard(id, actor, time.Now()); err != nil {
		return err.Error()
	}
	return ""
}

// Block blocks the release an entry offers, by its digest: the one it
// carries, or the one its tag points at now.
func (s *Server) Block(id int64, reason, actor string) string {
	e, ok := s.Queue.ByID(id)
	if !ok {
		return ErrNotOpen.Error()
	}
	digest := e.Digest
	if digest == "" {
		if s.Resolve == nil {
			return "this release's digest is not known yet, and nothing here can look it up"
		}
		var err error
		if digest, err = s.Resolve(e.Image, e.Tag); err != nil {
			return fmt.Sprintf("looking up what %s:%s points at: %v", e.Image, e.Tag, err)
		}
	}
	if err := s.Queue.Block(id, digest, reason, actor, time.Now()); err != nil {
		return err.Error()
	}
	return ""
}

// Unblock lifts a block.
func (s *Server) Unblock(image, digest, actor string) string {
	if err := s.Queue.Unblock(image, digest, actor, time.Now()); err != nil {
		return err.Error()
	}
	return ""
}

// cooldown is how soon the same service may be checked again on request: a
// person clicking twice must not spend a registry's rate limit.
const cooldown = time.Minute

func (s *Server) cooled(service string) bool {
	s.cooldownMu.Lock()
	defer s.cooldownMu.Unlock()
	if s.lastAsked == nil {
		s.lastAsked = map[string]time.Time{}
	}
	if last, ok := s.lastAsked[service]; ok && time.Since(last) < cooldown {
		return false
	}
	s.lastAsked[service] = time.Now()
	return true
}

// CheckNow asks the watcher to look at a service now.
func (s *Server) CheckNow(service, actor string) string {
	return s.ask(service, actor, "check", s.Check)
}

// CheckAll asks the watcher to look at every service now: the page's
// "Check now". Once a minute at most, like a check of one.
func (s *Server) CheckAll(actor string) string {
	if s.Check == nil {
		return "nothing here can check"
	}
	if !s.cooled("*") {
		return "a check was asked for a moment ago; try again in a minute"
	}
	s.Queue.Asked("", "check-all", actor, time.Now())
	go func() {
		if err := s.Check(""); err != nil {
			s.Queue.fail(fmt.Errorf("checking every service: %w", err))
		}
	}()
	return ""
}

// RescanNow asks the watcher to forget a service's cutoff and look again.
func (s *Server) RescanNow(service, actor string) string {
	return s.ask(service, actor, "rescan", s.Rescan)
}

func (s *Server) ask(service, actor, what string, do func(string) error) string {
	if do == nil {
		return "nothing here can " + what
	}
	if service == "" {
		return "no service"
	}
	if !s.cooled(service) {
		return fmt.Sprintf("%s was asked for a moment ago; try again in a minute", service)
	}
	s.Queue.Asked(service, what, actor, time.Now())
	// In the background: a check is minutes of registry calls, and the
	// answer is what it finds, which arrives in the queue.
	go func() {
		if err := do(service); err != nil {
			s.Queue.fail(fmt.Errorf("%s of %s: %w", what, service, err))
		}
	}()
	return ""
}

// ApproveEntry starts applying one entry, by its row: what a person approves
// when a service has more than one -- one soaking, one waiting -- and "the
// service" would not say which.
func (s *Server) ApproveEntry(id int64, actor string) string {
	if s.Applier == nil {
		return "no actor is configured, so nothing can be applied from here"
	}
	e, ok := s.Queue.ByID(id)
	if !ok {
		return ErrNotOpen.Error()
	}
	if err := s.Applier.Start(e); err != nil {
		return err.Error()
	}
	s.Queue.Approved(e, actor, time.Now())
	return ""
}
