package main

import (
	"encoding/json"
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
// a rule holding in isolation is a tautology; whether one service's failure
// leaves another's line alone is the thing that matters.

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

// check runs one whole cycle and reports what each service's line did.
//
// Per service now, because the rule is: one service's failure must not move
// or hold another's line.
func check(t *testing.T, services []detect.Service, reg detect.Registry) (before, after map[string]time.Time, found, failed int) {
	return checkPublishing(t, services, reg, "")
}

// checkPublishing is check with a webhook. An empty url means there is nowhere
// to publish, which is the case every other test here runs under -- and which
// now means findings are *held* rather than dropped.
func checkPublishing(t *testing.T, services []detect.Service, reg detect.Registry, url string) (before, after map[string]time.Time, found, failed int) {
	t.Helper()

	mem, err := loadMemory(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadMemory: %v", err)
	}
	before = lines(mem, services)

	reporter := newReporter(url, "testhost", zerolog.Nop())
	handHeld(mem, reporter.report, zerolog.Nop())
	found, failed = checkAll(services, mem, reg, reporter.report, zerolog.Nop())
	if err := mem.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := loadMemory(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadMemory after: %v", err)
	}
	return before, lines(reloaded, services), found, failed
}

// lines is each service's cutoff, read back through the same accessor the
// detector uses -- so a test cannot pass by reading state a run never would.
func lines(mem *memory, services []detect.Service) map[string]time.Time {
	out := map[string]time.Time{}
	for _, svc := range services {
		out[svc.Name] = mem.Cutoff(svc.Name)
	}
	return out
}

// held is what is waiting to be handed on, by service.
func held(t *testing.T) map[string]int {
	t.Helper()
	mem, err := loadMemory(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadMemory: %v", err)
	}
	out := map[string]int{}
	for _, f := range mem.Held() {
		out[f.Service]++
	}
	return out
}

// --- the rule that matters ---------------------------------------------------

// A complete run earns the right to move the line forward.
func TestACompleteRunAdvancesTheCutoff(t *testing.T) {
	useTempState(t)
	// Seed a cutoff, so "advanced" means something rather than "a first run
	// wrote one".
	seeded := time.Now().Add(-48 * time.Hour)
	seedCutoff(t, seeded, "app", "ok", "down", "a", "b")

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
	if !after["app"].After(before["app"]) {
		t.Errorf("app's line did not move: %v -> %v", before["app"], after["app"])
	}
}

// A run with a hole in it must not. Whatever the unreachable service
// published falls between the old cutoff and the new one, and is never
// reported again -- silently, and forever.
func TestAFailedServiceLeavesTheCutoffAlone(t *testing.T) {
	useTempState(t)
	seeded := time.Now().Add(-48 * time.Hour)
	seedCutoff(t, seeded, "app", "ok", "down", "a", "b")

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
	// The broken one is pinned; the healthy one is not. That is the whole
	// change: one failure no longer costs every other service its line.
	if !after["down"].Equal(before["down"]) {
		t.Errorf("down's line moved despite being unreachable: %v -> %v", before["down"], after["down"])
	}
	if !after["ok"].After(before["ok"]) {
		t.Errorf("ok's line was held by another service's failure: %v -> %v", before["ok"], after["ok"])
	}
}

// And the consequence, for the service that failed: its line never moved, so
// once its registry comes back the tag it published in the meantime is still
// seen. Noisy is the correct failure mode; silently skipped is not.
//
// Scoped to that service. Under the old whole-run rule this test asserted
// that a *healthy* service re-reported too, because one failure pinned the
// single line for everything -- which is exactly the behaviour being removed.
func TestAfterAFailureTheNextRunStillSeesTheFailedServicesTag(t *testing.T) {
	useTempState(t)
	seedCutoff(t, time.Now().Add(-48*time.Hour), "ok", "down")

	ok := service("ok", "example.com/ok", "1.0.0")
	down := service("down", "example.com/down", "1.0.0")
	reg := fakeReg{
		tags: map[string][]string{
			"example.com/ok":   {"1.1.0"},
			"example.com/down": {"2.1.0"},
		},
		dates: map[string]time.Time{
			"1.1.0": time.Now().Add(-time.Hour),
			"2.1.0": time.Now().Add(-time.Hour),
		},
		broken: map[string]bool{"example.com/down": true},
	}

	// First run: ok's tag is found, down cannot be reached at all.
	_, _, firstFound, firstFailed := check(t, []detect.Service{ok, down}, reg.registry())
	if firstFound != 1 || firstFailed != 1 {
		t.Fatalf("first run found=%d failed=%d, want 1 and 1", firstFound, firstFailed)
	}

	// down's registry comes back. Its tag must still be visible, because its
	// line never moved past it -- and ok's must not be reported again,
	// because its line did move.
	reg.broken = nil
	_, _, secondFound, _ := check(t, []detect.Service{ok, down}, reg.registry())
	if secondFound != 1 {
		t.Errorf("second run found %d, want only down's tag: its line should not have passed it, "+
			"and ok's should have", secondFound)
	}
}

// The other half: after a clean run, the same tag is not reported twice.
// Without this the rule above would be satisfied by a detector that never
// advanced at all.
func TestAfterACleanRunTheSameTagIsNotReportedAgain(t *testing.T) {
	useTempState(t)
	seedCutoff(t, time.Now().Add(-48*time.Hour), "app", "ok", "down", "a", "b")

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
	seedCutoff(t, time.Now().Add(-48*time.Hour), "app", "ok", "down", "a", "b")

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
	for _, name := range []string{"a", "b"} {
		if !after[name].Equal(before[name]) {
			t.Errorf("%s moved despite failing: %v -> %v", name, before[name], after[name])
		}
	}
}

// A clean run that found nothing still advances. "Nothing new" is a complete
// answer, and refusing to move on it would mean the window grew forever on a
// quiet stack.
func TestACleanRunWithNoFindingsStillAdvances(t *testing.T) {
	useTempState(t)
	seedCutoff(t, time.Now().Add(-48*time.Hour), "app", "ok", "down", "a", "b")

	reg := fakeReg{
		tags:  map[string][]string{"example.com/app": {"1.0.0"}},
		dates: map[string]time.Time{"1.0.0": time.Now().Add(-72 * time.Hour)},
	}
	before, after, found, failed := check(t,
		[]detect.Service{service("app", "example.com/app", "1.0.0")}, reg.registry())

	if found != 0 || failed != 0 {
		t.Fatalf("found=%d failed=%d, want a clean and empty run", found, failed)
	}
	if !after["app"].After(before["app"]) {
		t.Errorf("a clean run that found nothing did not advance: %v -> %v", before["app"], after["app"])
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

// seedCutoff gives every named service a starting line, so "advanced" means
// something rather than "a first run wrote one".
func seedCutoff(t *testing.T, at time.Time, services ...string) {
	t.Helper()
	mem, err := loadMemory(zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range services {
		mem.Checked(s, at)
	}
	if err := mem.save(); err != nil {
		t.Fatal(err)
	}
}

// --- what is held, and when it is handed on ----------------------------------

// No webhook configured is a legitimate way to run: findings go to the log and
// wait. The moment there is somewhere to send them, they are sent.
func TestFindingsHeldWithNoWebhookAreSentWhenOneAppears(t *testing.T) {
	useTempState(t)
	seedCutoff(t, time.Now().Add(-48*time.Hour), "app")

	svc := service("app", "example.com/app", "1.0.0")
	reg := fakeReg{
		tags:  map[string][]string{"example.com/app": {"1.1.0"}},
		dates: map[string]time.Time{"1.1.0": time.Now().Add(-time.Hour)},
	}

	// Run with nowhere to publish.
	_, _, found, _ := check(t, []detect.Service{svc}, reg.registry())
	if found != 1 {
		t.Fatalf("found %d, want the one new tag", found)
	}
	if h := held(t)["app"]; h != 1 {
		t.Fatalf("held %d findings, want the one that could not be published", h)
	}

	// A webhook appears. The backlog goes first, and the tag is not found
	// again -- the line moved when it was *checked*, not when it was sent.
	var got []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e Event
		json.NewDecoder(r.Body).Decode(&e)
		got = append(got, e)
	}))
	defer srv.Close()

	_, _, secondFound, _ := checkPublishing(t, []detect.Service{svc}, reg.registry(), srv.URL)
	if secondFound != 0 {
		t.Errorf("the second run found %d, want 0: the line advanced when it was checked", secondFound)
	}
	if len(got) != 1 || got[0].Tag != "1.1.0" {
		t.Fatalf("the webhook received %+v, want the held 1.1.0", got)
	}
	if h := held(t)["app"]; h != 0 {
		t.Errorf("%d findings are still held after being handed on", h)
	}
}

// The line moves when a service is checked, not when its findings land. That
// separation is what stops an unreachable webhook costing a weekly re-scan.
func TestAnUnpublishableFindingStillAdvancesTheLine(t *testing.T) {
	useTempState(t)
	seedCutoff(t, time.Now().Add(-48*time.Hour), "app")

	reg := fakeReg{
		tags:  map[string][]string{"example.com/app": {"1.1.0"}},
		dates: map[string]time.Time{"1.1.0": time.Now().Add(-time.Hour)},
	}
	// Port 9 is discard: nothing listens, so the webhook is unreachable.
	before, after, found, failed := checkPublishing(t,
		[]detect.Service{service("app", "example.com/app", "1.0.0")},
		reg.registry(), "http://127.0.0.1:9/notify")

	if found != 1 || failed != 0 {
		t.Fatalf("found=%d failed=%d, want the tag found and the check clean", found, failed)
	}
	if !after["app"].After(before["app"]) {
		t.Error("the line did not move: looking is not publishing, and the check succeeded")
	}
	if h := held(t)["app"]; h != 1 {
		t.Errorf("held %d, want the unpublishable finding kept for the next run", h)
	}
}
