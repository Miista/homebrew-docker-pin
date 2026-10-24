package queue

import (
	"strings"
	"testing"
	"time"
)

// --- ParseSoak ---------------------------------------------------------------

func TestParseSoak(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"3d", 72 * time.Hour},
		{"0d", 0},
		{"72h", 72 * time.Hour},
		{"90m", 90 * time.Minute},
		{" 2d ", 48 * time.Hour},
	} {
		got, err := ParseSoak(c.in)
		if err != nil {
			t.Errorf("ParseSoak(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSoak(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// A soak that silently became none is the permissive direction, so anything
// unreadable is refused.
func TestParseSoakRejectsWhatItCannotRead(t *testing.T) {
	for _, in := range []string{"3 days", "d", "-1d", "-5h", "soon", "1.5d"} {
		if _, err := ParseSoak(in); err == nil {
			t.Errorf("ParseSoak(%q) accepted it", in)
		}
	}
}

// --- releaseAt ---------------------------------------------------------------

func TestReleaseAtCountsFromPublication(t *testing.T) {
	published := now.Add(-24 * time.Hour)
	got := releaseAt(72*time.Hour, published, now)
	if want := published.Add(72 * time.Hour); !got.Equal(want) {
		t.Errorf("releaseAt = %s, want %s", got, want)
	}
}

func TestReleaseAtIsZeroWithoutASoak(t *testing.T) {
	if got := releaseAt(0, now, now); !got.IsZero() {
		t.Errorf("releaseAt = %s, want none", got)
	}
}

// A release found after it has already soaked long enough waits for nothing.
func TestReleaseAtIsZeroForAReleaseAlreadyOldEnough(t *testing.T) {
	if got := releaseAt(72*time.Hour, now.Add(-73*time.Hour), now); !got.IsZero() {
		t.Errorf("releaseAt = %s, want none", got)
	}
}

func TestReleaseAtCountsFromNowWithoutAPublishTime(t *testing.T) {
	if got := releaseAt(time.Hour, time.Time{}, now); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("releaseAt = %s, want an hour from now", got)
	}
}

// A publish time in the future is a clock that cannot be trusted.
func TestReleaseAtDoesNotTrustAFuturePublishTime(t *testing.T) {
	if got := releaseAt(time.Hour, now.Add(24*time.Hour), now); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("releaseAt = %s, want an hour from now", got)
	}
}

// --- Lookup ------------------------------------------------------------------

const soakedProject = `
services:
  labelled:
    image: example.com/app:1.0.0@sha256:old
    container_name: labelled
    labels:
      duva.auto: patch
      duva.defer: 3d
  unlabelled:
    image: example.com/app:1.0.0@sha256:old
    container_name: unlabelled
    labels:
      duva.auto: patch
  opted-out:
    image: example.com/app:1.0.0@sha256:old
    container_name: opted-out
    labels:
      duva.auto: patch
      duva.defer: "0"
`

func TestLookupReadsTheSoak(t *testing.T) {
	l := project(t, soakedProject)
	for container, want := range map[string]time.Duration{
		"labelled":   72 * time.Hour,
		"unlabelled": 0, // no host default: no label, no soak
		"opted-out":  0,
	} {
		svc, _, err := l.Service(container)
		if err != nil {
			t.Fatalf("%s: %v", container, err)
		}
		if svc.Soak != want {
			t.Errorf("%s: soak = %s, want %s", container, svc.Soak, want)
		}
	}
}

func TestLookupRejectsAnUnreadableSoak(t *testing.T) {
	l := project(t, `
services:
  app:
    image: example.com/app:1.0.0@sha256:old
    container_name: my-app
    labels:
      duva.defer: a week
`)
	if _, _, err := l.Service("my-app"); err == nil {
		t.Fatal("want an error for an unreadable duva.defer")
	}
}

// --- Handler -----------------------------------------------------------------

const soakingProject = `
services:
  app:
    image: example.com/app:1.0.0@sha256:old
    container_name: my-app
    labels:
      duva.auto: patch
      duva.defer: 3d
`

// Allowed by policy, but too young: it waits in the queue rather than being
// applied, and nobody is told, since nobody needs to do anything.
func TestHandleSoaksAReleaseThatIsTooYoung(t *testing.T) {
	app := &fakeApplier{}
	h, q := handlerOver(t, soakingProject, app)
	announced := 0
	h.Announce = func(Entry) { announced++ }
	published := now.Add(-time.Hour)

	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.1", Published: published})

	if res.Outcome != Queue {
		t.Errorf("outcome = %q, want queue (%s)", res.Outcome, res.Message)
	}
	if len(app.started) != 0 {
		t.Errorf("applied before the soak was over: %+v", app.started)
	}
	e, ok := q.Get("app")
	if !ok {
		t.Fatal("nothing was queued")
	}
	if want := published.Add(72 * time.Hour).UTC().Format(time.RFC3339); e.ReleaseAt != want {
		t.Errorf("release_at = %q, want %q", e.ReleaseAt, want)
	}
	if !strings.Contains(e.Why, "delayed until") {
		t.Errorf("why = %q, want it to say it is soaking", e.Why)
	}
	if announced != 0 {
		t.Errorf("announced %d time(s), want none", announced)
	}
}

func TestHandleAppliesAReleaseAlreadySoaked(t *testing.T) {
	app := &fakeApplier{}
	h, q := handlerOver(t, soakingProject, app)

	res := h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.1", Published: now.Add(-4 * 24 * time.Hour)})

	if res.Outcome != Apply {
		t.Fatalf("outcome = %q, want apply (%s)", res.Outcome, res.Message)
	}
	if len(app.started) != 1 {
		t.Errorf("started = %+v", app.started)
	}
	if e, _ := q.Get("app"); e.ReleaseAt != "" {
		t.Errorf("an entry being applied still has a soak: %q", e.ReleaseAt)
	}
}

// The same candidate arriving again keeps the clock it already had, and is
// still not applied.
func TestHandleKeepsTheSoakOnANoticeArrivingAgain(t *testing.T) {
	app := &fakeApplier{}
	h, q := handlerOver(t, soakingProject, app)
	n := Notice{Container: "my-app", Image: "example.com/app:1.0.1", Published: now.Add(-time.Hour)}
	h.Handle(n)
	first, _ := q.Get("app")

	h.Now = func() time.Time { return now.Add(time.Hour) }
	h.Handle(n)

	if len(app.started) != 0 {
		t.Errorf("applied on the second notice: %+v", app.started)
	}
	if again, _ := q.Get("app"); again.ReleaseAt != first.ReleaseAt {
		t.Errorf("release_at moved from %q to %q", first.ReleaseAt, again.ReleaseAt)
	}
}

// duva.defer says when the next action happens, whatever it is. For what
// policy sends to a person that action is showing them, so it is held --
// Upcoming, not the queue -- and announced only when the delay is over.
func TestADelayHoldsWhatWaitsForAPersonToo(t *testing.T) {
	h, q := handlerOver(t, soakingProject, &fakeApplier{})
	announced := 0
	h.Announce = func(Entry) { announced++ }
	// A minor, which duva.auto: patch does not cover.
	h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.1.0", Published: now})

	if up := q.Upcoming(); len(up) != 1 || up[0].ReleaseAt == "" {
		t.Fatalf("upcoming = %+v, want the minor, held", up)
	}
	if len(q.Waiting()) != 0 || len(q.Soaking()) != 0 {
		t.Errorf("waiting %+v, scheduled %+v: want it in neither", q.Waiting(), q.Soaking())
	}
	if announced != 0 {
		t.Errorf("announced %d times while held", announced)
	}
}

// Without an updater nothing applies unattended, so there is nothing to soak.
func TestWithoutAnUpdaterNothingSoaks(t *testing.T) {
	h, q := handlerOver(t, soakingProject, nil)
	h.Handle(Notice{Container: "my-app", Image: "example.com/app:1.0.1", Published: now})

	if e, _ := q.Get("app"); e.ReleaseAt != "" {
		t.Errorf("release_at = %q, want none", e.ReleaseAt)
	}
}

// --- Release -----------------------------------------------------------------

func TestReleaseClearsTheSoak(t *testing.T) {
	q := NewPending()
	put, _ := q.PutEntry(Entry{Service: "app", From: "1.0.0", To: "1.0.1", ReleaseAt: now.Format(time.RFC3339)}, now)

	e, ok := q.Release(put, now)
	if !ok {
		t.Fatal("a due entry was not released")
	}
	if e.ReleaseAt != "" {
		t.Errorf("released entry still has release_at %q", e.ReleaseAt)
	}
	if got, _ := q.Get("app"); got.ReleaseAt != "" {
		t.Errorf("the queue still has release_at %q", got.ReleaseAt)
	}
	// Released once. Due again on every tick would re-offer a failed apply.
	if due := q.Due(now.Add(time.Hour)); len(due) != 0 {
		t.Errorf("still due after release: %+v", due)
	}
}

func TestReleaseRefusesWhatIsNotDue(t *testing.T) {
	q := NewPending()
	soaking, _ := q.PutEntry(Entry{Service: "app", From: "1.0.0", To: "1.0.1", ReleaseAt: now.Add(time.Hour).Format(time.RFC3339)}, now)
	manual, _ := q.PutEntry(Entry{Service: "manual", From: "1.0.0", To: "1.1.0"}, now)

	for name, e := range map[string]Entry{"soaking": soaking, "manual": manual, "absent": {ID: 999}} {
		if _, ok := q.Release(e, now); ok {
			t.Errorf("%s was released", name)
		}
	}
	if got, _ := q.Get("app"); got.ReleaseAt == "" {
		t.Error("a refused release cleared the soak anyway")
	}
}
