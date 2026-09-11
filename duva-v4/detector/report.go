package main

import (
	"bytes"
	"encoding/json"
	"net/http"
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
}

func newReporter(url, host string, log zerolog.Logger) *reporter {
	if url == "" {
		// Legitimate, not broken. A detector with nowhere to publish is one
		// being read through its log -- which is exactly how you point it at
		// a real project without wiring anything to it.
		log.Info().Msg("no DETECTOR_WEBHOOK_URL: findings will be logged and not published anywhere")
	}
	return &reporter{
		url:  url,
		host: host,
		log:  log,
		http: &http.Client{Timeout: 15 * time.Second},
	}
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
		return
	}

	resp, err := r.http.Post(r.url, "application/json", bytes.NewReader(body))
	if err != nil {
		r.log.Error().Msgf("%s: could not publish %s — %v", f.Service, f.Tag, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// What is at the other end refused it. Said plainly rather than
		// interpreted: this tool does not know what a 422 means to whatever
		// is subscribed, only that it did not accept the finding.
		r.log.Error().Msgf("%s: the webhook refused %s with %s", f.Service, f.Tag, resp.Status)
		return
	}
	r.log.Debug().Msgf("%s: published %s", f.Service, f.Tag)
}
