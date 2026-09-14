// Package ui serves the approval queue over one or more deciders.
//
// Lifted from duva's internal/ui, which served the same page over agents.
// What changed is the protocol underneath: a decider's queue is one list
// rather than a queue and a soak list, and there is no refresh button because
// the detector owns when a check happens -- the UI cannot make a registry be
// asked.
package ui

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/decide"
	"github.com/Miista/homebrew-docker-pin/oci/version"
)

//go:embed page.html
var pageHTML string

//go:embed assets/logo.png
var logoPNG []byte

//go:embed assets/favicon.png
var faviconPNG []byte

var page = template.Must(template.New("page").Parse(pageHTML))

// Source supplies what the page renders.
//
// An interface rather than the collector, so the page can be tested without
// a decider to talk to -- and so a single-decider deployment could serve
// itself without fanning out.
type Source interface {
	// Pending returns what is waiting, labelled by host and sorted.
	Pending() []Hosted
	// WithoutActor names deciders that have no actor, so the page can say
	// so once and disable the buttons those rows would otherwise offer.
	WithoutActor() []string
	// Unreachable lists deciders that could not be asked. Rendered rather
	// than logged: a queue missing a host looks exactly like that host
	// having nothing to do.
	Unreachable() []Problem
}

// Approver acts on a click.
type Approver interface {
	// Approve tells the owning decider to apply one queued entry, keyed
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
	if s.Approver != nil {
		mux.HandleFunc("/apply", s.apply)
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
type row struct {
	Service string
	// Host is the decider it came from, shown so two services of the same
	// name on different hosts are tellable apart.
	Host string
	// Key is what the form posts back: "host/service". The row carries it
	// rather than the page rebuilding it, so there is one definition of what
	// identifies a row.
	Key        string
	Image      string
	CurrentTag string
	Candidate  string
	// Moved marks a digest move, where the tag did not change and there is
	// no from/to to render.
	Moved bool
	// Tag is what the service follows, shown instead of a digest pair.
	Tag string
	// CanApply is whether this row's decider has an actor. Per row: one host
	// can have an actor while another does not.
	CanApply bool
	// Stale marks a row whose decider has stopped answering.
	Stale     bool
	Kind      string
	Why       string
	FirstSeen string
	Auto      string
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// One route, one path: anything else is a typo, not a page.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	pending := s.Source.Pending()
	rows := make([]row, 0, len(pending))
	for _, e := range pending {
		rows = append(rows, row{
			Service:    e.Service,
			Host:       e.Host,
			Key:        Key(e.Host, e.Service),
			Image:      e.Image,
			CurrentTag: e.From,
			Candidate:  display(e.Entry),
			// A moving tag that moved has no new tag to show: the tag is the
			// same, and the digest is 71 characters that say nothing a person
			// can act on. The tag alone is the whole of what changed.
			Moved:     isDigestMove(e.Entry),
			Tag:       e.Tag,
			CanApply:  e.CanApply && !e.Stale,
			Stale:     e.Stale,
			Kind:      kindLabel(e.Entry),
			Why:       e.Why,
			FirstSeen: e.FirstSeen,
			Auto:      string(e.Auto),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Execute's error is checked rather than dropped. A template naming a
	// field the data does not carry fails mid-render, after a 200 and a half
	// a page have already gone out -- which reads as a blank page with no
	// explanation anywhere. Logging it is the difference between a minute and
	// an hour.
	if err := page.Execute(w, struct {
		Pending      []row
		Unreachable  []Problem
		WithoutActor []string
		Version      string
		CanApply     bool
		Service      string
		Level        string
		Message      string
	}{
		Pending:      rows,
		Unreachable:  s.Source.Unreachable(),
		WithoutActor: s.Source.WithoutActor(),
		Version:      s.Version,
		CanApply:     s.Approver != nil,
		Service:      r.URL.Query().Get("service"),
		Level:        r.URL.Query().Get("level"),
		Message:      r.URL.Query().Get("message"),
	}); err != nil {
		fmt.Fprintf(w, "\n<!-- the page failed to render: %s -->\n", err)
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
func display(e decide.Entry) string {
	return e.To
}

// isDigestMove reports whether an entry is a moving tag that moved, rather
// than a change of tag.
func isDigestMove(e decide.Entry) bool {
	return strings.HasPrefix(e.To, "sha256:")
}

// kindLabel is what the Kind column says.
//
// A digest move has no version pair, so there is nothing to classify -- and
// saying so plainly beats an empty cell, which reads as missing data.
func kindLabel(e decide.Entry) string {
	if e.Kind == "" {
		return "digest"
	}
	if e.Kind == version.KindUnknown {
		return "unknown"
	}
	return string(e.Kind)
}
