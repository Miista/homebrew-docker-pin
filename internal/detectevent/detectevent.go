// Package detectevent translates our own detector's webhook into something
// the gate understands.
//
// The gate does not know which detector it has: every assumption about a
// detector's payload lives here, so pointing it at a different one means
// writing another of these rather than touching the gate.
//
// One shape at a time, deliberately. A gate that sniffed between several
// payload formats would be carrying translators for detectors nobody is
// running, and deciding which is which on every request. Putting a different
// detector in front of it is a translator there, not a branch here.
package detectevent

import (
	"encoding/json"
	"fmt"

	"github.com/Miista/homebrew-docker-pin/internal/decide"
)

// Event is what the detector publishes.
//
// Only the fields the gate reads are named. The detector sends more --
// when it observed the tag, which detector and version sent it -- and a
// struct listing them all would imply this cared.
type Event struct {
	// Container is how a finding is matched to a service: an image
	// reference is not unique, and a container name is.
	Container string `json:"container"`
	// Image is the base reference, without tag or digest.
	Image string `json:"image"`
	// Tag is the tag that appeared.
	Tag string `json:"tag"`
}

// Translate turns an event into a notice.
//
// Returns ok=false with a reason for anything unusable, which the caller logs
// rather than acting on. The detector reports observations, not judgements, so
// there is no equivalent of diun's "first sighting" to filter here -- every
// event is a tag that genuinely appeared.
func Translate(e Event) (decide.Notice, string, bool) {
	if e.Container == "" {
		return decide.Notice{}, "no container name, so nothing to match it to", false
	}
	if e.Image == "" || e.Tag == "" {
		return decide.Notice{}, "image or tag missing", false
	}
	return decide.Notice{
		Container: e.Container,
		// The gate wants a full reference. It reads the tag from it and
		// re-reads the compose file for what is pinned now.
		Image: e.Image + ":" + e.Tag,
		// No digest: the detector does not resolve one, because doing so
		// would be a registry request per finding to tell the gate
		// something the actor is better placed to learn when it pulls.
		Digest: "",
	}, "", true
}

// Parse reads a webhook body.
func Parse(body []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return Event{}, fmt.Errorf("parsing the detector's payload: %w", err)
	}
	return e, nil
}
