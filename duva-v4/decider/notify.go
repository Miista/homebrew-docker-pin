package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/decide"
)

// ntfy publishes a line to a topic when something is waiting for approval.
//
// Configured the way diun is on these hosts -- an endpoint, a topic and a
// token, all from the environment -- because that is the notifier already set
// up here and a second convention would be one more thing to remember. The
// token is NTFY_TOKEN, the name the shared .ntfy.env already uses, so the
// decider joins that file rather than carrying a second copy of one secret.
//
// It is deliberately thin. A decider that could not tell anyone still decided
// correctly, so nothing here can change what the detector is told or what the
// queue holds: a failure is logged and dropped.
type ntfy struct {
	endpoint string
	topic    string
	token    string
	// clickURL, when set, is where tapping the notification goes. The
	// decider has no notion of its own public address -- it serves an API,
	// and the UI it would point at is a different container -- so this is
	// given rather than derived.
	clickURL string
	client   *http.Client
	log      zerolog.Logger
}

// newNtfy reads the configuration, returning nil when no endpoint is set.
//
// Nil means nobody is notified, which is a legitimate way to run a decider and
// the default: this host had no notifier before and must not gain one by
// upgrading.
func newNtfy(log zerolog.Logger) *ntfy {
	endpoint := strings.TrimSuffix(os.Getenv("DECIDER_NOTIF_NTFY_ENDPOINT"), "/")
	if endpoint == "" {
		return nil
	}
	topic := os.Getenv("DECIDER_NOTIF_NTFY_TOPIC")
	if topic == "" {
		// An endpoint with no topic is a half-configuration, and publishing
		// to a guessed topic would be worse than saying so.
		log.Warn().Msg("DECIDER_NOTIF_NTFY_ENDPOINT is set but DECIDER_NOTIF_NTFY_TOPIC is not, so nothing will be notified")
		return nil
	}
	n := &ntfy{
		endpoint: endpoint,
		topic:    topic,
		token:    os.Getenv("NTFY_TOKEN"),
		clickURL: os.Getenv("DECIDER_NOTIF_NTFY_CLICK"),
		// Short: a notification nobody is waiting on must not hold a
		// detector's request open.
		client: &http.Client{Timeout: 10 * time.Second},
		log:    log,
	}
	log.Info().Msgf("notifying %s on topic %q", endpoint, topic)
	return n
}

// announce publishes one entry. Never fatal, never blocking the decision.
func (n *ntfy) announce(host string, e decide.Entry) {
	title, body := message(host, e)

	req, err := http.NewRequest(http.MethodPost, n.endpoint+"/"+n.topic, bytes.NewBufferString(body))
	if err != nil {
		n.log.Warn().Msgf("could not build a notification for %s: %v", e.Service, err)
		return
	}
	req.Header.Set("Title", title)
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	if n.clickURL != "" {
		req.Header.Set("Click", n.clickURL)
	}

	res, err := n.client.Do(req)
	if err != nil {
		n.log.Warn().Msgf("could not notify about %s: %v", e.Service, err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		n.log.Warn().Msgf("notifying about %s was refused with %s", e.Service, res.Status)
	}
}

// message is what a person reads. Separate from the sending so it can be
// tested without a server.
//
// The title is what identifies the thing, because ntfy groups by it and two
// services of the same name on different hosts must not collapse together.
// The body is the change.
func message(host string, e decide.Entry) (title, body string) {
	title = e.Service
	if host != "" {
		title = fmt.Sprintf("%s on %s", e.Service, host)
	}

	// A digest move has no version pair -- Kind is empty for exactly that
	// reason -- so there is no "x to y" to write. What moved is the tag.
	if e.Kind == "" {
		if e.Digest != "" {
			return title, fmt.Sprintf("%s moved (%s)", e.Tag, shortDigest(e.Digest))
		}
		return title, fmt.Sprintf("%s moved", e.Tag)
	}
	return title, fmt.Sprintf("%s → %s (%s)", e.From, e.To, e.Kind)
}

// shortDigest is the first 12 hex characters, the length docker itself shows.
// A full digest is 71 characters and says nothing more to a person.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return "sha256:" + d
}
