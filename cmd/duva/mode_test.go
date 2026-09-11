package main

import "testing"

func TestParseModeDefaultsToLocal(t *testing.T) {
	// Unset must stay what duva has always been: a single-host install that
	// was never configured keeps working across this change.
	if got := parseMode(""); got != ModeLocal {
		t.Errorf("parseMode(%q) = %q, want local", "", got)
	}
}

func TestParseModeAccepts(t *testing.T) {
	for raw, want := range map[string]Mode{
		"local": ModeLocal,
		"agent": ModeAgent,
		"hub":   ModeHub,
	} {
		if got := parseMode(raw); got != want {
			t.Errorf("parseMode(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseAgentsEmpty(t *testing.T) {
	got, err := parseAgents("")
	if err != nil {
		t.Fatalf("parseAgents(\"\"): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestParseAgentsReadsHostAndURL(t *testing.T) {
	got, err := parseAgents("optiplex=http://192.0.2.10:10256,pi=http://192.0.2.12:10256")
	if err != nil {
		t.Fatalf("parseAgents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Host != "optiplex" || got[0].URL != "http://192.0.2.10:10256" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Host != "pi" || got[1].URL != "http://192.0.2.12:10256" {
		t.Errorf("second = %+v", got[1])
	}
}

func TestParseAgentsTolerantOfSpacing(t *testing.T) {
	got, err := parseAgents("  optiplex = http://a:1 , pi = http://b:2 ")
	if err != nil {
		t.Fatalf("parseAgents: %v", err)
	}
	if len(got) != 2 || got[0].Host != "optiplex" || got[1].URL != "http://b:2" {
		t.Errorf("got %+v", got)
	}
}

// A malformed entry is an error, not something skipped. A hub silently
// dropping an agent it could not parse would show a queue missing a host,
// which reads as "that host is fine".
func TestParseAgentsRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		"optiplex",               // no url
		"=http://a:1",            // no host
		"optiplex=",              // no url
		"optiplex=http://a:1,pi", // second entry malformed
	} {
		if _, err := parseAgents(raw); err == nil {
			t.Errorf("parseAgents(%q) accepted; want an error", raw)
		}
	}
}

// Two agents under one name would make a row ambiguous in exactly the way
// the host label exists to prevent, and applies would route to whichever came
// first.
func TestParseAgentsRejectsDuplicateHost(t *testing.T) {
	if _, err := parseAgents("pi=http://a:1,pi=http://b:2"); err == nil {
		t.Fatal("want an error for a repeated host")
	}
}
