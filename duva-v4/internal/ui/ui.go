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
	"strings"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/queue"
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

// Approver acts on a click.
type Approver interface {
	// Approve tells the owning queue to apply one queued entry, keyed
	// "host/service".
	Approve(key string) error
}

// Server serves the queue.
type Server struct {
	Source Source
	// Approver enables the Update button. Nil leaves the page read-only and
	// the endpoint unregistered -- not merely refusing, absent -- because an
	// endpoint that triggers updates should not exist on a UI meant only to
	// report.
	Approver Approver
	Version  string
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/logo.png", servePNG(logoPNG))
	mux.HandleFunc("/favicon.png", servePNG(faviconPNG))
	mux.HandleFunc("/petite-vue.js", serveJS(petiteVue))
	mux.HandleFunc("/api/state", s.stateJSON)
	mux.HandleFunc("/api/stream/", s.stream)
	if s.Approver != nil {
		mux.HandleFunc("/apply", s.apply)
		mux.HandleFunc("/api/apply/", s.applyJSON)
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
	Key        string `json:"key"`
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
	Stale     bool   `json:"stale"`
	Kind      string `json:"kind"`
	Why       string `json:"why"`
	FirstSeen string `json:"first_seen"`
	Auto      string `json:"auto"`
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
	Rows        []row     `json:"rows"`
	Unreachable []Problem `json:"unreachable"`
	Blocked     []Blocked `json:"blocked"`
	CanApply    bool      `json:"can_apply"`
	Version     string    `json:"version"`
}

func (s *Server) snapshot() state {
	pending := s.Source.Pending()
	rows := make([]row, 0, len(pending))
	for _, e := range pending {
		rows = append(rows, toRow(e))
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
		Unreachable: unreachable,
		Blocked:     blocked,
		CanApply:    s.Approver != nil,
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
	if err := s.Approver.Approve(key); err != nil {
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
		Image:      shortImage(e.Image),
		CurrentTag: e.From,
		Candidate:  display(e.Entry),
		Moved:      isDigestMove(e.Entry),
		Tag:        e.Tag,
		CanApply:   e.CanApply && !e.Stale,
		Stale:      e.Stale,
		Kind:       kindLabel(e.Entry),
		Why:        e.Why,
		FirstSeen:  e.FirstSeen,
		Auto:       string(e.Auto),
	}
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
	if err := s.Approver.Approve(key); err != nil {
		message = err.Error()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}
