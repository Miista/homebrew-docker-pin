package watch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Miista/homebrew-docker-pin/oci/version"
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
	// Soaking is what duva.delay is holding back, keyed by service.
	//
	// Kept apart from Pending because they are different situations, even
	// though an operator can act on both: a pending update is waiting for a
	// decision, a soaking one has been decided and is waiting for time. Taking
	// a soaking update early is overriding the soak, and code that cannot tell
	// them apart could do that without meaning to.
	Soaking map[string]Soaking `json:"soaking"`
}

// Soaking is a candidate duva.delay is holding back.
type Soaking struct {
	Service string `json:"service"`
	// Host is the box this service runs on. Filled in by the hub, omitted by
	// an agent, for the same reason as Pending.Host.
	Host  string `json:"host,omitempty"`
	File  string `json:"file"`
	Image string `json:"image"`
	// CurrentTag is what the file pins now; Candidate is what is waiting.
	CurrentTag string `json:"current_tag"`
	Candidate  string `json:"candidate"`
	// Bump is how big the change would be.
	Bump version.Kind `json:"bump,omitempty"`
	// Remaining is how much longer the soak has to run, and Outcome what
	// happens then -- which depends on the policy: a soaking major on a
	// service set to patch is not going to be applied when the wait ends, it
	// is going to be queued. Saying "will be applied" regardless would be a
	// promise duva does not keep.
	Remaining string `json:"remaining"`
	Outcome   string `json:"outcome"`
}

// Pending is an update duva will not apply on its own. It carries everything
// the UI needs to render a row and the actor needs to apply it, so neither has
// to re-derive it from the compose file.
type Pending struct {
	Service string `json:"service"`
	// Host is the box this service runs on, filled in by the hub as it
	// collects from each agent. Omitted on an agent's own state file: it is
	// one host's queue, and a field naming it in every entry would be noise
	// on disk and a lie waiting to happen if the host were ever renamed.
	Host  string `json:"host,omitempty"`
	File  string `json:"file"`
	Image string `json:"image"`
	// CurrentTag and CurrentDigest are what the compose file pins now.
	CurrentTag    string `json:"current_tag"`
	CurrentDigest string `json:"current_digest"`
	// Kind is "tag" or "digest"; Candidate is the newer tag or the moved
	// digest respectively.
	Kind      Kind   `json:"kind"`
	Candidate string `json:"candidate"`
	// Bump is how big the version change is, for a tag candidate. Empty for a
	// digest candidate, which has no version pair to compare.
	Bump version.Kind `json:"bump,omitempty"`
	// Why says why this needs a human rather than being applied: the
	// classification against the service's duva.auto threshold.
	Why string `json:"why"`
	// Auto is that threshold itself, so the queue can show the rule and not
	// just the verdict it produced.
	Auto string `json:"auto"`
	// FirstSeen is when this candidate first showed up. Recorded but not
	// currently displayed: a raw timestamp told the reader less than it
	// seemed to, being neither the release's age nor a relative one, and
	// duva.delay already answers staleness from the tag's real publish date.
	// Kept because it is one string per entry and the only place the answer
	// to "how long has this waited" survives a restart. RFC 3339.
	FirstSeen string `json:"first_seen"`
}

// Key is what identifies this row to the thing that will apply it.
//
// "host/service" when the row came from a hub, which has several hosts and so
// several services of the same name to tell apart; the bare service when it
// did not, so a single-host duva sends exactly what it always sent and its
// Applier needs no knowledge of hosts at all.
func (p Pending) Key() string { return rowKey(p.Host, p.Service) }

// Key identifies a soaking row, on the same terms as Pending.Key.
func (s Soaking) Key() string { return rowKey(s.Host, s.Service) }

func rowKey(host, service string) string {
	if host == "" {
		return service
	}
	return host + "/" + service
}

// NewState returns an empty state with its maps ready to use.
func NewState() *State {
	return &State{
		Baseline: Baseline{},
		Notified: map[string]string{},
		Pending:  map[string]Pending{},
		Soaking:  map[string]Soaking{},
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
	if st.Soaking == nil {
		st.Soaking = map[string]Soaking{}
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
// ReconcileSoaking records what duva.delay is holding back, and forgets what
// it is no longer waiting on -- a candidate that has soaked long enough moves
// to Pending or is applied, and one that vanished from the registry is not
// coming.
func (s *State) ReconcileSoaking(seen []Finding) {
	// Only services this run actually looked at, and only those it could
	// answer for: a service absent from `seen` was not checked, and one whose
	// check errored says nothing about whether it is still soaking -- the
	// registry was unreachable, not the release suddenly old enough.
	for _, f := range seen {
		if f.Status == StatusError {
			continue
		}
		if f.Soaking == nil {
			delete(s.Soaking, f.Service)
			continue
		}
		bump := version.Classify(f.CurrentTag, f.Soaking.Tag)
		s.Soaking[f.Service] = Soaking{
			Service:    f.Service,
			File:       f.File,
			Image:      f.Image,
			CurrentTag: f.CurrentTag,
			Candidate:  f.Soaking.Tag,
			Bump:       bump,
			Remaining:  HumanDuration(f.Soaking.Delay - f.Soaking.Age),
			Outcome:    soakOutcome(bump, f.Auto),
		}
	}
}

// soakOutcome is what happens when the wait ends: duva applies it, or it
// joins the queue for a human. The soak decides WHEN, the policy decides WHAT.
func soakOutcome(bump version.Kind, auto Auto) string {
	if auto != AutoNone && bumpRank(bump) <= auto.rank() {
		return "will be applied automatically"
	}
	return "moves to approval"
}

// HumanDuration renders a wait in the largest unit that says something: days
// for a soak measured in days, hours for the last day of it.
//
// Exported so the log and the page say the same thing. "168h0m0s" is what a
// duration prints as, not what a week is called.
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if days := int(d.Hours() / 24); days >= 1 {
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
	hours := int(d.Hours())
	if hours <= 1 {
		return "less than an hour"
	}
	return fmt.Sprintf("%d hours", hours)
}

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
			Why:           f.Why,
			Auto:          string(f.Auto),
			FirstSeen:     firstSeen,
		}
	}
}

// PendingList returns the pending updates sorted by service name, so the UI
// and any test see a stable order.
// SoakingList is what duva.delay is holding back, in a stable order.
func (s *State) SoakingList() []Soaking {
	out := make([]Soaking, 0, len(s.Soaking))
	for _, p := range s.Soaking {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

func (s *State) PendingList() []Pending {
	out := make([]Pending, 0, len(s.Pending))
	for _, p := range s.Pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}
