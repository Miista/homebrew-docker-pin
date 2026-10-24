package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/watch"
	"github.com/Miista/homebrew-docker-pin/internal/croncal"
)

// The rules the watcher exists to keep, tested as behaviour rather than as
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

func (f fakeReg) registry() watch.Registry {
	return watch.Registry{
		ListTags: func(image string) ([]watch.DatedTag, error) {
			if f.broken[image] {
				return nil, errors.New("connection refused")
			}
			var out []watch.DatedTag
			for _, name := range f.tags[image] {
				out = append(out, watch.DatedTag{Name: name, Published: f.dates[name]})
			}
			return out, nil
		},
		TagCreated: func(_, tag string) (time.Time, error) {
			return f.dates[tag], nil
		},
	}
}

func service(name, image, tag string) watch.Service {
	return watch.Service{Name: name, Container: name, Image: image, Tag: tag}
}

// check runs one whole cycle and reports what each service's line did.
//
// Per service now, because the rule is: one service's failure must not move
// or hold another's line.
func check(t *testing.T, services []watch.Service, reg watch.Registry) (before, after map[string]time.Time, found, failed int) {
	return checkPublishing(t, services, reg, "")
}

// checkPublishing is check with a webhook. An empty url means there is nowhere
// to publish, which is the case every other test here runs under -- and which
// now means findings are *held* rather than dropped.
func checkPublishing(t *testing.T, services []watch.Service, reg watch.Registry, url string) (before, after map[string]time.Time, found, failed int) {
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
// watcher uses -- so a test cannot pass by reading state a run never would.
func lines(mem *memory, services []watch.Service) map[string]time.Time {
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
		[]watch.Service{service("app", "example.com/app", "1.0.0")}, reg.registry())

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
	before, after, _, failed := check(t, []watch.Service{
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
	_, _, firstFound, firstFailed := check(t, []watch.Service{ok, down}, reg.registry())
	if firstFound != 1 || firstFailed != 1 {
		t.Fatalf("first run found=%d failed=%d, want 1 and 1", firstFound, firstFailed)
	}

	// down's registry comes back. Its tag must still be visible, because its
	// line never moved past it -- and ok's must not be reported again,
	// because its line did move.
	reg.broken = nil
	_, _, secondFound, _ := check(t, []watch.Service{ok, down}, reg.registry())
	if secondFound != 1 {
		t.Errorf("second run found %d, want only down's tag: its line should not have passed it, "+
			"and ok's should have", secondFound)
	}
}

// The other half: after a clean run, the same tag is not reported twice.
// Without this the rule above would be satisfied by a watcher that never
// advanced at all.
func TestAfterACleanRunTheSameTagIsNotReportedAgain(t *testing.T) {
	useTempState(t)
	seedCutoff(t, time.Now().Add(-48*time.Hour), "app", "ok", "down", "a", "b")

	svc := service("app", "example.com/app", "1.0.0")
	reg := fakeReg{
		tags:  map[string][]string{"example.com/app": {"1.1.0"}},
		dates: map[string]time.Time{"1.1.0": time.Now().Add(-time.Hour)},
	}

	_, _, firstFound, _ := check(t, []watch.Service{svc}, reg.registry())
	if firstFound != 1 {
		t.Fatalf("first run found %d, want 1", firstFound)
	}

	_, _, secondFound, _ := check(t, []watch.Service{svc}, reg.registry())
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
	before, after, _, failed := check(t, []watch.Service{
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
		[]watch.Service{service("app", "example.com/app", "1.0.0")}, reg.registry())

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
	t.Setenv("DUVA_SCHEDULE", "")
	if got := scheduleFromEnv(); got != defaultSchedule {
		t.Errorf("schedule = %q, want the default %q", got, defaultSchedule)
	}
}

func TestAnExplicitScheduleWins(t *testing.T) {
	t.Setenv("DUVA_SCHEDULE", "0 */6 * * *")
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

// A schedule that does not parse stops the watcher at startup rather than at
// the first tick -- which for a daily schedule would be a day later, with
// nothing in the log to say why nothing happened.
func TestServeRefusesAnUnparseableSchedule(t *testing.T) {
	t.Setenv("DUVA_SCHEDULE", "every tuesday-ish")
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
// rate limit and costs the watcher the registry lookups it actually needs.
func TestAnUnreachableWebhookIsTriedOnce(t *testing.T) {
	log := zerolog.Nop()
	// Port 9 is discard: nothing listens, so the dial is refused immediately
	// rather than hanging on a timeout.
	r := newReporter("http://127.0.0.1:9/notify", "testhost", log)

	for i := 0; i < 50; i++ {
		r.report(watch.Finding{Service: "app", Tag: "1.0.1"})
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
		r.report(watch.Finding{Service: "app", Tag: "1.0.1"})
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
	_, _, found, _ := check(t, []watch.Service{svc}, reg.registry())
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

	_, _, secondFound, _ := checkPublishing(t, []watch.Service{svc}, reg.registry(), srv.URL)
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
		[]watch.Service{service("app", "example.com/app", "1.0.0")},
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

// A run named after a service checks that one and nothing else.
func TestRunNarrowsToTheNamedService(t *testing.T) {
	all := []watch.Service{
		{Name: "wiki", Container: "wiki"},
		{Name: "whoami", Container: "whoami"},
		{Name: "cloudflared", Container: "cloudflared-wr"},
	}

	got := justOne(all, "whoami")
	if len(got) != 1 || got[0].Name != "whoami" {
		t.Fatalf("justOne(whoami) = %+v, want just whoami", got)
	}
}

// The name a person has to hand is whatever docker ps showed them, which in
// this project is often not the service name.
func TestRunAcceptsTheContainerName(t *testing.T) {
	all := []watch.Service{
		{Name: "cloudflared", Container: "cloudflared-wr"},
	}

	got := justOne(all, "cloudflared-wr")
	if len(got) != 1 || got[0].Name != "cloudflared" {
		t.Fatalf("justOne(cloudflared-wr) = %+v, want the cloudflared service", got)
	}
}

// A name that matches nothing must not quietly check everything -- that would
// turn a typo into a full run against every service on the host.
func TestRunRefusesANameItDoesNotWatch(t *testing.T) {
	all := []watch.Service{
		{Name: "wiki", Container: "wiki"},
	}

	if got := justOne(all, "wik"); got != nil {
		t.Fatalf("justOne(wik) = %+v, want nil so the run refuses", got)
	}
}

// A service that reached no usable tag keeps its old cutoff.
//
// The point of the error: a check that confirmed nothing must not record a
// line it never earned, or every later run trusts it and the service is
// silently never checked properly again.
func TestAServiceThatReachedNoTagDoesNotAdvance(t *testing.T) {
	useTempState(t)
	// Seeded, so "did not advance" is a line staying put rather than one
	// never written.
	seeded := time.Now().Add(-48 * time.Hour)
	seedCutoff(t, seeded, "app")

	// Publishes only a tag the include rejects, so nothing is usable.
	reg := fakeReg{
		tags:  map[string][]string{"example.com/app": {"latest"}},
		dates: map[string]time.Time{"latest": time.Now().Add(-90 * 24 * time.Hour)},
	}
	svc := service("app", "example.com/app", "1.0.0")
	svc.Include = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

	before, after, found, failed := check(t, []watch.Service{svc}, reg.registry())

	if failed != 1 {
		t.Errorf("failed = %d, want the service counted as not checked", failed)
	}
	if found != 0 {
		t.Errorf("found = %d, want nothing -- no tag was usable", found)
	}
	if !after["app"].Equal(before["app"]) {
		t.Errorf("the cutoff moved from %v to %v for a check that reached no tag",
			before["app"], after["app"])
	}
}

// A normal run needs only new tags since the cutoff, and advances either way.
//
// The counterpart to a first run having to reach a real tag however far back.
// Once a service has a line, "nothing published since then" is the ordinary
// healthy answer -- not a failure -- and the line still moves, or a quiet
// project would be re-asked the same question forever.
func TestANormalRunNeedsNothingButAdvancesAnyway(t *testing.T) {
	useTempState(t)
	seeded := time.Now().Add(-48 * time.Hour)
	seedCutoff(t, seeded, "app")

	// Published long before the cutoff, so a normal run has nothing to say --
	// and the service is not on it, so this is not the running-tag case.
	reg := fakeReg{
		tags: map[string][]string{"example.com/app": {"1.0.0", "0.9.0"}},
		dates: map[string]time.Time{
			"1.0.0": time.Now().Add(-300 * 24 * time.Hour),
			"0.9.0": time.Now().Add(-400 * 24 * time.Hour),
		},
	}

	before, after, found, failed := check(t,
		[]watch.Service{service("app", "example.com/app", "1.0.0")}, reg.registry())

	if failed != 0 {
		t.Errorf("failed = %d, want a clean run -- old tags are not a failure", failed)
	}
	if found != 0 {
		t.Errorf("found = %d, want nothing new since the cutoff", found)
	}
	if !after["app"].After(before["app"]) {
		t.Errorf("the line did not advance: %v -> %v", before["app"], after["app"])
	}
}

// And the first run of that same world reports the newest tag, however old.
func TestAFirstRunOfTheSameWorldReportsTheNewest(t *testing.T) {
	useTempState(t)
	// No seeded cutoff: this is a first check.

	reg := fakeReg{
		tags: map[string][]string{"example.com/app": {"1.1.0", "1.0.0", "0.9.0"}},
		dates: map[string]time.Time{
			"1.1.0": time.Now().Add(-300 * 24 * time.Hour),
			"1.0.0": time.Now().Add(-350 * 24 * time.Hour), // the tag it runs
			"0.9.0": time.Now().Add(-400 * 24 * time.Hour),
		},
	}

	_, _, found, failed := check(t,
		[]watch.Service{service("app", "example.com/app", "1.0.0")}, reg.registry())

	if failed != 0 {
		t.Errorf("failed = %d, want a clean run", failed)
	}
	if found != 1 {
		t.Errorf("found = %d, want the newest tag reported however old it is", found)
	}
}
