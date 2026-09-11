package main

import (
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/internal/detect"
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
	t.Helper()

	before, _, err := loadCutoff(zerolog.Nop())
	if err != nil {
		t.Fatalf("loadCutoff: %v", err)
	}

	startedAt := time.Now()
	found, failed = checkAll(services, before, reg, func(detect.Finding) {}, zerolog.Nop())

	if mayAdvance(failed) {
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
