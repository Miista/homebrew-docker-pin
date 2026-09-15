package watch

import (
	"errors"
	"regexp"
	"strings"
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

// collectFirst runs a first check and gathers what it streamed.
func collectFirst(svc Service, cutoff time.Time, reg Registry) ([]Finding, error) {
	var out []Finding
	err := AtLeastOne(svc, cutoff, reg, func(f Finding) { out = append(out, f) })
	return out, err
}

// A service whose newest release predates the window still reports it, once.
//
// Without this a quiet project reports nothing on its first check, which
// reads as "up to date" when it is not -- and stays that way until someone
// publishes again, which may be never.
func TestAFirstCheckFindsSomethingEvenWhenNothingIsRecent(t *testing.T) {
	reg := &fakeRegistry{
		tags:         []string{"1.0.0", "1.1.0", "1.2.0"},
		datedListing: true,
		published: map[string]time.Time{
			// All well before the cutoff: nothing is "new".
			"1.0.0": at("2026-01-01T00:00:00Z"),
			"1.1.0": at("2026-02-01T00:00:00Z"),
			"1.2.0": at("2026-03-01T00:00:00Z"),
		},
	}

	// The ordinary check finds nothing, which is the behaviour being fixed.
	if got, err := collect(svc("1.0.0", "", ""), cutoff, reg.registry()); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("a windowed check found %v, want nothing older than the cutoff", got)
	}

	got, err := collectFirst(svc("1.0.0", "", ""), cutoff, reg.registry())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a first check found %d findings, want exactly one", len(got))
	}
	// The newest, not merely the first the registry happened to list.
	if got[0].Tag != "1.2.0" {
		t.Errorf("reported %q, want the newest tag there is", got[0].Tag)
	}
}

// When the window does find something, the fallback stays out of the way:
// reporting the newest as well would duplicate it.
func TestAFirstCheckThatFindsSomethingReportsOnlyThat(t *testing.T) {
	reg := &fakeRegistry{
		tags:         []string{"1.0.0", "1.2.0"},
		datedListing: true,
		published: map[string]time.Time{
			"1.0.0": at("2026-01-01T00:00:00Z"),
			"1.2.0": at("2026-09-15T00:00:00Z"), // after the cutoff
		},
	}

	got, err := collectFirst(svc("1.0.0", "", ""), cutoff, reg.registry())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tag != "1.2.0" {
		t.Errorf("first check reported %v, want just the tag the window found", got)
	}
}

// The include pattern still decides what qualifies. A fallback that ignored it
// would report a tag the service has said it does not follow.
func TestAFirstCheckStillHonoursTheIncludePattern(t *testing.T) {
	reg := &fakeRegistry{
		tags:         []string{"1.0.0", "1.2.0", "2.0.0-rc1"},
		datedListing: true,
		published: map[string]time.Time{
			"1.0.0":     at("2025-06-01T00:00:00Z"), // the tag it runs
			"1.2.0":     at("2026-01-01T00:00:00Z"),
			"2.0.0-rc1": at("2026-02-01T00:00:00Z"), // newer, but excluded
		},
	}

	got, err := collectFirst(svc("1.0.0", `^\d+\.\d+\.\d+$`, ""), cutoff, reg.registry())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tag != "1.2.0" {
		t.Errorf("first check reported %v, want the newest tag the include allows", got)
	}
}

// A first check that reaches no usable tag is an error, not silence.
//
// Silence would record a cutoff the watcher never earned -- it would trust
// that line on every later run, having never confirmed a single tag behind it.
// An error leaves the cutoff where it was and asks again next time.
func TestAFirstCheckThatReachesNoTagIsAnError(t *testing.T) {
	// The repository publishes only a tag the include rejects.
	reg := &fakeRegistry{tags: []string{"latest"}, datedListing: true,
		published: map[string]time.Time{"latest": at("2026-01-01T00:00:00Z")}}

	got, err := collectFirst(svc("1.0.0", `^\d+\.\d+\.\d+$`, ""), cutoff, reg.registry())
	if err == nil {
		t.Fatalf("no error, and reported %v -- a cutoff would be recorded for a check that reached nothing", got)
	}
	var noTag *NoTagError
	if !errors.As(err, &noTag) {
		t.Fatalf("error is %T, want a NoTagError the caller can tell from an unreachable registry", err)
	}
	// The message names the tag it could not find, because that is what makes
	// this different from "nothing new": a tag a container is running must be
	// in the repository it was pulled from.
	if !strings.Contains(err.Error(), "1.0.0") {
		t.Errorf("the error does not name the running tag it could not find: %v", err)
	}
}

// An empty repository says so plainly rather than blaming the include.
func TestAFirstCheckAgainstAnEmptyRepositorySaysSo(t *testing.T) {
	reg := &fakeRegistry{tags: nil, datedListing: true}

	_, err := collectFirst(svc("1.0.0", "", ""), cutoff, reg.registry())
	if err == nil {
		t.Fatal("no error for a repository with no tags at all")
	}
	if !strings.Contains(err.Error(), "no tags at all") {
		t.Errorf("an empty repository is reported as something else: %v", err)
	}
}

// The running tag is absent, which on a first check is the serious failure:
// a container is running an image pulled from this repository, so that tag
// being missing means it was deleted, the image moved, or this is not the
// repository it runs from.
func TestAFirstCheckWhoseDatesAllFailIsAnError(t *testing.T) {
	reg := &fakeRegistry{
		tags: []string{"1.1.0", "1.2.0"},
		dateErr: map[string]error{
			"1.1.0": errors.New("unreadable"),
			"1.2.0": errors.New("unreadable"),
		},
	}

	_, err := collectFirst(svc("1.0.0", "", ""), cutoff, reg.registry())
	if err == nil {
		t.Fatal("no error when no tag's date could be read")
	}
}

// Seeing the tag this service already runs is proof the check reached a real
// tag, even though that tag is never a candidate -- it is what you are on, not
// somewhere to go. A repository publishing only that tag is a service up to
// date, not a check that failed.
func TestSeeingTheRunningTagIsProofEnough(t *testing.T) {
	reg := &fakeRegistry{
		tags:         []string{"1.0.0"},
		datedListing: true,
		published:    map[string]time.Time{"1.0.0": at("2026-01-01T00:00:00Z")},
	}

	got, err := collectFirst(svc("1.0.0", "", ""), cutoff, reg.registry())
	if err != nil {
		t.Fatalf("a service already on the only tag published is up to date, not a failure: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reported %v, want nothing -- the only tag is the one it runs", got)
	}
}

// movingSvc is a service following a stream, pinned to digest.
func movingSvc(tag, digest string) Service {
	return Service{Name: "app", Container: "my-app", Image: "example.com/app",
		Tag: tag, Digest: digest, Moving: true}
}

// movingReg answers what a tag points at now, which is the only question a
// moving tag raises.
func movingReg(now string) Registry {
	return Registry{
		ListTags: func(string) ([]DatedTag, error) {
			panic("a moving tag must not cost a listing: it is one request")
		},
		TagDigest: func(_, _ string) (string, error) { return now, nil },
	}
}

// latest points somewhere else, so it is reported -- whatever the age of
// whatever it points at. No window applies here, which is why a stream always
// answers on a first check and needs no fallback.
func TestAMovedStreamIsReported(t *testing.T) {
	got, err := collect(movingSvc("latest", "sha256:old"), cutoff, movingReg("sha256:new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("reported %v, want one finding", got)
	}
	if got[0].Digest != "sha256:new" {
		t.Errorf("reported digest %q, want what the tag points at now", got[0].Digest)
	}
	// The tag is what it follows, not a new one to move to.
	if got[0].Tag != "latest" {
		t.Errorf("reported tag %q, want the stream it follows", got[0].Tag)
	}
	// A moved stream has no version pair, and inventing one would make the
	// queue classify a change it cannot see.
	if got[0].Published != (time.Time{}) {
		t.Errorf("a moved stream carries a publish time: %v", got[0].Published)
	}
}

// The digest matches, so it is current and nothing is reported. This is the
// stop condition -- and it is also why a stream that is up to date stays
// silent rather than reporting the tag it already runs.
func TestAnUnmovedStreamIsSilent(t *testing.T) {
	got, err := collect(movingSvc("latest", "sha256:same"), cutoff, movingReg("sha256:same"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("reported %v, want nothing -- the tag has not moved", got)
	}
}

// A first check of a stream behaves identically: there is no window to widen,
// so AtLeastOne changes nothing here.
func TestAFirstCheckOfAStreamIsTheSameCheck(t *testing.T) {
	moved, err := collectFirst(movingSvc("latest", "sha256:old"), cutoff, movingReg("sha256:new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 {
		t.Errorf("a first check of a moved stream reported %v, want one", moved)
	}

	still, err := collectFirst(movingSvc("latest", "sha256:same"), cutoff, movingReg("sha256:same"))
	if err != nil {
		t.Fatal(err)
	}
	if len(still) != 0 {
		t.Errorf("a first check of an unmoved stream reported %v, want nothing", still)
	}
}

// Unpinned: there is no recorded digest to have moved away from, so reporting
// would be inventing a change out of the absence of a record.
func TestAnUnpinnedStreamIsNotAFinding(t *testing.T) {
	got, err := collect(movingSvc("latest", ""), cutoff, movingReg("sha256:whatever"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("reported %v for an unpinned service, want nothing", got)
	}
}

// A registry that cannot resolve digests cannot answer the question a stream
// raises. Silence would read as "nothing has changed", which is the failure
// this tool exists to prevent.
func TestAStreamAgainstADigestlessRegistryIsAnError(t *testing.T) {
	_, err := collect(movingSvc("latest", "sha256:old"), cutoff, Registry{})
	if err == nil {
		t.Fatal("no error for a registry that cannot resolve digests")
	}
	if !strings.Contains(err.Error(), "latest") {
		t.Errorf("the error does not name the tag it could not resolve: %v", err)
	}
}

// A first check must reach the tag the service is running, even when it found
// a candidate.
//
// Not "a tag" -- that one. A container is running an image pulled from this
// repository, so its tag is the one the registry is certain to have. Missing
// means it was deleted, the image moved, or this is not the repository the
// service runs from -- and recording a cutoff on that would be the watcher
// certifying a world it could not see.
func TestAFirstCheckWithoutTheRunningTagFailsEvenWithACandidate(t *testing.T) {
	reg := &fakeRegistry{
		// 2.0.0 is a fine candidate. 1.0.0, which the service runs, is gone.
		tags:         []string{"2.0.0"},
		datedListing: true,
		published:    map[string]time.Time{"2.0.0": at("2026-09-10T00:00:00Z")},
	}

	_, err := collectFirst(svc("1.0.0", "", ""), cutoff, reg.registry())
	if err == nil {
		t.Fatal("no error: a first check certified a repository missing the tag its container runs")
	}
	var noTag *NoTagError
	if !errors.As(err, &noTag) {
		t.Fatalf("error is %T, want a NoTagError", err)
	}
	if noTag.Tag != "1.0.0" {
		t.Errorf("the error names tag %q, want the one the service runs", noTag.Tag)
	}
}

// A later run does not assert it. The question there is narrower -- what
// appeared since the line -- and a tag deleted after the line was drawn is not
// that question. Asserting it every run would turn an upstream retention
// policy into a permanently failing service.
func TestALaterRunDoesNotRequireTheRunningTag(t *testing.T) {
	reg := &fakeRegistry{
		tags:         []string{"2.0.0"},
		datedListing: true,
		published:    map[string]time.Time{"2.0.0": at("2026-09-10T00:00:00Z")},
	}

	got, err := collect(svc("1.0.0", "", ""), cutoff, reg.registry())
	if err != nil {
		t.Fatalf("a later run failed for a tag that has since been deleted: %v", err)
	}
	if len(got) != 1 || got[0].Tag != "2.0.0" {
		t.Errorf("reported %v, want the candidate it found", got)
	}
}
