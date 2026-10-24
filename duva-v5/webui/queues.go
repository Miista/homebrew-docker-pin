package webui

import (
	"fmt"
	"strings"

	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/ui"
)

// parsePeers reads the peers setting, "host=url,host=url": the other hosts
// running duva, whose queues the page shows beside this host's own.
//
// Tokens are deliberately not in this string. A token is the authority to
// approve a container being replaced on that host, and putting it beside the
// URL would mean one setting that is partly a secret: it could not be shown
// in a log line, a `docker inspect`, or a compose file in git, which is where
// the rest of it belongs. Each peer's token is its own setting instead, so
// rotating one is a one-line change rather than a rewrite of the list.
func parseAgents(raw string, token func(host string) string) ([]ui.Upstream, error) {
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
		// Two peers under one name would make a row ambiguous in exactly the
		// way the host label exists to prevent -- and worse than ambiguous,
		// since Approve routes on it and would send the click to whichever
		// was configured first.
		if seen[host] {
			return nil, fmt.Errorf("%q appears twice", host)
		}
		seen[host] = true
		out = append(out, ui.Upstream{Host: host, URL: url, Token: token(host)})
	}
	return out, nil
}

// agentToken is an agent's API token: agent.<host>.token, which is what that
// host reads as its api_token.
//
// Per agent, with no shared fallback. v4 had one, for the operator with a
// single token set on every queue -- but in practice every host has a
// different one, and the "shared" variable ended up holding whichever host
// was configured first. A default that is really one host's secret is worse
// than no default: the others fail with a token mismatch rather than with "no
// token for that host".
func agentToken(host string) string {
	return settings.Value(settings.AgentToken(host))
}
