package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/ui"
)

// parseDeciders reads the decider list, "host=url,host=url".
//
// Tokens are deliberately not in this string. A token is the authority to
// approve a container being replaced on that host, and putting it beside the
// URL would mean one variable that is partly a secret: it could not be shown
// in a log line, a `docker inspect`, or a compose file in git, which is where
// the rest of it belongs. Each host's token comes from its own variable
// instead, so rotating one is a one-line change rather than a rewrite of the
// list.
func parseDeciders(raw string, token func(host string) string) ([]ui.Decider, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []ui.Decider
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
		// Two deciders under one name would make a row ambiguous in exactly
		// the way the host label exists to prevent -- and worse than
		// ambiguous, since Approve routes on it and would send the click to
		// whichever was configured first.
		if seen[host] {
			return nil, fmt.Errorf("%q appears twice", host)
		}
		seen[host] = true
		out = append(out, ui.Decider{Host: host, URL: url, Token: token(host)})
	}
	return out, nil
}

// tokenEnv is the variable a host's token comes from: DUVA_UI_TOKEN_<HOST>,
// upper-cased with anything that cannot appear in an environment variable
// name replaced by an underscore, since a host name may contain "-" or ".".
func tokenEnv(host string) string {
	var b strings.Builder
	b.WriteString("DUVA_UI_TOKEN_")
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

// tokenFromEnv is the per-host token, falling back to a shared one.
//
// The shared fallback exists because the common case is one operator with one
// token they set on every decider; requiring a variable per host to express
// that would be ceremony. Per-host wins where both are set, so adding a host
// with its own token does not mean re-tokening the others.
func tokenFromEnv(host string) string {
	if t := os.Getenv(tokenEnv(host)); t != "" {
		return t
	}
	return os.Getenv("DUVA_UI_TOKEN")
}
