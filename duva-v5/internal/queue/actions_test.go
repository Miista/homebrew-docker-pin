package queue

import (
	"errors"
	"testing"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

func releaseOf(service, image, to string) Entry {
	return Entry{Service: service, Image: image, From: "1.0.0", To: to, Tag: to, Kind: version.KindPatch}
}

func lastActor(t *testing.T, q *Pending, kind string) string {
	t.Helper()
	var actor string
	if err := q.st.DB().QueryRow(`SELECT actor FROM queue_events WHERE kind = ? ORDER BY id DESC LIMIT 1`, kind).Scan(&actor); err != nil {
		t.Fatalf("no %s event: %v", kind, err)
	}
	return actor
}

// Discarding is "not now", not "never": the entry goes, and the same release
// found again is offered again.
func TestDiscardingIsNotForever(t *testing.T) {
	q := NewPending()
	e, _ := q.PutEntry(releaseOf("app", "example.com/app", "1.0.1"), now)

	if err := q.Discard(e.ID, "soren", now); err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Get("app"); ok {
		t.Error("a discarded entry is still waiting")
	}
	if got := lastActor(t, q, "discarded"); got != "soren" {
		t.Errorf("discarded by %q, want soren", got)
	}
	if !q.Put(releaseOf("app", "example.com/app", "1.0.1"), now) {
		t.Error("the discarded release, found again, was not offered again")
	}
	if err := q.Discard(e.ID, "soren", now); !errors.Is(err, ErrNotOpen) {
		t.Errorf("discarding twice = %v, want ErrNotOpen", err)
	}
}

func TestBlockingCoversTheReleaseOnEveryService(t *testing.T) {
	q := NewPending()
	// A version change, whose digest the caller resolved from its tag.
	web, _ := q.PutEntry(releaseOf("web", "postgres", "17.2"), now)
	// A digest move on another service running the same image.
	q.Put(Entry{Service: "db", Container: "db", Image: "postgres", From: "sha256:old", To: "sha256:bad", Tag: "17", Digest: "sha256:bad"}, now)
	// Another image entirely.
	q.Put(releaseOf("app", "example.com/app", "1.0.1"), now)

	if err := q.Block(web.ID, "sha256:bad", "missing migrations", "soren", now); err != nil {
		t.Fatal(err)
	}

	for _, service := range []string{"web", "db"} {
		if _, ok := q.Get(service); ok {
			t.Errorf("%s still offers the blocked release", service)
		}
	}
	if _, ok := q.Get("app"); !ok {
		t.Error("a block on postgres touched another image")
	}
	if reason, blocked := q.IsBlocked("postgres", "sha256:bad"); !blocked || reason != "missing migrations" {
		t.Errorf("IsBlocked = %q %v", reason, blocked)
	}
	if _, blocked := q.IsBlocked("example.com/app", "sha256:bad"); blocked {
		t.Error("a block is per image, and leaked to another")
	}
	if got := lastActor(t, q, "blocked"); got != "soren" {
		t.Errorf("blocked by %q", got)
	}
}

func TestABlockNeedsADigest(t *testing.T) {
	q := NewPending()
	e, _ := q.PutEntry(releaseOf("app", "example.com/app", "1.0.1"), now)
	if err := q.Block(e.ID, "", "bad", "soren", now); err == nil {
		t.Error("blocked without a digest")
	}
}

// No reason is fine; the block says who made it instead.
func TestABlockWithoutAReasonSaysWho(t *testing.T) {
	q := NewPending()
	e, _ := q.PutEntry(releaseOf("app", "example.com/app", "1.0.1"), now)
	if err := q.Block(e.ID, "sha256:bad", "", "soren", now); err != nil {
		t.Fatal(err)
	}
	if reason, blocked := q.IsBlocked("example.com/app", "sha256:bad"); !blocked || reason != "blocked by soren" {
		t.Errorf("IsBlocked = %q %v", reason, blocked)
	}
}

func TestBlockingTwiceIsRefusedAndUnblockingLiftsIt(t *testing.T) {
	q := NewPending()
	e, _ := q.PutEntry(releaseOf("app", "example.com/app", "1.0.1"), now)
	q.Block(e.ID, "sha256:bad", "broken", "soren", now)

	other, _ := q.PutEntry(releaseOf("app", "example.com/app", "1.0.2"), now)
	if err := q.Block(other.ID, "sha256:bad", "again", "soren", now); !errors.Is(err, ErrAlreadyBlocked) {
		t.Errorf("blocking twice = %v, want ErrAlreadyBlocked", err)
	}

	if err := q.Unblock("example.com/app", "sha256:bad", "soren", now); err != nil {
		t.Fatal(err)
	}
	if _, blocked := q.IsBlocked("example.com/app", "sha256:bad"); blocked {
		t.Error("still blocked after unblocking")
	}
	if err := q.Unblock("example.com/app", "sha256:bad", "soren", now); err == nil {
		t.Error("unblocked something not blocked")
	}
	var history int
	q.st.DB().QueryRow(`SELECT COUNT(*) FROM blocked_releases`).Scan(&history)
	if history != 1 {
		t.Errorf("%d block rows, want the lifted one kept as history", history)
	}
}

func TestAnApprovalRecordsWho(t *testing.T) {
	q := NewPending()
	e, _ := q.PutEntry(releaseOf("app", "example.com/app", "1.0.1"), now)
	q.Approved(e, "soren@example.com", now)
	if got := lastActor(t, q, "approved"); got != "soren@example.com" {
		t.Errorf("approved by %q", got)
	}
}

// A digest move to a blocked digest is not offered, however policy sizes it.
func TestABlockedDigestMoveIsNotOffered(t *testing.T) {
	h, q := handlerOver(t, `
services:
  app:
    image: example.com/app:latest@sha256:old
    container_name: my-app
    labels:
      duva.auto: digest
`, &fakeApplier{})
	seed, _ := q.PutEntry(Entry{Service: "seed", Image: "example.com/app", From: "a", To: "b"}, now)
	q.Block(seed.ID, "sha256:bad", "broken build", "soren", now)

	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:latest", Digest: "sha256:bad"})
	if res.Outcome != Ignore {
		t.Errorf("outcome = %s (%s), want ignored", res.Outcome, res.Message)
	}
	if _, ok := q.Get("app"); ok {
		t.Error("a blocked digest was queued")
	}
}
