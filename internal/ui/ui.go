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
)

//go:embed page.html
var pageHTML string

var page = template.Must(template.New("page").Parse(pageHTML))

// Source supplies what the page renders. The server holds no state of its own;
// it asks whoever owns the state, so there is one copy of the truth.
type Source interface {
	// Pending returns the updates awaiting approval, sorted.
	Pending() []watch.Pending
	// LastCheck describes when detection last ran, for the footer.
	LastCheck() string
}

// Server serves the queue.
type Server struct {
	Source  Source
	Host    string // shown in the header, so several hosts are tellable apart
	Version string
}

// Handler returns the routes: the page, and a health endpoint for whatever is
// watching the container.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", s.index)
	return mux
}

type row struct {
	Service    string
	Image      string
	CurrentTag string
	Candidate  string
	Reason     string
	FirstSeen  string
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	// One route, one path: anything else is a typo, not a page.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	pending := s.Source.Pending()
	rows := make([]row, 0, len(pending))
	for _, p := range pending {
		rows = append(rows, row{
			Service:    p.Service,
			Image:      p.Image,
			CurrentTag: p.CurrentTag,
			Candidate:  display(p),
			Reason:     reason(p),
			FirstSeen:  p.FirstSeen,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(w, struct {
		Pending   []row
		Host      string
		Version   string
		LastCheck string
	}{rows, s.Host, s.Version, s.Source.LastCheck()}); err != nil {
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

// reason is the label shown in the Kind column: how big the change is for a
// tag candidate, or "digest" for a moving tag, which has no version pair to
// compare and so can never be classified.
func reason(p watch.Pending) string {
	if p.Kind == watch.KindDigest {
		return "digest"
	}
	if p.Bump != "" {
		return strings.ToLower(string(p.Bump))
	}
	return "tag"
}
