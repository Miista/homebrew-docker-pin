// Package ui serves the approval queue over one or more queues.
//
// Lifted from duva's internal/ui, which served the same page over agents.
// What changed is the protocol underneath: a host's queue is one list
// rather than a queue and a soak list, and there is no refresh button because
// the watcher owns when a check happens -- the UI cannot make a registry be
// asked.
package ui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/queue"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
	"github.com/Miista/homebrew-docker-pin/oci/version"
)

//go:embed page.html
var pageHTML string

//go:embed assets/logo.png
var logoPNG []byte

//go:embed assets/petite-vue.js
var petiteVue []byte

//go:embed assets/favicon.png
var faviconPNG []byte

// Geist, served from here rather than a font host: a signed-in page that
// fetches from elsewhere on every load tells that host who is looking, and
// when. Variable fonts, one file per family for every weight. SIL OFL 1.1,
// assets/Geist-OFL.txt; from the geist npm package, 1.8.1.
//
//go:embed assets/Geist-Variable.woff2
var geistSans []byte

//go:embed assets/GeistMono-Variable.woff2
var geistMono []byte

// The Go template's delimiters are moved out of petite-vue's way.
//
// Both use {{ }} by default, and the page is markup petite-vue reads at
// runtime -- so anything Go interpolated would have to be something petite-vue
// never sees, and vice versa. Rather than police that by hand, Go uses [[ ]]
// and leaves {{ }} entirely to the page.
var page = template.Must(template.New("page").Delims("[[", "]]").Parse(pageHTML))

// Source supplies what the page renders.
//
// An interface rather than the collector, so the page can be tested without
// a queue to talk to -- and so a single-queue deployment could serve
// itself without fanning out.
type Source interface {
	// Pending returns what is waiting, labelled by host and sorted.
	Pending() []Hosted
	// Blocked names queues that cannot apply right now, with why, so the
	// page can say it once and disable the buttons those rows offer.
	Blocked() []Blocked
	// Unreachable lists queues that could not be asked. Rendered rather
	// than logged: a queue missing a host looks exactly like that host
	// having nothing to do.
	Unreachable() []Problem
}

// SoakSource is a Source that also reports what is soaking. Separate, as
// Streamer is, so a Source that only knows a queue still serves the page.
type SoakSource interface {
	Soaking() []Hosted
}

// HealthSource is a Source that can say how the fleet is: the hub's collector.
type HealthSource interface {
	Health() queue.Health
}

// UpcomingSource is a Source that also reports what is held for a person.
type UpcomingSource interface {
	Upcoming() []Hosted
}

// Approver acts on a click.
type Approver interface {
	// Approve tells the owning queue to apply one queued entry, keyed
	// "host/service" -- and, when the page says which, by its row: a service
	// can have one entry soaking and another waiting for a person.
	Approve(key string, entry int64, actor string) error
}

// Actions are what a person may do to a queue besides approving. Collector
// implements them, routing each to the host that owns it.
type Actions interface {
	Discard(host string, id int64, actor string) error
	Block(host string, id int64, reason, actor string) error
	// Check asks host to look at one service now, and CheckAll every host at
	// every service; each at least back far back (zero: since the last check).
	Check(host, service, actor string, back time.Duration) error
	Unblock(host, image, digest, actor string) error
	CheckAll(actor string, back time.Duration) error
	RunJob(host, job, actor string) error
}

// BlockSource is a Source that also reports what is blocked.
type BlockSource interface {
	Blocks() []HostedBlock
}

// CheckSource is a Source that also reports when each host last checked.
type CheckSource interface {
	LastChecks() []HostCheck
}

// Server serves the queue.
type Server struct {
	Source Source
	// Approver enables the Update button. Nil leaves the page read-only and
	// the endpoint unregistered -- not merely refusing, absent -- because an
	// endpoint that triggers updates should not exist on a UI meant only to
	// report.
	Approver Approver
	// Actions enables discard, block, unblock and check. Nil leaves them off
	// the page and their endpoints unregistered, as for Approver: they carry
	// the same authority over a host.
	Actions Actions
	// Actor names who a request is from, for the record of an approval.
	// Nil records no one in particular.
	Actor   func(*http.Request) string
	Version string
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// The fleet's health where the source can say it -- the hub's collector
	// -- and plain "ok" where it cannot.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if hs, ok := s.Source.(HealthSource); ok {
			queue.WriteHealth(w, hs.Health())
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/logo.png", servePNG(logoPNG))
	mux.HandleFunc("/favicon.png", servePNG(faviconPNG))
	mux.HandleFunc("/petite-vue.js", serveJS(petiteVue))
	mux.HandleFunc("/geist.woff2", serveFont(geistSans))
	mux.HandleFunc("/geist-mono.woff2", serveFont(geistMono))
	mux.HandleFunc("/api/state", s.stateJSON)
	mux.HandleFunc("/api/stream/", s.stream)
	mux.HandleFunc("/notes/", s.notes)
	if s.Approver != nil {
		mux.HandleFunc("/apply", s.apply)
		mux.HandleFunc("/api/apply/", s.applyJSON)
	}
	if s.Actions != nil {
		mux.HandleFunc("/api/discard/", s.action("/api/discard/", s.discard))
		mux.HandleFunc("/api/block/", s.action("/api/block/", s.block))
		mux.HandleFunc("/api/check/", s.action("/api/check/", s.check))
		mux.HandleFunc("/api/unblock/", s.unblock)
		mux.HandleFunc("/api/check-all", s.checkAll)
		mux.HandleFunc("/api/run/", s.action("/api/run/", func(host, job string, r *http.Request) error {
			return s.Actions.RunJob(host, job, s.actor(r))
		}))
	}
	mux.HandleFunc("/", s.index)
	return mux
}

// servePNG returns a handler for one embedded image. Content-Type is set
// explicitly rather than left to Go's sniffing: embedded bytes never change,
// so neither should the answer.
func servePNG(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	}
}

// serveFont returns a handler for one embedded font. A week's cache: the
// bytes are fixed for the life of the binary.
func serveFont(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.Write(b)
	}
}

// row is one line of the table.
//
// Rendered by the Go template for first paint and serialised to the page's
// poller, from one toRow -- so the two cannot describe a row differently.
type row struct {
	Service string `json:"service"`
	// Host is the queue it came from, shown so two services of the same
	// name on different hosts are tellable apart.
	Host string `json:"host"`
	// Key is what a click posts back: "host/service". The row carries it
	// rather than the page rebuilding it, so there is one definition of what
	// identifies a row.
	Key string `json:"key"`
	// Entry is the queued entry's row on its host, for the actions that act
	// on an entry rather than a service: discard and block.
	Entry      int64  `json:"entry"`
	Image      string `json:"image"`
	CurrentTag string `json:"current_tag"`
	Candidate  string `json:"candidate"`
	// Moved marks a digest move, where the tag did not change and there is
	// no from/to to render.
	Moved bool `json:"moved"`
	// Tag is what the service follows, shown instead of a digest pair.
	Tag string `json:"tag"`
	// CanApply is whether this row's queue has an update. Per row: one host
	// can have an updater while another does not.
	CanApply bool `json:"can_apply"`
	// Stale marks a row whose queue has stopped answering.
	Stale bool `json:"stale"`
	// Applying is whether the queue is applying this row right now, however
	// the apply began. The page resumes watching it rather than offering it
	// again.
	Applying  bool   `json:"applying"`
	Kind      string `json:"kind"`
	Why       string `json:"why"`
	FirstSeen string `json:"first_seen"`
	// Published is when the release appeared upstream, RFC 3339; empty when
	// the registry does not say.
	Published string `json:"published,omitempty"`
	Auto      string `json:"auto"`
	// ReleaseAt is when a soaking entry applies itself, RFC 3339; empty for
	// one waiting for a person.
	ReleaseAt string `json:"release_at,omitempty"`
	// Notes is where to read what the release changes, from the repository
	// its image names; empty when it names none.
	Notes string `json:"notes,omitempty"`
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// One route, one path: anything else is a typo, not a page.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	st := s.snapshot()
	initial, err := json.Marshal(st)
	if err != nil {
		// Cannot happen with these types, but a page whose script found no
		// data would render empty and say nothing about why.
		initial = []byte("{}")
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Execute's error is checked rather than dropped. A template naming a
	// field the data does not carry fails mid-render, after a 200 and a half
	// a page have already gone out -- which reads as a blank page with no
	// explanation anywhere. Logging it is the difference between a minute and
	// an hour.
	if err := page.Execute(w, struct {
		state
		// InitialJSON is the same state again, for the page's script to start
		// from. Rendered rather than fetched so the first paint is already
		// correct and the no-JavaScript case is whole.
		InitialJSON template.JS
		Service     string
		Level       string
		Message     string
	}{
		state:       st,
		InitialJSON: template.JS(initial),
		Service:     r.URL.Query().Get("service"),
		Level:       r.URL.Query().Get("level"),
		Message:     r.URL.Query().Get("message"),
	}); err != nil {
		fmt.Fprintf(w, "\n<!-- the page failed to render: %s -->\n", err)
	}
}

// state is everything the page renders from, in one shape.
//
// One struct for the template and the JSON, so the first paint and every poll
// after it are describing the same thing. Two shapes would be two chances to
// disagree about what a queue is.
type state struct {
	Rows []row `json:"rows"`
	// Soaking is what applies itself once its soak runs out: a thing of its
	// own, shown apart from what waits for a person.
	Soaking []row `json:"soaking"`
	// Upcoming is what comes to a person once its delay is over: held, so not
	// in the queue yet, and not something duva will apply.
	Upcoming []row `json:"upcoming"`
	// Blocks are the releases a person blocked, on every host.
	Blocks []HostedBlock `json:"blocks"`
	// Checks is when each host last looked for new tags.
	Checks      []HostCheck `json:"checks"`
	Unreachable []Problem   `json:"unreachable"`
	Blocked     []Blocked   `json:"blocked"`
	CanApply    bool        `json:"can_apply"`
	// Actions is whether the page offers discard, block, unblock and check.
	Actions bool   `json:"actions"`
	Version string `json:"version"`
}

func (s *Server) snapshot() state {
	pending := s.Source.Pending()
	rows := make([]row, 0, len(pending))
	for _, e := range pending {
		rows = append(rows, toRow(e))
	}
	soaking := []row{}
	if ss, ok := s.Source.(SoakSource); ok {
		for _, e := range ss.Soaking() {
			soaking = append(soaking, toRow(e))
		}
	}
	upcoming := []row{}
	if us, ok := s.Source.(UpcomingSource); ok {
		for _, e := range us.Upcoming() {
			upcoming = append(upcoming, toRow(e))
		}
	}
	blocks := []HostedBlock{}
	if bs, ok := s.Source.(BlockSource); ok {
		blocks = bs.Blocks()
	}
	checks := []HostCheck{}
	if cs, ok := s.Source.(CheckSource); ok {
		checks = cs.LastChecks()
	}
	// Initialised rather than left nil: Go encodes a nil slice as JSON null,
	// and a page reading .length off it throws before rendering anything.
	unreachable := s.Source.Unreachable()
	if unreachable == nil {
		unreachable = []Problem{}
	}
	blocked := s.Source.Blocked()
	if blocked == nil {
		blocked = []Blocked{}
	}
	return state{
		Rows:        rows,
		Soaking:     soaking,
		Upcoming:    upcoming,
		Blocks:      blocks,
		Checks:      checks,
		Unreachable: unreachable,
		Blocked:     blocked,
		CanApply:    s.Approver != nil,
		Actions:     s.Actions != nil,
		Version:     s.Version,
	}
}

// apply approves one queued entry and redirects back to the page carrying
// the result.
//
// A redirect rather than a fragment of HTML, so a reload does not re-submit
// and the result survives being read after the fact.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := r.FormValue("service")
	if key == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}

	level, message := "ok", "update started"
	if err := s.Approver.Approve(key, entryParam(r), s.actor(r)); err != nil {
		level, message = "error", err.Error()
	}
	_, service, _ := SplitKey(key)
	http.Redirect(w, r, "/?service="+service+"&level="+level+
		"&message="+template.HTMLEscapeString(message), http.StatusSeeOther)
}

// display shortens a candidate for the table.
//
// A moved digest is seventy-one characters of hex nobody reads; the first
// twelve identify it well enough to match against a log line or a commit.
func display(e queue.Entry) string {
	return e.To
}

// isDigestMove reports whether an entry is a moving tag that moved, rather
// than a change of tag.
func isDigestMove(e queue.Entry) bool {
	return strings.HasPrefix(e.To, "sha256:")
}

// kindLabel is what the Kind column says.
//
// A digest move has no version pair, so there is nothing to classify -- and
// saying so plainly beats an empty cell, which reads as missing data.
func kindLabel(e queue.Entry) string {
	if e.Kind == "" {
		return "digest"
	}
	if e.Kind == version.KindUnknown {
		return "unknown"
	}
	return string(e.Kind)
}

// stateJSON is the page data for the poller, from the same snapshot the
// template renders -- so a poll and a first paint cannot disagree.
func (s *Server) stateJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(s.snapshot())
}

// stream relays one in-flight apply's progress to the page.
//
// Relayed rather than letting the browser talk to the queue directly: the
// token that reaches a queue is this process's, and putting it in a page
// would hand the authority to replace containers to every browser that loads
// the queue.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/api/stream/")
	if key == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}
	streamer, ok := s.Source.(Streamer)
	if !ok {
		http.Error(w, "this source cannot stream", http.StatusNotImplemented)
		return
	}
	if err := streamer.Stream(key, w, r); err != nil {
		// Headers may already be out by the time this fails, so this is
		// best-effort: the page treats a stream that ends without a terminal
		// line as finished and re-polls, which is the honest answer anyway.
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}

// Streamer is a Source that can relay an apply's progress.
//
// Separate from Source because a Source that only ever renders a queue is
// still useful -- and because the page degrades to polling when there is
// nothing to stream from.
type Streamer interface {
	Stream(key string, w http.ResponseWriter, r *http.Request) error
}

// shortImage drops the registry host from an image shown to a person.
//
// Every row carries one and almost every one is docker.io or ghcr.io, so the
// prefix distinguishes nothing while costing the width that the repository
// and the tag actually need. Docker Hub images are already written both ways
// in this project -- deluan/navidrome and docker.io/happierdev/relay-server --
// so stripping it also makes those agree.
//
// Only those two. A private registry is kept: registry.example.com/hemma-agent
// is not the same thing as hemma-agent, and that is worth seeing. This is
// display only; nothing pulls or pins from what comes back.
func shortImage(image string) string {
	for _, host := range []string{"docker.io/", "ghcr.io/"} {
		if strings.HasPrefix(image, host) {
			return strings.TrimPrefix(image, host)
		}
	}
	return image
}

// toRow is the one place a Hosted becomes a row, shared by the template and
// the JSON so the two cannot drift.
func toRow(e Hosted) row {
	return row{
		Service:    e.Service,
		Host:       e.Host,
		Key:        Key(e.Host, e.Service),
		Entry:      e.ID,
		ReleaseAt:  e.ReleaseAt,
		Image:      shortImage(e.Image),
		CurrentTag: e.From,
		Candidate:  display(e.Entry),
		Moved:      isDigestMove(e.Entry),
		Tag:        e.Tag,
		CanApply:   e.CanApply && !e.Stale,
		Stale:      e.Stale,
		Applying:   e.Applying,
		Kind:       kindLabel(e.Entry),
		Why:        e.Why,
		FirstSeen:  e.FirstSeen,
		Published:  e.Published,
		Auto:       string(e.Auto),
		Notes:      notesLink(e),
	}
}

// notesLink is a row's release notes link: through this page, which finds
// the release when it is followed. Empty when the image names no source.
func notesLink(e Hosted) string {
	if len(queue.ReleaseNotes(e.Source, e.To, isDigestMove(e.Entry), e.Release)) == 0 {
		return ""
	}
	return fmt.Sprintf("/notes/%s/%d", url.PathEscape(e.Host), e.ID)
}

// notesClient asks whether a release page exists. One for the page, as the
// registry client is one for duva: a client per request is a connection and
// a DNS lookup each.
var notesClient = &http.Client{Timeout: 5 * time.Second}

// notes sends a person to a release's notes: GET /notes/<host>/<entry>.
//
// Found when followed rather than when the page is drawn: which tag form a
// repository uses -- 3.3.3 or v3.3.3 -- cannot be told from the image, and
// asking GitHub for every row on every poll would be two requests a row each
// fifteen seconds to answer a click that mostly never comes. So: the first of
// queue.ReleaseNotes' places that answers, else the last, which is the
// repository's newest releases and always there.
func (s *Server) notes(w http.ResponseWriter, r *http.Request) {
	host, id, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/notes/"), "/")
	entry, err := strconv.ParseInt(id, 10, 64)
	if !ok || err != nil {
		http.NotFound(w, r)
		return
	}
	rows := s.Source.Pending()
	if ss, ok := s.Source.(SoakSource); ok {
		rows = append(rows, ss.Soaking()...)
	}
	if us, ok := s.Source.(UpcomingSource); ok {
		rows = append(rows, us.Upcoming()...)
	}
	for _, e := range rows {
		if e.Host != host || e.ID != entry {
			continue
		}
		places := queue.ReleaseNotes(e.Source, e.To, isDigestMove(e.Entry), e.Release)
		if len(places) == 0 {
			break
		}
		to := places[len(places)-1]
		for _, p := range places[:len(places)-1] {
			req, err := http.NewRequestWithContext(r.Context(), http.MethodHead, p, nil)
			if err != nil {
				continue
			}
			if resp, err := notesClient.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					to = p
					break
				}
			}
		}
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

// serveJS returns a handler for the vendored script.
//
// Vendored rather than fetched from a CDN: this page approves container
// replacements, and a script pulled from someone else's server at render time
// is a supply chain for that. It also has to work on a LAN with no internet.
func serveJS(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	}
}

// applyJSON approves an entry for the page's script.
//
// Beside /apply rather than replacing it: /apply is a form POST that
// redirects, which is what makes the page work with JavaScript disabled. This
// one answers with a message and no navigation, because the page renders the
// result itself.
func (s *Server) applyJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/api/apply/")
	if key == "" {
		http.Error(w, "no service", http.StatusBadRequest)
		return
	}

	message := ""
	if err := s.Approver.Approve(key, entryParam(r), s.actor(r)); err != nil {
		message = err.Error()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func (s *Server) actor(r *http.Request) string {
	if s.Actor == nil {
		return ""
	}
	return s.Actor(r)
}

// action is the shape the four action routes share: POST, a path of
// "host/subject", and an answer that is a message for a person.
func (s *Server) action(prefix string, do func(host, subject string, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host, subject, ok := SplitKey(strings.TrimPrefix(r.URL.Path, prefix))
		if !ok || subject == "" {
			http.Error(w, "want host/subject", http.StatusBadRequest)
			return
		}
		message := ""
		if err := do(host, subject, r); err != nil {
			message = err.Error()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"message": message})
	}
}

func entryOf(subject string) (int64, error) {
	id, err := strconv.ParseInt(subject, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("no such entry")
	}
	return id, nil
}

func (s *Server) discard(host, subject string, r *http.Request) error {
	id, err := entryOf(subject)
	if err != nil {
		return err
	}
	return s.Actions.Discard(host, id, s.actor(r))
}

func (s *Server) block(host, subject string, r *http.Request) error {
	id, err := entryOf(subject)
	if err != nil {
		return err
	}
	return s.Actions.Block(host, id, strings.TrimSpace(r.FormValue("reason")), s.actor(r))
}

func (s *Server) check(host, service string, r *http.Request) error {
	back, err := watch.ParseLookBack(r.FormValue("since"))
	if err != nil {
		return fmt.Errorf("since: %w", err)
	}
	return s.Actions.Check(host, service, s.actor(r), back)
}

// entryParam is the entry an approval names, zero for none.
func entryParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.FormValue("entry"), 10, 64)
	return id
}

// unblock lifts a block: POST /api/unblock/<host>, with the image and digest.
func (s *Server) unblock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := strings.TrimPrefix(r.URL.Path, "/api/unblock/")
	message := ""
	if err := s.Actions.Unblock(host, r.FormValue("image"), r.FormValue("digest"), s.actor(r)); err != nil {
		message = err.Error()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// checkAll asks every host to look for new tags now: POST /api/check-all,
// with since -- how far back, "7d" -- or nothing for since the last check.
func (s *Server) checkAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	message := ""
	if back, err := watch.ParseLookBack(r.FormValue("since")); err != nil {
		message = "since: " + err.Error()
	} else if err := s.Actions.CheckAll(s.actor(r), back); err != nil {
		message = err.Error()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}
