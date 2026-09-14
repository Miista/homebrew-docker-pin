package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/detect"
	"github.com/Miista/homebrew-docker-pin/internal/croncal"
)

// The rules the detector exists to keep, tested as behaviour rather than as
// units: a check runs against a registry that behaves however the test says,
// and the assertion is on what the cutoff does afterwards. Whether
// mayAdvance(0) returns true is a tautology; whether a failed run leaves the
// cutoff alone is the thing that matters.

// fakeReg is a registry that answers from a table and can be made to fail for
// particular images.
type fakeReg struct {
	tags   map[string][]string
	dates  map[string]time.Time
	broken map[string]bool
}

func (f fakeReg) registry() detect.Registry {
	return detect.Registry{
		ListTags: func(image string) ([]detect.DatedTag, error) {
			if f.broken[image] {
				return nil, errors.New("connection refused")
			}
			var out []detect.DatedTag
			for _, name := range f.tags[image] {
				out = append(out, detect.DatedTag{Name: name, Published: f.dates[name]})
			}
			return out, nil
		},
		TagCreated: func(_, tag string) (time.Time, error) {
			return f.dates[tag], nil
		},
	}
}

func service(name, image, tag string) detect.Service {
	return detect.Service{Name: name, Container: name, Image: image, Tag: tag}
}

// check runs one whole cycle the way the binary does: load the cutoff, check
// everything, then advance or not. Returns what the cutoff is afterwards.
func check(t *testing.T, services []detect.Service, reg detect.Registry) (before, after time.Time, found, failed int) {
	return checkPublishing(t, services, reg, "")
}

// checkPublishing is check with a webhook. An empty url means findings are
// not published at all, which is the case every other test here runs under.
func checkPublishing(t *testing.T, services []detect.Service, reg detect.Registry, url string) (before, after time.Time, found, failed int) {
	t.Helper()

	before, _, err := loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff: %v", err)
	}

	startedAt := time.Now()
	reporter := newReporter(url, "testhost", zerolog.Nop())
	found, failed = checkAll(services, before, reg, reporter.report, zerolog.Nop())

	if mayAdvance(failed, reporter.unpublished()) {
		if err := saveCutoff(startedAt); err != nil {
			t.Fatalf("saveCutoff: %v", err)
		}
	}

	after, _, err = loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff after: %v", err)
	}
	return before, after, found, failed
}

// --- the rule that matters ---------------------------------------------------

// A complete run earns the right to move the line forward.
func TestACompleteRunAdvancesTheCutoff(t *testing.T) {
	useTempState(t)
	// Seed a cutoff, so "advanced" means something rather than "a first run
	// wrote one".
	seeded := time.Now().Add(-48 * time.Hour)
	if err := saveCutoff(seeded); err != nil {
		t.Fatal(err)
	}

	reg := fakeReg{
		tags:  map[string][]string{"example.com/app": {"1.0.0", "1.1.0"}},
		dates: map[string]time.Time{"1.1.0": time.Now().Add(-time.Hour)},
	}
	before, after, found, failed := check(t,
		[]detect.Service{service("app", "example.com/app", "1.0.0")}, reg.registry())

	if failed != 0 {
		t.Fatalf("failed = %d, want a clean run", failed)
	}
	if found != 1 {
		t.Errorf("found = %d, want the one new tag", found)
	}
	if !after.After(before) {
		t.Errorf("the cutoff did not move: %v -> %v", before, after)
	}
}

// A run with a hole in it must not. Whatever the unreachable service
// published falls between the old cutoff and the new one, and is never
// reported again -- silently, and forever.
func TestAFailedServiceLeavesTheCutoffAlone(t *testing.T) {
	useTempState(t)
	seeded := time.Now().Add(-48 * time.Hour)
	if err := saveCutoff(seeded); err != nil {
		t.Fatal(err)
	}

	reg := fakeReg{
		tags:   map[string][]string{"example.com/ok": {"1.1.0"}},
		dates:  map[string]time.Time{"1.1.0": time.Now().Add(-time.Hour)},
		broken: map[string]bool{"example.com/down": true},
	}
	before, after, _, failed := check(t, []detect.Service{
		service("ok", "example.com/ok", "1.0.0"),
		service("down", "example.com/down", "1.0.0"),
	}, reg.registry())

	if failed != 1 {
		t.Fatalf("failed = %d, want the one unreachable service counted", failed)
	}
	if !after.Equal(before) {
		t.Errorf("the cutoff moved despite a service that could not be checked: %v -> %v", before, after)
	}
}

// And the consequence: because the cutoff stayed, the next run reports the
// same tag again rather than skipping it. Noisy is the correct failure mode.
func TestAfterAFailureTheNextRunStillSeesTheTag(t *testing.T) {
	useTempState(t)
	if err := saveCutoff(time.Now().Add(-48 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	ok := service("ok", "example.com/ok", "1.0.0")
	down := service("down", "example.com/down", "1.0.0")
	reg := fakeReg{
		tags:   map[string][]string{"example.com/ok": {"1.1.0"}},
		dates:  map[string]time.Time{"1.1.0": time.Now().Add(-time.Hour)},
		broken: map[string]bool{"example.com/down": true},
	}

	_, _, firstFound, _ := check(t, []detect.Service{ok, down}, reg.registry())
	if firstFound != 1 {
		t.Fatalf("first run found %d, want 1", firstFound)
	}

	// Same registry, same everything: the tag must still be visible, because
	// the cutoff never moved past it.
	_, _, secondFound, _ := check(t, []detect.Service{ok, down}, reg.registry())
	if secondFound != 1 {
		t.Errorf("second run found %d, want the tag reported again -- the cutoff should not have passed it", secondFound)
	}
}

// The other half: after a clean run, the same tag is not reported twice.
// Without this the rule above would be satisfied by a detector that never
// advanced at all.
func TestAfterACleanRunTheSameTagIsNotReportedAgain(t *testing.T) {
	useTempState(t)
	if err := saveCutoff(time.Now().Add(-48 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	svc := service("app", "example.com/app", "1.0.0")
	reg := fakeReg{
		tags:  map[string][]string{"example.com/app": {"1.1.0"}},
		dates: map[string]time.Time{"1.1.0": time.Now().Add(-time.Hour)},
	}

	_, _, firstFound, _ := check(t, []detect.Service{svc}, reg.registry())
	if firstFound != 1 {
		t.Fatalf("first run found %d, want 1", firstFound)
	}

	_, _, secondFound, _ := check(t, []detect.Service{svc}, reg.registry())
	if secondFound != 0 {
		t.Errorf("second run found %d, want 0 -- the cutoff should have passed it", secondFound)
	}
}

// Every service failing is the same rule, and the case where getting it wrong
// would skip everything at once.
func TestEveryServiceFailingLeavesTheCutoffAlone(t *testing.T) {
	useTempState(t)
	if err := saveCutoff(time.Now().Add(-48 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	reg := fakeReg{broken: map[string]bool{
		"example.com/a": true,
		"example.com/b": true,
	}}
	before, after, _, failed := check(t, []detect.Service{
		service("a", "example.com/a", "1.0.0"),
		service("b", "example.com/b", "1.0.0"),
	}, reg.registry())

	if failed != 2 {
		t.Fatalf("failed = %d, want both counted", failed)
	}
	if !after.Equal(before) {
		t.Errorf("the cutoff moved despite every service failing: %v -> %v", before, after)
	}
}

// A clean run that found nothing still advances. "Nothing new" is a complete
// answer, and refusing to move on it would mean the window grew forever on a
// quiet stack.
func TestACleanRunWithNoFindingsStillAdvances(t *testing.T) {
	useTempState(t)
	if err := saveCutoff(time.Now().Add(-48 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	reg := fakeReg{
		tags:  map[string][]string{"example.com/app": {"1.0.0"}},
		dates: map[string]time.Time{"1.0.0": time.Now().Add(-72 * time.Hour)},
	}
	before, after, found, failed := check(t,
		[]detect.Service{service("app", "example.com/app", "1.0.0")}, reg.registry())

	if found != 0 || failed != 0 {
		t.Fatalf("found=%d failed=%d, want a clean and empty run", found, failed)
	}
	if !after.After(before) {
		t.Errorf("a clean run that found nothing did not advance: %v -> %v", before, after)
	}
}

// --- the schedule ------------------------------------------------------------

// An unset schedule is daily, not an error. The choice is between checking
// daily and checking never, and never is not something to arrive at by
// omission.
func TestAnUnsetScheduleIsDaily(t *testing.T) {
	t.Setenv("DETECTOR_SCHEDULE", "")
	if got := scheduleFromEnv(); got != defaultSchedule {
		t.Errorf("schedule = %q, want the default %q", got, defaultSchedule)
	}
}

func TestAnExplicitScheduleWins(t *testing.T) {
	t.Setenv("DETECTOR_SCHEDULE", "0 */6 * * *")
	if got := scheduleFromEnv(); got != "0 */6 * * *" {
		t.Errorf("schedule = %q, want what was set", got)
	}
}

// The default has to be a schedule croncal can actually read -- a typo here
// would not fail until something ran serve.
func TestTheDefaultScheduleParses(t *testing.T) {
	if _, err := croncal.Next(defaultSchedule, time.Now()); err != nil {
		t.Errorf("the default schedule %q does not parse: %v", defaultSchedule, err)
	}
}

// A schedule that does not parse stops the detector at startup rather than at
// the first tick -- which for a daily schedule would be a day later, with
// nothing in the log to say why nothing happened.
func TestServeRefusesAnUnparseableSchedule(t *testing.T) {
	t.Setenv("DETECTOR_SCHEDULE", "every tuesday-ish")
	err := serve(zerolog.Nop())
	if err == nil {
		t.Fatal("an unparseable schedule was accepted")
	}
	if !strings.Contains(err.Error(), "every tuesday-ish") {
		t.Errorf("the error should quote what it could not read, got %v", err)
	}
}

// Not tested here: a schedule restricting both day-of-month and day-of-week.
// croncal.Translate rejects that as ambiguous -- cron ORs them, systemd ANDs
// them -- but croncal.Next, which is what serve uses, deliberately accepts it
// and ORs as cron does. Asserting a refusal here would be asserting a rule
// that lives in a function this never calls.

// An unreachable webhook must be given up on, not retried per finding: one
// DNS lookup each across hundreds of findings is what exhausts a resolver's
// rate limit and costs the detector the registry lookups it actually needs.
func TestAnUnreachableWebhookIsTriedOnce(t *testing.T) {
	log := zerolog.Nop()
	// Port 9 is discard: nothing listens, so the dial is refused immediately
	// rather than hanging on a timeout.
	r := newReporter("http://127.0.0.1:9/notify", "testhost", log)

	for i := 0; i < 50; i++ {
		r.report(detect.Finding{Service: "app", Tag: "1.0.1"})
	}

	if !r.unreachable {
		t.Fatal("a refused dial did not mark the webhook unreachable")
	}
	// One attempt, then 49 skipped without touching the network.
	if r.skipped != 49 {
		t.Errorf("skipped = %d, want 49 (one attempt, the rest dropped)", r.skipped)
	}
	// But every one of them still counts as unpublished, or the cutoff would
	// advance past findings nothing ever heard about.
	if r.unpublished() != 50 {
		t.Errorf("unpublished = %d, want 50", r.unpublished())
	}
}

// A refusal is not unreachability. A webhook that answers 500 is there, and
// the next finding must still be offered to it -- writing it off would drop
// findings over one bad request.
func TestARefusalDoesNotWriteOffTheWebhook(t *testing.T) {
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got++
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := newReporter(srv.URL, "testhost", zerolog.Nop())
	for i := 0; i < 5; i++ {
		r.report(detect.Finding{Service: "app", Tag: "1.0.1"})
	}

	if r.unreachable {
		t.Error("a 500 was treated as the webhook being unreachable")
	}
	if got != 5 {
		t.Errorf("the webhook was offered %d of 5 findings", got)
	}
	if r.unpublished() != 5 {
		t.Errorf("unpublished = %d, want 5", r.unpublished())
	}
}
