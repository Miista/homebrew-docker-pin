package queue

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseIgnore(t *testing.T) {
	for value, want := range map[string]bool{"": false, "false": false, "true": true} {
		if got, err := ParseIgnore(value); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", value, got, err, want)
		}
	}
	for _, bad := range []string{"yes", "True", "1", "on"} {
		if _, err := ParseIgnore(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

const ignoredProject = `
services:
  broker:
    image: redis:7.4-alpine@sha256:old
    container_name: paperless-broker
    labels:
      duva.ignore: "true"
`

// The lookup reads duva.ignore, and refuses a value it cannot read.
func TestTheLookupReadsDuvaIgnore(t *testing.T) {
	svc, _, err := project(t, ignoredProject).Service("paperless-broker")
	if err != nil || !svc.Ignored {
		t.Errorf("ignored = %v, err %v", svc.Ignored, err)
	}
	_, _, err = project(t, strings.Replace(ignoredProject, `"true"`, "yes", 1)).Service("paperless-broker")
	if err == nil || !strings.Contains(err.Error(), "duva.ignore") {
		t.Errorf("err = %v, want one naming duva.ignore", err)
	}
}

// A finding for an ignored service is turned away with a 2xx -- dropped, not
// held for another try -- and nothing is queued.
func TestHandleIgnoresAnIgnoredService(t *testing.T) {
	h, q := handlerOver(t, ignoredProject, &fakeApplier{})
	res := h.Handle(Notice{Container: "paperless-broker", Image: "redis:7.4-alpine", Digest: "sha256:new"})
	if res.Outcome != Ignore || res.Status != http.StatusOK {
		t.Errorf("outcome %q status %d (%s), want ignore, 200", res.Outcome, res.Status, res.Message)
	}
	if q.Len() != 0 {
		t.Errorf("queued %+v for an ignored service", q.List())
	}
}

// What waits for a service when it is ignored is dropped by the reconcile.
func TestRecheckDropsWhatWaitsForAnIgnoredService(t *testing.T) {
	r := RecheckEntry(waitingMove(AutoNone), Service{Name: "radarr", Ignored: true})
	if r.Keep || !strings.Contains(r.Why, "duva.ignore") {
		t.Errorf("keep %v, why %q", r.Keep, r.Why)
	}
}

// An ignored service is not tracked: not offered in "Check…", listed among
// the ignored, and the sync says when that changes.
func TestTheSyncKeepsIgnoredServicesApart(t *testing.T) {
	q := NewPending()
	broker := Declared{Container: "paperless-broker", Service: "paperless-broker", Image: "redis", Tag: "7.4-alpine", Pinned: true}
	app := Declared{Container: "app", Service: "app", Image: "example.com/app", Tag: "1", Pinned: true}
	q.SyncServices([]Declared{broker, app}, running("paperless-broker", "app"), now)

	broker.Ignored = true
	changes, _ := q.SyncServices([]Declared{broker, app}, running("paperless-broker", "app"), now)
	if len(changes) != 1 || changes[0].String() != "paperless-broker: now ignored (duva.ignore is true)" {
		t.Errorf("changes = %v", changes)
	}
	if got := strings.Join(q.Tracked(), ","); got != "app" {
		t.Errorf("tracked = %q, want app alone", got)
	}
	if got := q.Ignored(); len(got) != 1 || got[0].Service != "paperless-broker" || got[0].Image != "redis" {
		t.Errorf("ignored = %+v", got)
	}
	s := &Server{Queue: q}
	if snap := s.Snapshot(); len(snap.Ignored) != 1 || strings.Join(snap.Services, ",") != "app" {
		t.Errorf("snapshot ignored %+v, services %v", snap.Ignored, snap.Services)
	}

	broker.Ignored = false
	if changes, _ = q.SyncServices([]Declared{broker, app}, running("paperless-broker", "app"), now); len(of(changes, ChangeUnignored)) != 1 {
		t.Errorf("unignoring: %v", changes)
	}
}
