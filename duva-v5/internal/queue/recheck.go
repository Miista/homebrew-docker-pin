package queue

import (
	"errors"
	"fmt"
)

// ErrNotDeclared is a recheck's answer for an entry whose service the
// compose project no longer declares (with a container_name): renamed,
// removed, or lost its name. Not a conclusion by itself -- Reconcile drops the
// entry only once the service's container is gone too, which is what
// decommissioning a service is.
var ErrNotDeclared = errors.New("no longer declared in the compose project")

// Recheck is what reconciling concluded about one waiting entry.
type Recheck struct {
	// Keep is whether the entry still applies.
	Keep bool
	// Entry is the entry as it should now read: unchanged, or with From
	// moved to what the file declares now.
	Entry Entry
	// Why says, for a dropped entry, why it no longer applies.
	Why string
}

// RecheckEntry asks whether a waiting entry still applies to the service as
// its compose file declares it now.
//
// Not only whether it has been done. An entry records a transition, From to
// To, and a service changed by other means -- a hand edit, `docker pin
// upgrade`, a move to `latest` -- no longer starts where the entry does. Kept
// as it was, the entry goes on offering To as though nothing had happened,
// and approving it is a step backwards: v7.8 -> v7.9 offered to a service
// already on v7.10. That happened on 2026-10-14, and only the entries'
// staleness was noticed.
//
// A version change is decided again by Decide, against the service as it is
// now: the one implementation of "is this newer" answers, rather than a second
// one here. A change it would still make is kept, and From is moved to where
// the service is, so the row shows the step that would actually be taken.
// Dropping it instead would lose it for good -- the watcher reports a tag once,
// when it is published, and not again.
//
// A digest move has no order to decide by: two digests of one tag say nothing
// about which is newer. So it applies only while the file still pins the tag
// and digest it moved from.
//
// Pure, like Decide: the caller reads the compose file.
func RecheckEntry(e Entry, svc Service) Recheck {
	// Where the image says its source lives, as the container says now: so
	// an entry queued before it was recorded gains it, and one whose image
	// changed its say follows.
	e.Source = svc.Source
	if svc.Digest != "" && svc.Digest == e.Digest {
		return Recheck{Why: "already matches the compose file"}
	}

	if e.Kind == "" {
		// An entry from before From was recorded can only be asked the old
		// question: has it been done.
		if e.From == "" {
			return Recheck{Keep: true, Entry: e}
		}
		if svc.Tag == e.Tag && svc.Digest == e.From {
			return Recheck{Keep: true, Entry: e}
		}
		return Recheck{Why: fmt.Sprintf("the compose file now pins %s, not the %s this moved from",
			describe(svc), shortDigest(e.From))}
	}

	if svc.Tag == e.To {
		return Recheck{Why: "already matches the compose file"}
	}

	v := Decide(Notice{Image: e.Image + ":" + e.To, Digest: e.Digest}, svc, true)
	if v.Outcome == Ignore || v.Outcome == Reject {
		return Recheck{Why: v.Why}
	}

	kept := e
	kept.From = v.From
	kept.Kind = v.Kind
	kept.Auto = svc.Auto
	// Only a reason for waiting is a reason to show. If policy would now
	// apply it, reconciling still does not: it keeps and drops, and an entry
	// it keeps waits as it did.
	if v.Outcome == Queue {
		kept.Why = v.Why
	}
	return Recheck{Keep: true, Entry: kept}
}

// describe is what a service pins, short enough for a log line.
func describe(svc Service) string {
	if svc.Digest == "" {
		return svc.Tag + " unpinned"
	}
	return svc.Tag + "@" + shortDigest(svc.Digest)
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}
