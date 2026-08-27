package help

import (
	"strings"
	"testing"
)

// These do not assert what the help text says -- comparing the text to itself
// proves only that a lookup works. They assert the invariants that actually
// break: a command gaining a subcommand nobody documented, or a topic whose
// usage line contradicts its own name.

// Every command the CLI dispatches must have a topic, or `docker pin help
// <cmd>` silently falls back to the general usage for a command that exists.
func TestPinTopics_CoverEveryDispatchedCommand(t *testing.T) {
	// Kept in step with the switch in cmd/docker-pin/main.go and the
	// subcommands in runSchedule. A new case there needs a topic here.
	for _, cmd := range []string{
		"pin", "upgrade", "list", "schedule", "version",
		"schedule apply", "schedule status", "schedule remove", "schedule run",
	} {
		if _, ok := For(PinTopics, cmd); !ok {
			t.Errorf("no help topic for %q", cmd)
		}
	}
}

func TestUnpinTopics_CoverEveryDispatchedCommand(t *testing.T) {
	for _, cmd := range []string{"unpin", "version"} {
		if _, ok := For(UnpinTopics, cmd); !ok {
			t.Errorf("no help topic for %q", cmd)
		}
	}
}

func TestFor_UnknownTopic(t *testing.T) {
	if text, ok := For(PinTopics, "nonesuch"); ok || text != "" {
		t.Errorf("unknown topic returned (%q, %v), want (\"\", false)", text, ok)
	}
}

// A topic with no text is worse than no topic: help prints an empty screen
// rather than falling back to the general usage.
func TestTopics_AreNonEmptyAndUnique(t *testing.T) {
	for _, ts := range []struct {
		name   string
		topics []Topic
	}{{"pin", PinTopics}, {"unpin", UnpinTopics}} {
		seen := map[string]bool{}
		for _, topic := range ts.topics {
			if topic.Cmd == "" {
				t.Errorf("%s: a topic has no command name", ts.name)
			}
			if strings.TrimSpace(topic.Text) == "" {
				t.Errorf("%s: topic %q has no text", ts.name, topic.Cmd)
			}
			if seen[topic.Cmd] {
				t.Errorf("%s: topic %q is defined twice; the first wins silently", ts.name, topic.Cmd)
			}
			seen[topic.Cmd] = true
		}
	}
}

// Every topic should show how to invoke the thing it documents, and name
// itself while doing so -- a copy-paste that leaves the wrong command in the
// usage line is easy to miss and actively misleading.
func TestTopics_UsageLineNamesTheCommand(t *testing.T) {
	for _, ts := range []struct {
		tool   string
		topics []Topic
	}{{"docker pin", PinTopics}, {"docker unpin", UnpinTopics}} {
		for _, topic := range ts.topics {
			if !strings.Contains(topic.Text, "Usage:") {
				t.Errorf("%s topic %q has no Usage: line", ts.tool, topic.Cmd)
				continue
			}
			// "pin" and "unpin" document the bare command, so the tool name
			// alone is the invocation; the rest must name their subcommand.
			if topic.Cmd == "pin" || topic.Cmd == "unpin" {
				continue
			}
			want := ts.tool + " " + topic.Cmd
			if !strings.Contains(topic.Text, want) {
				t.Errorf("%s topic %q never shows %q in its text", ts.tool, topic.Cmd, want)
			}
		}
	}
}

// PinUsage and UnpinUsage are what a bare invocation prints, so they must at
// least list the commands a user can reach.
func TestUsage_ListsTheMainCommands(t *testing.T) {
	for _, cmd := range []string{"upgrade", "list", "schedule"} {
		if !strings.Contains(PinUsage, cmd) {
			t.Errorf("PinUsage does not mention %q", cmd)
		}
	}
	if !strings.Contains(UnpinUsage, "unpin") {
		t.Error("UnpinUsage does not mention unpin")
	}
}
