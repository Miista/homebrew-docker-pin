package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/Miista/homebrew-docker-pin/internal/registry"
)

// State is what duva remembers between runs. It lives as one JSON file on the
// /data volume.
//
// The previous version kept a single service -> string map doing two jobs at
// once: the moving-tag baseline and the last-notified marker. Those are
// separate concerns — one is "where did the tag point last time I looked", the
// other is "what have I already told you about" — and a third is now needed
// (what is waiting for approval, which the UI must survive a restart to show).
type State struct {
	// Baseline records the digest each moving-tag service pointed at when
	// last checked, so a move is reported once rather than every run.
	Baseline Baseline `json:"baseline"`
	// Notified records the candidate last announced per service, so a
	// notification is not repeated for the same candidate.
	Notified map[string]string `json:"notified"`
	// Pending is what is waiting for a human, keyed by service.
	Pending map[string]Pending `json:"pending"`
}

// Pending is an update duva will not apply on its own. It carries everything
// the UI needs to render a row and the actor needs to apply it, so neither has
// to re-derive it from the compose file.
type Pending struct {
	Service string `json:"service"`
	File    string `json:"file"`
	Image   string `json:"image"`
	// CurrentTag and CurrentDigest are what the compose file pins now.
	CurrentTag    string `json:"current_tag"`
	CurrentDigest string `json:"current_digest"`
	// Kind is "tag" or "digest"; Candidate is the newer tag or the moved
	// digest respectively.
	Kind      Kind   `json:"kind"`
	Candidate string `json:"candidate"`
	// Bump is how big the version change is, for a tag candidate. Empty for a
	// digest candidate, which has no version pair to compare.
	Bump registry.Kind `json:"bump,omitempty"`
	// Reason says why this needs a human rather than being applied: the
	// classification, or that it could not be classified.
	Reason string `json:"reason"`
	// FirstSeen is when this candidate first showed up, so the UI can show
	// how long something has been waiting. RFC 3339.
	FirstSeen string `json:"first_seen"`
}

// NewState returns an empty state with its maps ready to use.
func NewState() *State {
	return &State{
		Baseline: Baseline{},
		Notified: map[string]string{},
		Pending:  map[string]Pending{},
	}
}

// LoadState reads state from path. A missing file is not an error: it means
// nothing has been seen yet.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewState(), nil
	}
	if err != nil {
		return nil, err
	}
	st := NewState()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	// A file written by an older version, or a hand-edited one, can leave
	// any of these nil; callers should not have to check.
	if st.Baseline == nil {
		st.Baseline = Baseline{}
	}
	if st.Notified == nil {
		st.Notified = map[string]string{}
	}
	if st.Pending == nil {
		st.Pending = map[string]Pending{}
	}
	return st, nil
}

// Save writes state to path, creating its parent directory if needed. The
// write is atomic: a temp file in the same directory, then a rename, so a
// crash mid-write cannot leave a truncated state file behind.
func (s *State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".duva-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Reconcile brings Pending into line with what was just detected: findings
// that need approval are added or refreshed, and anything no longer waiting is
// dropped.
//
// Dropping matters as much as adding. A pending entry must disappear when the
// candidate is superseded by a newer one, when the update is applied, when the
// service stops being watched, or when it is removed from the compose file
// entirely — otherwise the queue accumulates rows for updates that no longer
// exist, and a stale row is worse than no row: it invites approving something
// that is gone.
//
// now is passed in so callers control the clock (and tests can pin it).
func (s *State) Reconcile(needApproval []Finding, seen []Finding, now string) {
	wanted := make(map[string]Finding, len(needApproval))
	for _, f := range needApproval {
		wanted[f.Service] = f
	}

	// Drop entries for services that were checked this run but no longer
	// need approval. Services NOT checked this run are left alone: a
	// transient error must not silently empty the queue.
	checked := make(map[string]bool, len(seen))
	for _, f := range seen {
		if f.Status != StatusError {
			checked[f.Service] = true
		}
	}
	for name := range s.Pending {
		if _, still := wanted[name]; !still && checked[name] {
			delete(s.Pending, name)
		}
	}

	for name, f := range wanted {
		prev, existed := s.Pending[name]
		firstSeen := now
		// Keep the original timestamp while it is the same candidate, so
		// "waiting since" means what it says.
		if existed && prev.Candidate == f.Candidate {
			firstSeen = prev.FirstSeen
		}
		s.Pending[name] = Pending{
			Service:       f.Service,
			File:          f.File,
			Image:         f.Image,
			CurrentTag:    f.CurrentTag,
			CurrentDigest: f.CurrentDigest,
			Kind:          f.Kind,
			Candidate:     f.Candidate,
			Bump:          f.Bump,
			Reason:        f.Reason,
			FirstSeen:     firstSeen,
		}
	}
}

// PendingList returns the pending updates sorted by service name, so the UI
// and any test see a stable order.
func (s *State) PendingList() []Pending {
	out := make([]Pending, 0, len(s.Pending))
	for _, p := range s.Pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}
