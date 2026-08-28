//go:build integration

package integration

import (
	"slices"
	"strings"
	"testing"
)

// One image, four services, four verdicts -- differing only by their labels.
// That is the point: a service's policy decides what happens to it, and the
// same available tags produce a different answer for each.
//
// The unit tests fake the registry, so what they cannot show is that a real
// tag list is sorted and classified correctly, that labels survive a round
// trip through compose parsing, or that the queue is served over HTTP as what
// duva actually recorded.
func TestPolicyDecidesWhatIsQueued(t *testing.T) {
	s := Up(t, "duva-policy/four-verdicts")

	// A newer patch and a newer minor of the 1.x line; see the scenario's
	// additional-images.
	s.PushAdditional()

	// Every watched service shares the image, so they all follow the same
	// tags and differ only in what their labels allow.
	s.Pin("autopatch", "exceeds", "defaultnone")

	// autopatch is running before duva looks: duva recreates a container, it
	// does not create one from nothing.
	s.composeUp("autopatch")

	s.Start()

	t.Run("a patch within duva.auto is applied and committed", func(t *testing.T) {
		if got := s.Image("autopatch"); !strings.Contains(got, ":1.26.3@sha256:") {
			t.Errorf("1.26.0 -> 1.26.3 was not applied:\n  got %s", got)
		}
		if got := s.Log(); !strings.Contains(got, "autopatch: 1.26.0 -> 1.26.3") {
			t.Errorf("the change was not committed with a readable subject:\n  got %s", got)
		}
	})

	t.Run("the queue holds exactly what needs a human", func(t *testing.T) {
		want := []string{"defaultnone", "exceeds"}
		if got := s.Queued(); !slices.Equal(got, want) {
			t.Errorf("queue is %v, want %v", got, want)
		}
	})

	t.Run("a minor beyond duva.auto needs approval", func(t *testing.T) {
		row := s.Pending("exceeds")
		if row.Candidate != "1.31.4" {
			t.Errorf("a wider regex should find the newer minor, got %q", row.Candidate)
		}
		if row.Bump != "minor" {
			t.Errorf("1.26.0 -> 1.31.4 should classify as minor, got %q", row.Bump)
		}
		if row.Why == "" {
			t.Error("a queued row should carry an explanation")
		}
	})

	t.Run("an absent duva.auto defaults to none", func(t *testing.T) {
		row := s.Pending("defaultnone")
		if row.Candidate != "1.26.3" {
			t.Errorf("the patch should still be found, got %q", row.Candidate)
		}
		if row.Bump != "patch" {
			t.Errorf("1.26.0 -> 1.26.3 should classify as patch, got %q", row.Bump)
		}
	})

	t.Run("an unpinned service is not watched", func(t *testing.T) {
		if row := s.Pending("loose"); row.Candidate != "" {
			t.Errorf("being pinned is the opt-in, but loose was queued: %+v", row)
		}
	})

	// A tool that re-announces what it already reported gets muted, and then
	// real news is missed too.
	t.Run("a second check leaves the queue unchanged", func(t *testing.T) {
		before := s.Queued()
		s.CheckNow()
		if got := s.Queued(); !slices.Equal(got, before) {
			t.Errorf("the queue changed on a second check:\n  was %v\n  now %v", before, got)
		}
	})

	// The page is what an operator sees, served from the same state the
	// assertions above read: a difference between them is the UI lying.
	t.Run("the queue is served as what duva recorded", func(t *testing.T) {
		page := s.Queue()

		if !strings.Contains(page, "2 waiting") {
			t.Error("the page should report two waiting")
		}
		if !strings.Contains(page, "kind-minor") {
			t.Error("the page should show the minor classification")
		}
		if strings.Contains(page, ">autopatch<") {
			t.Error("an auto-applied service should never appear in the queue")
		}
		if strings.Contains(page, ">loose<") {
			t.Error("an unwatched service should never appear in the queue")
		}
	})
}
