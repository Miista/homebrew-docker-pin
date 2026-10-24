package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/ui"
)

// parseQueues reads the queue list, "host=url,host=url".
//
// Tokens are deliberately not in this string. A token is the authority to
// approve a container being replaced on that host, and putting it beside the
// URL would mean one variable that is partly a secret: it could not be shown
// in a log line, a `docker inspect`, or a compose file in git, which is where
// the rest of it belongs. Each host's token comes from its own variable
// instead, so rotating one is a one-line change rather than a rewrite of the
// list.
func parseQueues(raw string, token func(host string) string) ([]ui.Upstream, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []ui.Upstream
	seen := map[string]bool{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host, url, ok := strings.Cut(entry, "=")
		host, url = strings.TrimSpace(host), strings.TrimSpace(url)
		if !ok || host == "" || url == "" {
			return nil, fmt.Errorf("%q is not host=url", entry)
		}
		// Two queues under one name would make a row ambiguous in exactly
		// the way the host label exists to prevent -- and worse than
		// ambiguous, since Approve routes on it and would send the click to
		// whichever was configured first.
		if seen[host] {
			return nil, fmt.Errorf("%q appears twice", host)
		}
		seen[host] = true
		out = append(out, ui.Upstream{Host: host, URL: url, Token: token(host)})
	}
	return out, nil
}

// tokenEnv is the variable a host's token comes from: DUVA_QUEUE_TOKEN_<HOST>,
// upper-cased with anything that cannot appear in an environment variable
// name replaced by an underscore, since a host name may contain "-" or ".".
func tokenEnv(host string) string {
	var b strings.Builder
	b.WriteString("DUVA_QUEUE_TOKEN_")
	for _, r := range strings.ToUpper(host) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// tokenFromEnv is a host's queue token.
//
// Per host, with no shared fallback. There used to be one, for the operator
// with a single token set on every queue -- but each queue mints its own when
// none is injected, so in practice every host has a different one, and the
// "shared" variable ended up holding whichever host was configured first. A
// default that is really one host's secret is worse than no default: the
// others fail with a token mismatch rather than with "no token for that host".
//
// The name is the queue's own -- DUVA_QUEUE_TOKEN_<HOST>, matching the
// DUVA_QUEUE_TOKEN that host's queue reads -- because what is being named is
// the door, not the process holding the key.
func tokenFromEnv(host string) string {
	return os.Getenv(tokenEnv(host))
}
