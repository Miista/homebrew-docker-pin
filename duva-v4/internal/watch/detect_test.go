package watch

import (
	"errors"
	"regexp"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// fakeRegistry answers from a table, so a test says what a registry holds
// rather than what a mock was told to expect.
//
// datedListing models the two kinds of registry this has to work with: one
// that returns publish times with its listing (Docker Hub) and one that does
// not (everything else), which is the difference between one request and one
// per tag.
type fakeRegistry struct {
	tags      []string
	published map[string]time.Time
	listErr   error
	dateErr   map[string]error
	// datedListing makes the listing carry publish times, as Docker Hub's
	// does. False means the dates have to be asked for one at a time.
	datedListing bool
	// dateCalls counts the expensive path, for asserting that a filter ran
	// before any date was fetched.
	dateCalls int
}

func (f *fakeRegistry) registry() Registry {
	return Registry{
		ListTags: func(string) ([]DatedTag, error) {
			if f.listErr != nil {
				return nil, f.listErr
			}
			out := make([]DatedTag, 0, len(f.tags))
			for _, name := range f.tags {
				d := DatedTag{Name: name}
				if f.datedListing {
					d.Published = f.published[name]
				}
				out = append(out, d)
			}
			return out, nil
		},
		TagCreated: func(_ string, tag string) (time.Time, error) {
			f.dateCalls++
			if err, ok := f.dateErr[tag]; ok {
				return time.Time{}, err
			}
			t, ok := f.published[tag]
			if !ok {
				return time.Time{}, errors.New("no such tag")
			}
			return t, nil
		},
	}
}

// collect runs a check and gathers what it streamed, for tests that assert on
// the whole set rather than on the streaming itself.
func collect(svc Service, cutoff time.Time, reg Registry) ([]Finding, error) {
	var out []Finding
	err := Since(svc, cutoff, reg, func(f Finding) { out = append(out, f) })
	return out, err
}

var cutoff = at("2026-09-01T00:00:00Z")

func svc(tag string, include, exclude string) Service {
	s := Service{Name: "app", Container: "my-app", Image: "example.com/app", Tag: tag}
	if include != "" {
		s.Include = regexp.MustCompile(include)
	}
	if exclude != "" {
		s.Exclude = regexp.MustCompile(exclude)
	}
	return s
}

func TestReportsTagsPublishedAfterTheCutoff(t *testing.T) {
	reg := &fakeRegistry{
		tags: []string{"1.0.0", "1.1.0", "1.2.0"},
		published: map[string]time.Time{
			"1.0.0": at("2026-08-01T00:00:00Z"), // before
			"1.1.0": at("2026-09-05T00:00:00Z"), // after
			"1.2.0": at("2026-09-10T00:00:00Z"), // after
		},
	}
	res, resErr := collect(svc("1.0.0", "", ""), cutoff, reg.registry())
	if resErr != nil {
		t.Fatalf("Since: %v", resErr)
	}
	if len(res) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(res), res)
	}
	if res[0].Tag != "1.1.0" || res[1].Tag != "1.2.0" {
		t.Errorf("got %q then %q, want 1.1.0 then 1.2.0", res[0].Tag, res[1].Tag)
	}
}

// Every qualifying tag is handed to the callback, once each.
//
// That it happens *during* the check rather than at the end is what makes a
// slow service legible, but it is not something a unit test can see: the
// callback firing early and the callback firing late look identical from
// here. What this can check is that nothing is dropped or repeated.
func TestEveryQualifyingTagIsReported(t *testing.T) {
	reg := &fakeRegistry{
		tags: []string{"early", "middle", "late"},
		published: map[string]time.Time{
			"early":  at("2026-09-02T00:00:00Z"),
			"middle": at("2026-09-10T00:00:00Z"),
			"late":   at("2026-09-20T00:00:00Z"),
		},
	}

	seen := map[string]int{}
	err := Since(svc("1.0.0", "", ""), cutoff, reg.registry(), func(f Finding) {
		seen[f.Tag]++
	})
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	for _, tag := range []string{"early", "middle", "late"} {
		if seen[tag] != 1 {
			t.Errorf("%s was reported %d times, want once", tag, seen[tag])
		}
	}
	if len(seen) != 3 {
		t.Errorf("reported %d distinct tags, want 3: %v", len(seen), seen)
	}
}

// Docker Hub returns publish times with its listing, so a tag's date costs
// nothing extra. Asking anyway would turn one request into one per tag --
// hundreds for a repository like paperless, which is slow enough to read as
// a hang and fast enough to earn a 429.
func TestADatedListingCostsNoExtraRequests(t *testing.T) {
	reg := &fakeRegistry{
		datedListing: true,
		tags:         []string{"1.1.0", "1.2.0"},
		published: map[string]time.Time{
			"1.1.0": at("2026-09-05T00:00:00Z"),
			"1.2.0": at("2026-09-10T00:00:00Z"),
		},
	}
	res, err := collect(svc("1.0.0", "", ""), cutoff, reg.registry())
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %+v, want both tags", res)
	}
	if reg.dateCalls != 0 {
		t.Errorf("asked for %d dates when the listing already carried them", reg.dateCalls)
	}
}

// And the other kind: a registry that does not date its listing is asked per
// surviving tag, which is correct and merely expensive.
func TestAnUndatedListingAsksPerTag(t *testing.T) {
	reg := &fakeRegistry{
		tags: []string{"1.1.0", "1.2.0"},
		published: map[string]time.Time{
			"1.1.0": at("2026-09-05T00:00:00Z"),
			"1.2.0": at("2026-09-10T00:00:00Z"),
		},
	}
	if _, err := collect(svc("1.0.0", "", ""), cutoff, reg.registry()); err != nil {
		t.Fatalf("Since: %v", err)
	}
	if reg.dateCalls != 2 {
		t.Errorf("asked for %d dates, want one per surviving tag", reg.dateCalls)
	}
}

// Nothing published since is a clean empty answer, not an error.
func TestNothingNewIsNotAnError(t *testing.T) {
	reg := &fakeRegistry{
		tags:      []string{"1.0.0"},
		published: map[string]time.Time{"1.0.0": at("2026-08-01T00:00:00Z")},
	}
	res, resErr := collect(svc("0.9.0", "", ""), cutoff, reg.registry())
	if resErr != nil {
		t.Errorf("err = %v, want none", resErr)
	}
	if len(res) != 0 {
		t.Errorf("got %+v, want nothing", res)
	}
}

// A tag published exactly at the cutoff is not after it. The boundary has to
// fall one way, and "strictly after" means a tag is never reported twice by
// two consecutive runs.
func TestATagAtTheCutoffIsNotNew(t *testing.T) {
	reg := &fakeRegistry{
		tags:      []string{"1.1.0"},
		published: map[string]time.Time{"1.1.0": cutoff},
	}
	res, _ := collect(svc("1.0.0", "", ""), cutoff, reg.registry())
	if len(res) != 0 {
		t.Errorf("a tag published exactly at the cutoff was reported: %+v", res)
	}
}

// --- what qualifies ----------------------------------------------------------

// The tag the file already follows is never a finding: reporting it would be
// telling the operator about where they already are.
func TestTheCurrentTagIsNeverAFinding(t *testing.T) {
	reg := &fakeRegistry{
		tags:      []string{"latest"},
		published: map[string]time.Time{"latest": at("2026-09-10T00:00:00Z")},
	}
	res, _ := collect(svc("latest", "", ""), cutoff, reg.registry())
	if len(res) != 0 {
		t.Errorf("the current tag was reported: %+v", res)
	}
}

func TestIncludeFiltersTags(t *testing.T) {
	reg := &fakeRegistry{
		tags: []string{"1.1.0", "nightly", "1.2.0"},
		published: map[string]time.Time{
			"1.1.0":   at("2026-09-05T00:00:00Z"),
			"nightly": at("2026-09-06T00:00:00Z"),
			"1.2.0":   at("2026-09-07T00:00:00Z"),
		},
	}
	res, _ := collect(svc("1.0.0", `^\d+\.\d+\.\d+$`, ""), cutoff, reg.registry())
	if len(res) != 2 {
		t.Fatalf("got %+v, want the two version tags", res)
	}
	for _, f := range res {
		if f.Tag == "nightly" {
			t.Error("nightly survived the include")
		}
	}
}

func TestExcludeFiltersTags(t *testing.T) {
	reg := &fakeRegistry{
		tags: []string{"1.1.0", "1.2.0-rc1"},
		published: map[string]time.Time{
			"1.1.0":     at("2026-09-05T00:00:00Z"),
			"1.2.0-rc1": at("2026-09-06T00:00:00Z"),
		},
	}
	res, _ := collect(svc("1.0.0", "", `(alpha|beta|rc)`), cutoff, reg.registry())
	if len(res) != 1 || res[0].Tag != "1.1.0" {
		t.Errorf("got %+v, want only 1.1.0", res)
	}
}

// Filtering happens before the dates are fetched. A repository with hundreds
// of tags would otherwise mean hundreds of requests to answer a question the
// include pattern dismisses for free.
func TestFilteringHappensBeforeFetchingDates(t *testing.T) {
	reg := &fakeRegistry{
		tags:      []string{"1.1.0", "junk-a", "junk-b", "junk-c", "junk-d"},
		published: map[string]time.Time{"1.1.0": at("2026-09-05T00:00:00Z")},
	}
	collect(svc("1.0.0", `^\d+\.\d+\.\d+$`, ""), cutoff, reg.registry())
	if reg.dateCalls != 1 {
		t.Errorf("asked for %d dates, want 1 -- the include should have dismissed the rest", reg.dateCalls)
	}
}

// --- failures ----------------------------------------------------------------

// A registry that cannot be reached is reported, not treated as "nothing
// new". Silence and failure must not look the same.
func TestAnUnreachableRegistryIsReported(t *testing.T) {
	reg := &fakeRegistry{listErr: errors.New("connection refused")}
	res, resErr := collect(svc("1.0.0", "", ""), cutoff, reg.registry())
	if resErr == nil {
		t.Fatal("want an error when the registry cannot be listed")
	}
	if len(res) != 0 {
		t.Error("it reported findings despite failing")
	}
}

// One tag whose date cannot be read is skipped, not fatal: the rest of the
// service is still answerable.
func TestATagWithNoReadableDateIsSkipped(t *testing.T) {
	reg := &fakeRegistry{
		tags:      []string{"1.1.0", "1.2.0"},
		published: map[string]time.Time{"1.2.0": at("2026-09-10T00:00:00Z")},
		dateErr:   map[string]error{"1.1.0": errors.New("no such manifest")},
	}
	res, resErr := collect(svc("1.0.0", "", ""), cutoff, reg.registry())
	if resErr != nil {
		t.Errorf("one unreadable date failed the whole service: %v", resErr)
	}
	if len(res) != 1 || res[0].Tag != "1.2.0" {
		t.Errorf("got %+v, want just 1.2.0", res)
	}
}

// --- what a finding carries --------------------------------------------------

// The container name travels with the finding: it is how the gate matches
// this back to a service, and the watcher is the only one that knows it.
func TestAFindingCarriesTheContainer(t *testing.T) {
	reg := &fakeRegistry{
		tags:      []string{"1.1.0"},
		published: map[string]time.Time{"1.1.0": at("2026-09-05T00:00:00Z")},
	}
	res, _ := collect(svc("1.0.0", "", ""), cutoff, reg.registry())
	if len(res) != 1 {
		t.Fatalf("got %+v", res)
	}
	f := res[0]
	if f.Container != "my-app" || f.Service != "app" || f.Image != "example.com/app" {
		t.Errorf("finding = %+v", f)
	}
	if f.Published.IsZero() {
		t.Error("the publish time is missing")
	}
}

// The watcher has no opinion about versions. A tag that is older by semver
// but newer by publish date is still reported -- deciding that 1.0.1 is not
// an upgrade from 2.0.0 is the gate's job, and doing it here would be two
// things that have to agree about ordering.
func TestItHasNoVersionOpinion(t *testing.T) {
	reg := &fakeRegistry{
		tags:      []string{"1.0.1"},
		published: map[string]time.Time{"1.0.1": at("2026-09-10T00:00:00Z")},
	}
	res, _ := collect(svc("2.0.0", "", ""), cutoff, reg.registry())
	if len(res) != 1 {
		t.Errorf("a backport published after the cutoff was filtered out; that is the gate's call, not ours")
	}
}
