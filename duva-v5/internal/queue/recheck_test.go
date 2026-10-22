package queue

import (
	"strings"
	"testing"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

const (
	d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	d3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

func versionEntry(from, to string) Entry {
	return Entry{
		Service: "duva-watch", Image: "ghcr.io/miista/duva-watch",
		From: from, To: to, Tag: to, Kind: version.KindMinor,
		Why: "minor; duva.auto is none", Auto: AutoNone, FirstSeen: "2026-10-13T21:52:42Z",
	}
}

func digestEntry(from, to string) Entry {
	return Entry{
		Service: "bazarr", Image: "lscr.io/linuxserver/bazarr",
		From: from, To: to, Tag: "latest", Digest: to,
		Why: "digest moved on a followed tag", Auto: AutoDigest,
	}
}

// declares names no image, so a case is about the tag and digest alone. The
// image's own cases set it.
func declares(tag, digest string) Service {
	return Service{Name: "svc", Tag: tag, Digest: digest, Auto: AutoNone}
}

func declaresImage(image, tag, digest string) Service {
	svc := declares(tag, digest)
	svc.Image = image
	return svc
}

// The question used to be only "has it been done". These are the cases where
// it had not been, and the entry still should not have been offered.
func TestRecheckEntry(t *testing.T) {
	cases := []struct {
		name   string
		entry  Entry
		svc    Service
		keep   bool
		from   string // the From a kept entry should carry
		whyHas string // part of the reason a dropped entry gives
	}{
		// --- the image ---
		{
			name:  "the same image",
			entry: versionEntry("v7.8", "v7.9"), svc: declaresImage("ghcr.io/miista/duva-watch", "v7.8", d1),
			keep: true, from: "v7.8",
		},
		{
			// A release is of one image; its tag means nothing on another.
			name:  "the service now follows another image",
			entry: versionEntry("v7.8", "v7.9"), svc: declaresImage("ghcr.io/miista/duva", "v7.8", d1),
			whyHas: "now follows ghcr.io/miista/duva",
		},
		{
			name:  "a digest move on another image",
			entry: digestEntry(d1, d2), svc: declaresImage("ghcr.io/linuxserver/bazarr", "latest", d1),
			whyHas: "not lscr.io/linuxserver/bazarr",
		},
		// --- version changes ---
		{
			name:  "still where it started",
			entry: versionEntry("v7.8", "v7.9"), svc: declares("v7.8", d1),
			keep: true, from: "v7.8",
		},
		{
			name:  "done: the file follows the tag it wanted",
			entry: versionEntry("v7.8", "v7.9"), svc: declares("v7.9", d2),
			whyHas: "already matches",
		},
		{
			// 2026-10-14: duva-watch moved to latest by hand. Approving the
			// entry would have put it back on v7.9.
			name:  "moved to a moving tag by hand",
			entry: versionEntry("v7.8", "v7.9"), svc: declares("latest", d3),
			whyHas: "latest",
		},
		{
			// Approving this would be a downgrade.
			name:  "moved past it by hand",
			entry: versionEntry("v7.8", "v7.9"), svc: declares("v7.10", d3),
			whyHas: "not newer",
		},
		{
			// Dropped, v7.10 would be lost: the watcher reported it once,
			// and does not again.
			name:  "moved partway: still a step to take, from where it is now",
			entry: versionEntry("v7.8", "v7.10"), svc: declares("v7.9", d2),
			keep: true, from: "v7.9",
		},
		{
			name:  "unpinned by hand: no decision left to watch over",
			entry: versionEntry("v7.8", "v7.9"), svc: declares("v7.8", ""),
			whyHas: "not pinned",
		},

		// --- digest moves ---
		{
			name:  "digest: still where it started",
			entry: digestEntry(d1, d2), svc: declares("latest", d1),
			keep: true, from: d1,
		},
		{
			name:  "digest: done",
			entry: digestEntry(d1, d2), svc: declares("latest", d2),
			whyHas: "already matches",
		},
		{
			// Two digests of one tag have no order, so this cannot tell
			// whether d2 is behind d3 -- and offering it might be a step
			// back.
			name:  "digest: pulled something else by hand",
			entry: digestEntry(d1, d2), svc: declares("latest", d3),
			whyHas: "sha256:33333333",
		},
		{
			name:  "digest: the tag it followed was changed by hand",
			entry: digestEntry(d1, d2), svc: declares("v2", d1),
			whyHas: "v2@",
		},
		{
			// Recorded before From was: only "has it been done" can be asked.
			name:  "digest: an entry with no From is kept until done",
			entry: digestEntry("", d2), svc: declares("latest", d3),
			keep: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := RecheckEntry(c.entry, c.svc)
			if r.Keep != c.keep {
				t.Fatalf("keep = %v, want %v (why: %q)", r.Keep, c.keep, r.Why)
			}
			if c.keep {
				if c.from != "" && r.Entry.From != c.from {
					t.Errorf("From = %q, want %q", r.Entry.From, c.from)
				}
				if r.Entry.To != c.entry.To {
					t.Errorf("To = %q, want it unchanged (%q)", r.Entry.To, c.entry.To)
				}
				return
			}
			if !strings.Contains(r.Why, c.whyHas) {
				t.Errorf("why = %q, want it to mention %q", r.Why, c.whyHas)
			}
		})
	}
}

// Reconciling keeps and drops. A service whose policy now allows the change
// is still left waiting -- with its old reason, since "within duva.auto"
// would describe an apply nobody started.
func TestRecheckNeverTurnsAnEntryIntoAnApply(t *testing.T) {
	svc := declares("v7.8", d1)
	svc.Auto = AutoMajor

	r := RecheckEntry(versionEntry("v7.8", "v7.9"), svc)
	if !r.Keep {
		t.Fatalf("dropped: %q", r.Why)
	}
	if r.Entry.Why != "minor; duva.auto is none" {
		t.Errorf("why = %q, want the reason it was waiting", r.Entry.Why)
	}
}
