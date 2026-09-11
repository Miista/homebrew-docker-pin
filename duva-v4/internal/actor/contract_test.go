package actor

import (
	"encoding/json"
	"testing"
)

func TestTerminalRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		status Status
		reason string
	}{
		{Completed, ""},
		{Failed, "the container refused the image"},
		{Completed, "opened pull request #12"},
	} {
		line := Terminal(tc.status, tc.reason)
		gotStatus, gotReason, ok := ParseTerminal(line)
		if !ok {
			t.Errorf("ParseTerminal(%q) did not recognise its own output", line)
			continue
		}
		if gotStatus != tc.status || gotReason != tc.reason {
			t.Errorf("round trip of (%q, %q) gave (%q, %q) via %q",
				tc.status, tc.reason, gotStatus, gotReason, line)
		}
	}
}

// Everything that is not a terminal line is progress, and progress is none of
// the decider's business. Misreading one as the end would cut a stream short.
func TestProgressLinesAreNotTerminal(t *testing.T) {
	for _, line := range []string{
		"pulling example.com/app:1.2.3",
		"recreating app",
		"",
		"statusless",
		"the status: is completed", // prefix not at the start
		"Status: completed",        // wrong case
	} {
		if _, _, ok := ParseTerminal(line); ok {
			t.Errorf("ParseTerminal(%q) claimed to be terminal", line)
		}
	}
}

// A status nobody defined must not end the stream. Treating an unrecognised
// one as the end would let a typo in an actor read as success; leaving it as
// progress means the decider waits and its timeout catches it.
func TestUnknownStatusIsNotTerminal(t *testing.T) {
	for _, line := range []string{
		"status: done",
		"status: ok",
		"status: in-progress", // real constant, but never terminal
		"status: ",
	} {
		if s, _, ok := ParseTerminal(line); ok {
			t.Errorf("ParseTerminal(%q) returned %q as terminal", line, s)
		}
	}
}

func TestParseTerminalTrimsLineEndings(t *testing.T) {
	for _, line := range []string{
		"status: completed\n",
		"status: completed\r\n",
	} {
		if _, _, ok := ParseTerminal(line); !ok {
			t.Errorf("ParseTerminal(%q) failed on a line ending", line)
		}
	}
}

// The reason survives a colon in its text -- "failed: could not pull: 404"
// must not lose the tail.
func TestReasonKeepsLaterColons(t *testing.T) {
	_, reason, ok := ParseTerminal("status: failed: could not pull: 404 not found")
	if !ok {
		t.Fatal("not recognised as terminal")
	}
	if reason != "could not pull: 404 not found" {
		t.Errorf("reason = %q, want the whole tail", reason)
	}
}

// The request is what crosses the wire, so its JSON shape is part of the
// contract: a field renamed here is a different actor's parse failing.
func TestRequestJSONShape(t *testing.T) {
	raw, err := json.Marshal(Request{
		Service: "authelia",
		File:    "/compose/pi/docker-compose.yml",
		Image:   "docker.io/authelia/authelia",
		From:    "4.39.20",
		To:      "4.39.25",
		Tag:     "4.39.25",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"service", "file", "image", "from", "to", "tag"} {
		if _, ok := got[field]; !ok {
			t.Errorf("the payload is missing %q: %s", field, raw)
		}
	}
	// Digest is omitted when empty: a version change has none, and sending
	// "digest": "" would invite an actor to pin nothing.
	if _, ok := got["digest"]; ok {
		t.Errorf("an empty digest should be omitted, got %s", raw)
	}
}

func TestRequestCarriesDigestWhenKnown(t *testing.T) {
	raw, _ := json.Marshal(Request{Service: "jellyfin", Digest: "sha256:abc"})
	var got map[string]any
	json.Unmarshal(raw, &got)
	if got["digest"] != "sha256:abc" {
		t.Errorf("digest missing from %s", raw)
	}
}

func TestAcceptedJSONShape(t *testing.T) {
	raw, _ := json.Marshal(Accepted{Stream: "/v1/stream/abc"})
	var got map[string]any
	json.Unmarshal(raw, &got)
	if got["stream"] != "/v1/stream/abc" {
		t.Errorf("stream missing from %s", raw)
	}
}
