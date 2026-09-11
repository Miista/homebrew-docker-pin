package main

import (
	"testing"
)

func noToken(string) string { return "" }

func TestParseDeciders(t *testing.T) {
	got, err := parseDeciders("optiplex=http://a:8080, pi=http://b:8080 ", noToken)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d deciders, want 2", len(got))
	}
	if got[0].Host != "optiplex" || got[0].URL != "http://a:8080" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Host != "pi" || got[1].URL != "http://b:8080" {
		t.Errorf("second = %+v", got[1])
	}
}

// The negative cases. Each of these is a config a person would write by
// mistake, and each must be refused rather than half-understood: a decider
// silently dropped is a host whose queue is never shown.
func TestParseDecidersRejects(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"no url", "optiplex"},
		{"empty host", "=http://a:8080"},
		{"empty url", "optiplex="},
		{"duplicate host", "optiplex=http://a:8080,optiplex=http://b:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDeciders(tc.raw, noToken); err == nil {
				t.Errorf("parseDeciders(%q) was accepted", tc.raw)
			}
		})
	}
}

// Empty is not an error here -- main decides what no deciders means, and does
// refuse. Parsing and policy are separate so the refusal has one home.
func TestParseDecidersEmpty(t *testing.T) {
	got, err := parseDeciders("  ", noToken)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d deciders, want none", len(got))
	}
}

func TestTokenEnv(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"optiplex", "DUVA_UI_TOKEN_OPTIPLEX"},
		// A host name may carry characters an environment variable name
		// cannot; they become underscores rather than being dropped, so two
		// different hosts cannot collapse onto one variable.
		{"raspberry.local", "DUVA_UI_TOKEN_RASPBERRY_LOCAL"},
		{"family-dashboard", "DUVA_UI_TOKEN_FAMILY_DASHBOARD"},
	} {
		if got := tokenEnv(tc.host); got != tc.want {
			t.Errorf("tokenEnv(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// Per-host wins over the shared token, so adding a host with its own token
// does not mean re-tokening the others.
func TestPerHostTokenWins(t *testing.T) {
	t.Setenv("DUVA_UI_TOKEN", "shared")
	t.Setenv("DUVA_UI_TOKEN_PI", "just-the-pi")

	got, err := parseDeciders("optiplex=http://a:8080,pi=http://b:8080", tokenFromEnv)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[0].Token != "shared" {
		t.Errorf("optiplex token = %q, want the shared one", got[0].Token)
	}
	if got[1].Token != "just-the-pi" {
		t.Errorf("pi token = %q, want its own", got[1].Token)
	}
}
