// Package ui serves duva's approval queue.
//
// Strictly a queue: the updates duva will not apply on its own, and (from M4)
// a way to approve them. No configuration, no registry browser, no container
// inventory, no history. Updates applied automatically never appear here —
// the git log is their record.
//
// The page is one embedded template with no JavaScript and no build step.
package ui

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/pin"
	"github.com/Miista/homebrew-docker-pin/internal/watch"
	"net/url"
)

//go:embed page.html
var pageHTML string

var page = template.Must(template.New("page").Parse(pageHTML))

// Source supplies what the page renders. The server holds no state of its own;
// it asks whoever owns the state, so there is one copy of the truth.
type Source interface {
	// Pending returns the updates awaiting approval, sorted.
	Pending() []watch.Pending
	// Soaking is what duva.delay is holding back: decided, waiting on time.
	Soaking() []watch.Soaking
	// LastCheck describes when detection last ran, for the footer, as an age
	// rather than a timestamp: the question a reader has is whether it was
	// recent.
	LastCheck() string
	// LastCheckExact is the same moment as a timestamp, for the tooltip,
	// when the age is not precise enough.
	LastCheckExact() string
}

// Applier applies one queued update. Approving from the page runs the same
// path as an automatic update -- one way of applying, not two -- so a click
// cannot do something an unattended run would not.
type Applier interface {
	// Apply performs the update for a service and reports what happened in
	// one clause, or an error if it did not reach the container.
	Apply(service string) (string, error)
}

// Server serves the queue.
type Server struct {
	Source  Source
	Host    string // shown in the header, so several hosts are tellable apart
	Version string
	// Applier enables the Apply action. Nil leaves the page read-only and
	// the endpoint unregistered -- not merely refusing, absent -- because an
	// endpoint that triggers updates should not exist on a duva that is only
	// meant to report.
	Applier Applier
}

// Handler returns the routes: the page, and a health endpoint for whatever is
// watching the container.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	if s.Applier != nil {
		mux.HandleFunc("/apply", s.apply)
	}
	mux.HandleFunc("/", s.index)
	return mux
}

// apply runs one queued update and re-renders the queue. It is a POST because
// it changes things: a link a browser might prefetch must never restart a
// container.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	service := r.FormValue("service")
	if service == "" {
		http.Error(w, "no service given", http.StatusBadRequest)
		return
	}

	note, err := s.Applier.Apply(service)
	msg, level := note, "ok"
	if err != nil {
		msg, level = err.Error(), "error"
	}

	// Redirect rather than rendering in place, so a refresh does not repeat
	// the update.
	http.Redirect(w, r, "/?"+url.Values{
		"service": {service},
		"level":   {level},
		"message": {msg},
	}.Encode(), http.StatusSeeOther)
}

type row struct {
	Service    string
	Image      string
	CurrentTag string
	Candidate  string
	Kind       string
	Why        string
	FirstSeen  string
	Auto       string
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// One route, one path: anything else is a typo, not a page.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	pending := s.Source.Pending()
	soaking := s.Source.Soaking()
	rows := make([]row, 0, len(pending))
	for _, p := range pending {
		rows = append(rows, row{
			Service:    p.Service,
			Image:      p.Image,
			CurrentTag: p.CurrentTag,
			Candidate:  display(p),
			Kind:       kindLabel(p),
			Why:        p.Why,
			FirstSeen:  p.FirstSeen,
			Auto:       p.Auto,
		})
	}

	// Soaking rows are the same shape: an operator acts on them the same way,
	// and the difference -- that duva decided already and is waiting on time
	// rather than on a person -- is in what the row says, not what it is.
	waiting := make([]row, 0, len(soaking))
	for _, p := range soaking {
		waiting = append(waiting, row{
			Service:    p.Service,
			Image:      p.Image,
			CurrentTag: p.CurrentTag,
			Candidate:  p.Candidate,
			Kind:       string(p.Bump),
			Why:        p.Outcome + " in " + p.Remaining,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(w, struct {
		Pending        []row
		Soaking        []row
		Host           string
		Version        string
		LastCheck      string
		LastCheckExact string
		CanApply       bool
		Service        string
		Level          string
		Message        string
	}{
		Pending:        rows,
		Soaking:        waiting,
		Host:           s.Host,
		Version:        s.Version,
		LastCheck:      s.Source.LastCheck(),
		LastCheckExact: s.Source.LastCheckExact(),
		CanApply:       s.Applier != nil,
		Service:        r.URL.Query().Get("service"),
		Level:          r.URL.Query().Get("level"),
		Message:        r.URL.Query().Get("message"),
	}); err != nil {
		// Too late for an error page — the response is already going out.
		return
	}
}

// display shortens a candidate for the table. A moved digest is 71 characters
// of hex nobody reads; the first 12 identify it well enough to match against a
// notification or a commit.
func display(p watch.Pending) string {
	if p.Kind == watch.KindDigest {
		return pin.ShortDigest(p.Candidate)
	}
	return p.Candidate
}

// kindLabel is the label shown in the Kind column: how big the change is for
// a tag candidate, or "digest" for a moving tag, which has no version pair to
// compare and so can never be classified.
func kindLabel(p watch.Pending) string {
	if p.Kind == watch.KindDigest {
		return "digest"
	}
	if p.Bump != "" {
		return strings.ToLower(string(p.Bump))
	}
	return "tag"
}
