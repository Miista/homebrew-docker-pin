package webui

import (
	"testing"
)

func noToken(string) string { return "" }

func TestParsePeers(t *testing.T) {
	got, err := parseAgents("optiplex=http://a:8080, pi=http://b:8080 ", noToken)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d peers, want 2", len(got))
	}
	if got[0].Host != "optiplex" || got[0].URL != "http://a:8080" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Host != "pi" || got[1].URL != "http://b:8080" {
		t.Errorf("second = %+v", got[1])
	}
}

// The negative cases. Each of these is a config a person would write by
// mistake, and each must be refused rather than half-understood: a peer
// silently dropped is a host whose queue is never shown.
func TestParsePeersRejects(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"no url", "optiplex"},
		{"empty host", "=http://a:8080"},
		{"empty url", "optiplex="},
		{"duplicate host", "optiplex=http://a:8080,optiplex=http://b:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseAgents(tc.raw, noToken); err == nil {
				t.Errorf("parseAgents(%q) was accepted", tc.raw)
			}
		})
	}
}

// Empty is not an error here -- Run decides what no queues means, and does
// refuse. Parsing and policy are separate so the refusal has one home.
func TestParsePeersEmpty(t *testing.T) {
	got, err := parseAgents("  ", noToken)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d peers, want none", len(got))
	}
}

// A token is per peer, and this host's own is not a fallback for one.
//
// In practice every host has a different token. A "shared" variable ended up
// holding whichever host was configured first -- and the others then failed
// with a token mismatch rather than with "no token for that host", which is
// the harder failure to read.
func TestATokenIsPerPeer(t *testing.T) {
	t.Setenv("DUVA_SETTING_API_TOKEN", "not-a-fallback")
	t.Setenv("DUVA_SETTING_AGENT_PI_TOKEN", "just-the-pi")
	t.Setenv("DUVA_SETTING_AGENT_OPTIPLEX_TOKEN", "just-optiplex")

	got, err := parseAgents("optiplex=http://a:8080,pi=http://b:8080", agentToken)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[0].Token != "just-optiplex" {
		t.Errorf("optiplex token = %q, want its own", got[0].Token)
	}
	if got[1].Token != "just-the-pi" {
		t.Errorf("pi token = %q, want its own", got[1].Token)
	}
}

// A peer with no token of its own gets none, rather than silently borrowing
// another host's. The page warns and that host reads as unreachable, which is
// the honest answer.
func TestAPeerWithoutATokenGetsNone(t *testing.T) {
	t.Setenv("DUVA_SETTING_API_TOKEN", "not-a-fallback")
	t.Setenv("DUVA_SETTING_AGENT_PI_TOKEN", "just-the-pi")

	got, err := parseAgents("optiplex=http://a:8080,pi=http://b:8080", agentToken)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[0].Token != "" {
		t.Errorf("optiplex token = %q, want empty -- it has none of its own", got[0].Token)
	}
}
