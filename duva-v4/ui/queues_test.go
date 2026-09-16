package main

import (
	"testing"
)

func noToken(string) string { return "" }

func TestParseQueues(t *testing.T) {
	got, err := parseQueues("optiplex=http://a:8080, pi=http://b:8080 ", noToken)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d queues, want 2", len(got))
	}
	if got[0].Host != "optiplex" || got[0].URL != "http://a:8080" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Host != "pi" || got[1].URL != "http://b:8080" {
		t.Errorf("second = %+v", got[1])
	}
}

// The negative cases. Each of these is a config a person would write by
// mistake, and each must be refused rather than half-understood: a queue
// silently dropped is a host whose queue is never shown.
func TestParseQueuesRejects(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"no url", "optiplex"},
		{"empty host", "=http://a:8080"},
		{"empty url", "optiplex="},
		{"duplicate host", "optiplex=http://a:8080,optiplex=http://b:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseQueues(tc.raw, noToken); err == nil {
				t.Errorf("parseQueues(%q) was accepted", tc.raw)
			}
		})
	}
}

// Empty is not an error here -- main decides what no queues means, and does
// refuse. Parsing and policy are separate so the refusal has one home.
func TestParseQueuesEmpty(t *testing.T) {
	got, err := parseQueues("  ", noToken)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d queues, want none", len(got))
	}
}

func TestTokenEnv(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"optiplex", "DUVA_QUEUE_TOKEN_OPTIPLEX"},
		// A host name may carry characters an environment variable name
		// cannot; they become underscores rather than being dropped, so two
		// different hosts cannot collapse onto one variable.
		{"raspberry.local", "DUVA_QUEUE_TOKEN_RASPBERRY_LOCAL"},
		{"family-dashboard", "DUVA_QUEUE_TOKEN_FAMILY_DASHBOARD"},
	} {
		if got := tokenEnv(tc.host); got != tc.want {
			t.Errorf("tokenEnv(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// Per-host wins over the shared token, so adding a host with its own token
// does not mean re-tokening the others.
// A token is per host, and there is no shared fallback.
//
// Each queue mints its own when none is injected, so in practice every host
// has a different one. A "shared" variable ended up holding whichever host was
// configured first -- and the others then failed with a token mismatch rather
// than with "no token for that host", which is the harder failure to read.
func TestATokenIsPerHost(t *testing.T) {
	t.Setenv("DUVA_QUEUE_TOKEN", "not-a-fallback")
	t.Setenv("DUVA_QUEUE_TOKEN_PI", "just-the-pi")
	t.Setenv("DUVA_QUEUE_TOKEN_OPTIPLEX", "just-optiplex")

	got, err := parseQueues("optiplex=http://a:8080,pi=http://b:8080", tokenFromEnv)
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

// A host with no token of its own gets none, rather than silently borrowing
// another host's. The UI warns and that queue reads as unreachable, which is
// the honest answer.
func TestAHostWithoutATokenGetsNone(t *testing.T) {
	t.Setenv("DUVA_QUEUE_TOKEN", "not-a-fallback")
	t.Setenv("DUVA_QUEUE_TOKEN_PI", "just-the-pi")

	got, err := parseQueues("optiplex=http://a:8080,pi=http://b:8080", tokenFromEnv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[0].Token != "" {
		t.Errorf("optiplex token = %q, want empty -- it has none of its own", got[0].Token)
	}
}
