// Package ui serves duva's approval queue.
//
// Strictly a queue: the updates duva will not apply on its own, and (from M4)
// a way to approve them. No configuration, no registry browser, no container
// inventory, no history. Updates applied automatically never appear here —
// the git log is their record.
//
// The page is one embedded template. A small amount of inline JavaScript
// polls /progress while an update is running, so the transaction's own steps
// (pulling, writing the pin, recreating, committing, pushing) show up as they
// happen rather than behind a single long-blocked page load.
package ui

import (
	_ "embed"
	"encoding/json"
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

//go:embed assets/logo.png
var logoPNG []byte

//go:embed assets/favicon.png
var faviconPNG []byte

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
//
// Applying is a transaction of several steps (pulling, writing the pin,
// recreating the container, committing, pushing) that can take long enough
// for a synchronous HTTP request to feel like the click did nothing. Start
// and Progress split it in two: Start launches the transaction and returns
// immediately, and the page polls Progress for what has happened so far --
// so a slow pull is visible as it happens, not as a blank tab.
type Applier interface {
	// Start begins updating a service in the background. An error here means
	// the update never began (nothing queued under that name, or one is
	// already running); anything the transaction itself fails on is reported
	// through Progress instead.
	Start(service string) error
	// Progress reports what has happened so far for a service's most recent
	// Start, and whether it has finished. ok is false when nothing has ever
	// been started for that service -- distinct from "finished with no
	// steps yet", which the page would otherwise be unable to tell apart
	// from "never asked".
	Progress(service string) (p Progress, ok bool)
}

// Progress is one update's state as the page's poller sees it.
type Progress struct {
	// Steps are the transaction's own log lines, oldest first, exactly as
	// applyOptions.Log recorded them.
	Steps []string `json:"steps"`
	// Done is true once the transaction has finished, successfully or not.
	Done bool `json:"done"`
	// Message is the final result, once Done -- the same clause /apply used
	// to carry in its redirect.
	Message string `json:"message"`
	// Failed marks Message as an error rather than a success, once Done.
	Failed bool `json:"failed"`
}

// Refresher runs a check on demand, for a "refresh now" button next to the
// footer's "checked ... ago" -- otherwise the only way to make duva look
// again before its own schedule is to restart the container.
//
// Split into Refresh/Progress the same way Applier is: a check is registry
// HTTP calls, which can stall on a slow or unreachable registry same as
// anything else that leaves the machine, so the button is answered
// immediately rather than blocking the request on however long that takes.
type Refresher interface {
	// Refresh starts a check in the background. An error here means one was
	// already running; a failure in the check itself is reported through
	// RefreshProgress instead.
	Refresh() error
	// RefreshProgress reports whether the most recently started check is
	// still running, and once done, whether it failed. Named distinctly from
	// Applier.Progress -- a type implementing both (duva's own store) cannot
	// have two methods sharing one name with different signatures.
	RefreshProgress() (running, done bool, err string)
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
	// Refresher enables the footer's refresh button. Nil leaves it absent,
	// for the same reason Applier being nil removes /apply: an endpoint that
	// can trigger a registry check should not exist on a duva not meant to
	// act on its own findings.
	Refresher Refresher
}

// Handler returns the routes: the page, and a health endpoint for whatever is
// watching the container.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/logo.png", servePNG(logoPNG))
	mux.HandleFunc("/favicon.png", servePNG(faviconPNG))
	if s.Applier != nil {
		mux.HandleFunc("/apply", s.apply)
		mux.HandleFunc("/progress", s.progress)
	}
	if s.Refresher != nil {
		mux.HandleFunc("/refresh", s.refresh)
		mux.HandleFunc("/refresh-progress", s.refreshProgress)
	}
	mux.HandleFunc("/", s.index)
	return mux
}

// servePNG returns a handler for one embedded image. Content-Type is set
// explicitly rather than left to Go's sniffing: embedded bytes never touch a
// filesystem, so nothing infers it from an extension the way http.FileServer
// would.
func servePNG(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(b)
	}
}

// refresh starts a check in the background and redirects straight back to
// the queue. Like apply, it does not wait: a registry can be as slow to
// answer as anything else that leaves the machine, and the page's poller
// picks up the result from refresh-progress.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	q := url.Values{}
	if err := s.Refresher.Refresh(); err != nil {
		q.Set("level", "error")
		q.Set("message", err.Error())
	}
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
}

// refreshProgress reports the in-flight check as JSON, for the page's
// poller.
func (s *Server) refreshProgress(w http.ResponseWriter, r *http.Request) {
	running, done, errMsg := s.Refresher.RefreshProgress()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(struct {
		Running bool   `json:"running"`
		Done    bool   `json:"done"`
		Error   string `json:"error,omitempty"`
	}{running, done, errMsg})
}

// apply starts one queued update in the background and redirects straight
// back to the queue. It is a POST because it changes things: a link a
// browser might prefetch must never restart a container.
//
// It does not wait for the update to finish -- that can take long enough
// that the click would otherwise sit behind a blank tab. The page's poller
// picks up where this left off, using the service named in the redirect.
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

	q := url.Values{"service": {service}}
	if err := s.Applier.Start(service); err != nil {
		q.Set("level", "error")
		q.Set("message", err.Error())
	}

	// Redirect rather than rendering in place, so a refresh does not repeat
	// the update.
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
}

// progress reports one service's update-in-progress as JSON, for the page's
// poller. A service with nothing ever started under it is a 404: the poller
// only ever asks about a service it just clicked Update for, so this
// shouldn't happen outside of someone hand-editing the query string.
func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	p, ok := s.Applier.Progress(service)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(p)
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
		CanRefresh     bool
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
		CanRefresh:     s.Refresher != nil,
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
