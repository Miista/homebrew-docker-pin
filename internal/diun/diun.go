// Package diun translates diun's webhook into something the decider
// understands.
//
// It exists so that the decider does not know which detector it has. Every
// assumption about diun's payload lives here: the field names, that `status`
// distinguishes a first sighting from a move, that the container name arrives
// under `metadata.ctn_names`. Swapping detectors means writing another one of
// these, not touching the gate.
package diun

import (
	"encoding/json"
	"fmt"

	"github.com/Miista/homebrew-docker-pin/internal/decide"
)

// Payload is diun's webhook body.
//
// Only the fields that matter are named. diun sends more — diun_version,
// hub_link, mime_type, platform, created, and the rest of the container
// metadata — and a struct that listed them all would imply this cared.
type Payload struct {
	// Status is "new" on a first sighting and "update" when something moved.
	Status string `json:"status"`
	// Provider is which diun provider found it: "docker", "file", and so on.
	Provider string `json:"provider"`
	// Image is the full reference, tag included.
	Image string `json:"image"`
	// Digest is what that reference resolves to now.
	Digest string `json:"digest"`

	Metadata struct {
		// Names is the container's name. diun's own field is plural and
		// carries one name for a docker-provider notification.
		Names string `json:"ctn_names"`
		// State is "running", "exited" and so on.
		State string `json:"ctn_state"`
	} `json:"metadata"`
}

// StatusUpdate is diun reporting that something moved, as opposed to
// recording a baseline.
const StatusUpdate = "update"

// StatusNew is diun's first sighting of an image. Not a signal: it means diun
// had nothing to compare against, not that anything changed.
const StatusNew = "new"

// Translate turns a payload into a notice.
//
// Returns ok=false for anything that is not a signal, with a reason — the
// caller logs it rather than acting on it. Distinguishing "not a signal" from
// "malformed" is deliberate: the first is routine and the second is worth
// someone's attention.
func Translate(p Payload) (decide.Notice, string, bool) {
	// A first sighting is diun recording a baseline, not reporting a change.
	// Acting on one would mean treating every newly-watched image as an
	// update to itself.
	if p.Status == StatusNew {
		return decide.Notice{}, "first sighting, not a change", false
	}
	if p.Status != StatusUpdate {
		return decide.Notice{}, fmt.Sprintf("unrecognised status %q", p.Status), false
	}

	// Only the docker provider names a container, and the container name is
	// the only thing that maps a notification to a service. A file-provider
	// notification describes an image nothing on this host necessarily runs.
	if p.Metadata.Names == "" {
		return decide.Notice{}, "no container name, so nothing to match it to", false
	}
	if p.Image == "" || p.Digest == "" {
		return decide.Notice{}, "image or digest missing", false
	}

	return decide.Notice{
		Container: p.Metadata.Names,
		Image:     p.Image,
		Digest:    p.Digest,
	}, "", true
}

// Parse reads a webhook body.
func Parse(body []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Payload{}, fmt.Errorf("parsing diun's payload: %w", err)
	}
	return p, nil
}
