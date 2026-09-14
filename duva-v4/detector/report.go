package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/detect"
)

// reporter POSTs findings to a webhook.
//
// A webhook, not "the decider". The detector does not know what is at the
// other end and has no opinion about it: it publishes what it found, and
// whatever subscribes decides what that means. A gate, a notifier, a script
// that appends to a file -- all equally valid, and none of them something
// this has to be taught about.
//
// That is why the payload is the detector's own shape rather than any
// consumer's. Sending diun's shape, or the gate's, would be this tool
// pretending to be a thing it is not, and would make swapping the consumer a
// change here rather than there.
type reporter struct {
	url  string
	host string
	log  zerolog.Logger
	http *http.Client

	// unreachable records that the webhook's host could not be resolved or
	// dialled, so the rest of this run stops trying.
	//
	// Not a retry policy -- it is the opposite. A run over ~45 services can
	// produce hundreds of findings, and a webhook whose name does not resolve
	// costs one DNS lookup each: enough to exhaust Pi-hole's default
	// 1000-queries-a-minute allowance and get every subsequent lookup in the
	// run answered REFUSED, including the registry ones this tool actually
	// needs. One unreachable webhook must not cost the detector its ability
	// to detect.
	//
	// Reset per run rather than persisted: the next scheduled run gets a
	// clean attempt, which is where "retry is a property of running again"
	// still holds.
	unreachable bool
	// skipped counts what was dropped after that, so the run says so once at
	// the end rather than either lying about publishing or printing hundreds
	// of identical errors.
	skipped int
	// refused counts findings the webhook answered but would not accept.
	// Separate from skipped, because a consumer that is there and saying no
	// is a different problem from one that is not there at all.
	refused int
	// failed counts every finding that did not land, whatever the reason --
	// refused, unreachable, or skipped after the webhook was written off.
	//
	// It is what decides whether the cutoff may advance. A finding this run
	// detected and could not hand on is a hole exactly like a service it
	// could not check: move the line past it and it is older than the cutoff
	// forever, so nothing ever hears about it.
	failed int
}

// unpublished is how many findings this run detected but did not hand on.
func (r *reporter) unpublished() int { return r.failed }

func newReporter(url, host string, log zerolog.Logger) *reporter {
	if url == "" {
		// Legitimate, not broken. A detector with nowhere to publish is one
		// being read through its log -- which is exactly how you point it at
		// a real project without wiring anything to it.
		log.Info().Msg("no DETECTOR_WEBHOOK_URL: findings will be logged and not published anywhere")
	}
	r := &reporter{
		url:  url,
		host: host,
		log:  log,
		http: &http.Client{Timeout: 15 * time.Second},
	}
	if url != "" {
		if err := dialWebhook(url); err != nil {
			// Said once, here, and not again per finding. Not a refusal to
			// start: a detector whose webhook is down still logs what it
			// found, which is how you point it at a project before wiring a
			// consumer to it.
			//
			// Not marked unreachable either -- that is this run's business,
			// and a webhook that is down now may be up by the time the first
			// finding is ready. The per-finding path writes it off on a real
			// failure; this only tells the operator.
			log.Warn().Msgf("the webhook at %s cannot be reached right now (%v)", url, err)
		}
	}
	return r
}

// dialWebhook opens a TCP connection to a webhook URL and closes it.
//
// Only the dial, not a request: whether the thing at the other end accepts a
// finding is its business and varies per finding. Whether the name resolves
// and the port answers is a fact about the deployment, and it is the one that
// produces hundreds of identical errors when it is wrong.
//
// Shared by startup and the health subcommand, so "reachable" means the same
// thing in the log and in `docker ps`.
func dialWebhook(rawURL string) error {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return err
	}
	host := u.Host
	if u.Port() == "" {
		// Dial needs a port even where a URL does not.
		if u.Scheme == "https" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Event is what the detector publishes: one tag, and when it appeared.
//
// Everything here is something the detector observed. There is no digest,
// because resolving one would be a request per finding to tell a consumer
// something it can look up itself; no "current version", because comparing is
// not this tool's job; and no verdict, for the same reason.
type Event struct {
	// Detector names the tool and its version, so a consumer reading a
	// payload it does not recognise can tell what sent it.
	Detector string `json:"detector"`
	// Host is where the observing happened.
	Host string `json:"host"`
	// Service is the compose service, and Container its container name --
	// which is what a consumer matches back to its own view of the host.
	Service   string `json:"service"`
	Container string `json:"container"`
	// Image is the base reference, without tag or digest.
	Image string `json:"image"`
	// Tag is the tag that appeared.
	Tag string `json:"tag"`
	// Published is when it appeared, RFC 3339.
	Published string `json:"published"`
	// ObservedAt is when this detector noticed, which is not the same thing:
	// a tag published during an outage is observed late, and a consumer
	// reasoning about staleness wants both.
	ObservedAt string `json:"observed_at"`
}

// report publishes one finding.
//
// Failures are logged and not retried. The detector is stateless and runs
// again on a schedule, so a finding that did not land this time lands next
// time -- retry is a property of running repeatedly rather than something
// either side implements.
func (r *reporter) report(f detect.Finding) {
	if r.url == "" {
		return
	}
	// The webhook was already found unreachable this run. Every further
	// attempt would be another DNS lookup for a name that does not resolve,
	// which is how a detector becomes a flood.
	if r.unreachable {
		r.skipped++
		r.failed++
		return
	}

	body, err := json.Marshal(Event{
		Detector:   "detector/" + version,
		Host:       r.host,
		Service:    f.Service,
		Container:  f.Container,
		Image:      f.Image,
		Tag:        f.Tag,
		Published:  f.Published.UTC().Format(time.RFC3339),
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.log.Error().Msgf("%s: could not encode the finding — %v", f.Service, err)
		r.failed++
		return
	}

	resp, err := r.http.Post(r.url, "application/json", bytes.NewReader(body))
	if err != nil {
		r.failed++
		// Could not resolve or dial it at all: that is a fact about the
		// webhook, not about this finding, and it will be just as true for
		// the next several hundred. Stop for the rest of the run.
		//
		// Said once, at WARN, and only if startup did not already say it --
		// a webhook that went down mid-run is news, one that was down before
		// the first finding is not. Everything after is counted and reported
		// by summarise, because several hundred identical lines is not more
		// informative than one.
		if isUnreachable(err) {
			r.unreachable = true
			r.log.Warn().Msg("the webhook stopped answering, so the rest of this run's findings will not be published")
			return
		}
		// Something else went wrong with this one request. Debug rather than
		// error: the run's own summary is where an operator learns how many
		// findings did not land, and one line per finding buries it.
		r.log.Debug().Msgf("%s: could not publish %s — %v", f.Service, f.Tag, err)
		return
	}
	// Drained before closing, or the connection is not returned to the pool
	// and every finding costs a fresh dial -- the same reuse problem, on the
	// path where the webhook is working.
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		// What is at the other end refused it. Said plainly rather than
		// interpreted: this tool does not know what a 422 means to whatever
		// is subscribed, only that it did not accept the finding.
		//
		// The first refusal is worth a line -- it carries the status, which
		// is the thing that explains the rest. After that they are counted:
		// a consumer refusing everything produces one refusal per finding,
		// and the hundredth says nothing the first did not.
		r.failed++
		r.refused++
		if r.refused == 1 {
			r.log.Warn().Msgf("the webhook refused %s for %s with %s", f.Tag, f.Service, resp.Status)
		}
		return
	}
	r.log.Debug().Msgf("%s: published %s", f.Service, f.Tag)
}

// isUnreachable says whether an error means the webhook's host cannot be
// reached at all, as opposed to one request going wrong.
//
// A name that does not resolve and a connection that is refused are both
// facts about the endpoint that will hold for every remaining finding in the
// run. A timeout is not: it may be one slow request, and a consumer that is
// merely slow should not be written off for the rest of the run.
func isUnreachable(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		// A dial that timed out is the slow case, not the absent one.
		if opErr.Timeout() {
			return false
		}
		return true
	}
	return false
}

// summarise says once, at the end of a run, what was not published.
//
// One line rather than one per finding: several hundred identical errors is
// not more information than the fact, and it buries the findings themselves.
// summarise says once, at the end, what the per-finding path stayed quiet
// about. This is where an operator learns how much did not land.
func (r *reporter) summarise() {
	if r.skipped > 0 {
		r.log.Warn().Msgf("%d finding(s) were not published, because the webhook could not be reached", r.skipped)
	}
	if r.refused > 1 {
		r.log.Warn().Msgf("%d finding(s) were refused by the webhook", r.refused)
	}
}
