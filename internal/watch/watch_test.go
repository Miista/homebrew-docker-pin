package watch

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// Detection is a function: a compose project and a set of registry answers in,
// a finding per service out. These test it as one, rather than through duva.

func project(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// reg builds a Registry whose answers are fixed per call.
func reg(tags []string, digest string, created time.Time) Registry {
	return Registry{
		ListMatchingTags: func(string, *regexp.Regexp, *regexp.Regexp, string) ([]string, error) {
			return tags, nil
		},
		RemoteDigest: func(string, string) (string, error) { return digest, nil },
		TagCreated:   func(string, string) (time.Time, error) { return created, nil },
	}
}

func one(t *testing.T, findings []Finding) Finding {
	t.Helper()
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	return findings[0]
}

func TestProject_StatusPerServiceShape(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantStatus Status
		wantReason string
	}{
		{
			name:       "unpinned is skipped",
			body:       "services:\n  a:\n    image: x/y:1.0.0\n",
			wantStatus: StatusSkipped, wantReason: "not pinned",
		},
		{
			name:       "built locally is skipped",
			body:       "services:\n  a:\n    build: ./a\n    image: local:dev\n",
			wantStatus: StatusSkipped, wantReason: "built locally (build:)",
		},
		{
			name:       "bad label is an error for that service",
			body:       "services:\n  a:\n    image: x/y:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4\n    labels:\n      duva.auto: wrong\n",
			wantStatus: StatusError,
		},
		{
			name:       "bad include regex is an error",
			body:       "services:\n  a:\n    image: x/y:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4\n    labels:\n      duva.include_tags: '^('\n",
			wantStatus: StatusError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := one(t, mustProject(t, project(t, tc.body), reg(nil, "", time.Now()), Baseline{}))
			if f.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (reason %q)", f.Status, tc.wantStatus, f.Reason)
			}
			if tc.wantReason != "" && f.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", f.Reason, tc.wantReason)
			}
		})
	}
}

func mustProject(t *testing.T, file string, r Registry, b Baseline) []Finding {
	t.Helper()
	got, err := Project(file, r, b)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// One service failing must never stop the others: a single unreachable
// registry would otherwise blind duva to the whole project.
func TestProject_OneFailureDoesNotStopTheRest(t *testing.T) {
	file := project(t, `services:
  broken:
    image: x/y:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4
    labels:
      duva.include_tags: '^\d'
  fine:
    image: x/z:latest@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4
`)
	calls := 0
	r := Registry{
		ListMatchingTags: func(string, *regexp.Regexp, *regexp.Regexp, string) ([]string, error) {
			calls++
			return nil, errors.New("registry unreachable")
		},
		RemoteDigest: func(string, string) (string, error) {
			return "sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437", nil
		},
	}
	findings := mustProject(t, file, r, Baseline{"fine": "sha256:cba06b5736faf67e54b07b561eae94395e774c517a7d910a54369e1263ccfbd4"})

	byName := map[string]Finding{}
	for _, f := range findings {
		byName[f.Service] = f
	}
	if byName["broken"].Status != StatusError {
		t.Errorf("broken should be an error, got %+v", byName["broken"])
	}
	if !byName["fine"].Available() {
		t.Errorf("fine should still have been checked, got %+v", byName["fine"])
	}
	if calls == 0 {
		t.Error("the failing registry was never called")
	}
}

// The soak is applied before anything else: a candidate younger than
// duva.delay is not a candidate at all, so it is never classified or queued.
func TestProject_SoakWithholdsFreshCandidates(t *testing.T) {
	file := project(t, `services:
  a:
    image: x/y:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4
    labels:
      duva.include_tags: '^\d+\.\d+\.\d+$'
      duva.delay: 7d
`)
	fresh := reg([]string{"1.0.0", "1.1.0"}, "", time.Now().Add(-time.Hour))
	if f := one(t, mustProject(t, file, fresh, Baseline{})); f.Available() {
		t.Errorf("a one-hour-old tag must not qualify against a 7d soak, got %+v", f)
	}

	soaked := reg([]string{"1.0.0", "1.1.0"}, "", time.Now().Add(-30*24*time.Hour))
	if f := one(t, mustProject(t, file, soaked, Baseline{})); !f.Available() {
		t.Errorf("a 30-day-old tag must qualify, got %+v", f)
	}
}

// The include regex is a structural guardrail, not a preference: a service
// constrained to one line must never be offered a candidate outside it.
func TestProject_IncludeConstrainsCandidates(t *testing.T) {
	file := project(t, `services:
  a:
    image: x/y:17.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4
    labels:
      duva.include_tags: '^17\.\d+$'
`)
	f := one(t, mustProject(t, file, reg([]string{"17.0", "18.0", "18.1"}, "", time.Now()), Baseline{}))
	if f.Available() {
		t.Errorf("18.x must not qualify for a 17.x constraint, got %+v", f)
	}
}

func TestProject_ExcludeDropsCandidates(t *testing.T) {
	file := project(t, `services:
  a:
    image: x/y:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4
    labels:
      duva.include_tags: '^\d+\.\d+\.\d+'
      duva.exclude_tags: 'rc'
`)
	f := one(t, mustProject(t, file, reg([]string{"1.0.0", "1.1.0-rc1"}, "", time.Now()), Baseline{}))
	if f.Available() {
		t.Errorf("an excluded candidate must not be offered, got %+v", f)
	}
}

// A moving tag reports a digest move once, then stays quiet until it moves
// again. The baseline only advances when the update is applied, so the
// finding survives restarts.
func TestProject_MovingTagLifecycle(t *testing.T) {
	file := project(t, "services:\n  a:\n    image: x/y:latest@sha256:3fab5c181bd28a09b64397df76ae2bfaf1eac182979b5fdb7a342858004f36af\n")
	base := Baseline{}

	// First sight records where the tag points and says nothing: the file
	// was never checked against this registry before, so there is nothing
	// yet to call a change.
	if f := one(t, mustProject(t, file, reg(nil, "sha256:9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0", time.Now()), base)); f.Available() {
		t.Fatalf("first check must be silent, got %+v", f)
	}
	if base["a"] != "sha256:9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0" {
		t.Fatalf("baseline = %q", base["a"])
	}

	// Unmoved from the file's own pin, the tag having never moved: still
	// available, since the file (3fab5c18...) and the registry
	// (9834876d...) disagree from the very first check onward -- reporting
	// it again is correct here, not a repeat to suppress. Whether an
	// operator has already been told is duva.State's Notified map's job, one
	// layer up, not baseline's.
	f := one(t, mustProject(t, file, reg(nil, "sha256:9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0", time.Now()), base))
	if !f.Available() {
		t.Errorf("the file's own pin disagreeing with the registry must be reported, got %+v", f)
	}

	// Moved further: still reported, as a digest with no bump to classify.
	f = one(t, mustProject(t, file, reg(nil, "sha256:3e744b9dc39389baf0c5a0660589b8402f3dbb49b89b3e75f2c9355852a3c677", time.Now()), base))
	if !f.Available() || f.Kind != KindDigest || f.Candidate != "sha256:3e744b9dc39389baf0c5a0660589b8402f3dbb49b89b3e75f2c9355852a3c677" {
		t.Fatalf("a moved tag must be reported, got %+v", f)
	}
	if f.Bump != "" {
		t.Errorf("Bump = %q, want empty: a moving tag has no version pair", f.Bump)
	}
	// The compose file holds the truth, and baseline is only ever what was
	// last seen from the registry -- it tracks detection, not application,
	// so it advances here too.
	if base["a"] != "sha256:3e744b9dc39389baf0c5a0660589b8402f3dbb49b89b3e75f2c9355852a3c677" {
		t.Errorf("baseline should track what was last seen, got %q", base["a"])
	}
}

// A pin that matches the registry is up to date regardless of what baseline
// last recorded -- covering a file edited by hand back to (or past) a digest
// duva has already seen, which must not go quiet just because baseline still
// remembers seeing it once.
func TestProject_FileMatchingRegistryIsUpToDateEvenIfBaselineDisagrees(t *testing.T) {
	const pinned = "sha256:3fab5c181bd28a09b64397df76ae2bfaf1eac182979b5fdb7a342858004f36af"
	const seenLater = "sha256:9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0"
	file := project(t, "services:\n  a:\n    image: x/y:latest@sha256:"+strings.TrimPrefix(pinned, "sha256:")+"\n")

	base := Baseline{"a": seenLater}
	f := one(t, mustProject(t, file, reg(nil, pinned, time.Now()), base))
	if f.Available() {
		t.Errorf("the file matching the registry must be up to date, got %+v", f)
	}
	if base["a"] != pinned {
		t.Errorf("baseline should catch up to what the file actually shows, got %q", base["a"])
	}
}

func TestProject_MovingTagRegistryFailure(t *testing.T) {
	file := project(t, "services:\n  a:\n    image: x/y:latest@sha256:3fab5c181bd28a09b64397df76ae2bfaf1eac182979b5fdb7a342858004f36af\n")
	r := Registry{RemoteDigest: func(string, string) (string, error) {
		return "", errors.New("no such host")
	}}
	f := one(t, mustProject(t, file, r, Baseline{"a": "sha256:cba06b5736faf67e54b07b561eae94395e774c517a7d910a54369e1263ccfbd4"}))
	if f.Status != StatusError {
		t.Fatalf("Status = %q, want error", f.Status)
	}
	if !strings.Contains(f.Reason, "no such host") {
		t.Errorf("Reason should carry the cause, got %q", f.Reason)
	}
}

// Findings carry the compose file the service is declared in, which for an
// include: layout is not the project root -- the actor needs the real one.
func TestProject_ResolvesIncludedFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "docker-compose.yml")
	sub := filepath.Join(dir, "media", "compose.yml")
	if err := os.MkdirAll(filepath.Dir(sub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("include:\n  - media/compose.yml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte("services:\n  a:\n    image: x/y:latest@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := one(t, mustProject(t, root, reg(nil, "sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4", time.Now()), Baseline{}))
	if f.File != sub {
		t.Errorf("File = %q, want the included file %q", f.File, sub)
	}
}

func TestProject_UnreadableProject(t *testing.T) {
	if _, err := Project(filepath.Join(t.TempDir(), "nope.yml"), Registry{}, Baseline{}); err == nil {
		t.Error("a missing project file should be an error")
	}
}

// Findings carry what the compose file pins, so neither the UI nor the actor
// has to re-read it.
func TestProject_FindingCarriesCurrentPin(t *testing.T) {
	file := project(t, "services:\n  a:\n    image: ghcr.io/x/y:1.2.3@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad\n")
	f := one(t, mustProject(t, file, reg(nil, "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", time.Now()), Baseline{"a": "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}))
	if f.Image != "ghcr.io/x/y" || f.CurrentTag != "1.2.3" || f.CurrentDigest != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("finding does not describe the current pin: %+v", f)
	}
}

// Decide is exercised through Project too, so a threshold in a label reaches
// the finding rather than only being unit-tested in isolation.
func TestProject_AppliesPolicyFromLabels(t *testing.T) {
	file := project(t, `services:
  a:
    image: x/y:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4
    labels:
      duva.include_tags: '^\d+\.\d+\.\d+$'
      duva.auto: patch
`)
	f := one(t, mustProject(t, file, reg([]string{"1.0.0", "1.0.1"}, "", time.Now()), Baseline{}))
	if !f.AutoApplies() {
		t.Errorf("a patch within duva.auto: patch should apply, got %+v (%s)", f.Decision, f.Why)
	}
	if f.Bump != version.KindPatch {
		t.Errorf("Bump = %q", f.Bump)
	}
}

// `image: nginx:${TAG}@sha256:...` has a digest, so it passes the pin check
// and looks watchable -- but duva reads the file as written, so the tag it
// would ask a registry about is the literal "${TAG}".
//
// Skipped rather than errored: the service is configured in a way this tool
// does not handle, which is not a fault to retry on every run. And the rest of
// the project must still be watched.
func TestProject_UnexpandedVariableIsSkipped(t *testing.T) {
	file := project(t, `services:
  templated:
    image: nginx:${TAG}@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4
  fine:
    image: x/z:1.0.0@sha256:18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4
`)

	asked := []string{}
	r := Registry{
		ListMatchingTags: func(image string, _, _ *regexp.Regexp, _ string) ([]string, error) {
			asked = append(asked, image)
			return []string{"1.0.1"}, nil
		},
		RemoteDigest: func(string, string) (string, error) {
			return "sha256:11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437", nil
		},
	}

	findings := mustProject(t, file, r, Baseline{})

	var templated *Finding
	for i := range findings {
		if findings[i].Service == "templated" {
			templated = &findings[i]
		}
	}
	if templated == nil {
		t.Fatal("the service should be reported, not dropped -- silence reads as nothing to do")
	}
	if templated.Status != StatusSkipped {
		t.Errorf("status = %v, want skipped", templated.Status)
	}
	if templated.Reason == "" {
		t.Error("a skip should say why")
	}

	// No registry should have been asked about a reference containing "${".
	for _, image := range asked {
		if strings.Contains(image, "${") {
			t.Errorf("asked a registry about %q", image)
		}
	}

	// And the well-formed service alongside it is still watched.
	var fine *Finding
	for i := range findings {
		if findings[i].Service == "fine" {
			fine = &findings[i]
		}
	}
	if fine == nil || fine.Status == StatusSkipped {
		t.Error("one templated service must not stop the rest of the project")
	}
}

// "@sha256:" is a text match, so `image: x/y:1.0.0@sha256:abc` looks pinned.
// It is not: the daemon rejects that reference outright, so the service cannot
// be pulled at all.
//
// Before this it was reported up-to-date and the malformed digest was recorded
// as the baseline -- duva saying a service was watched and healthy when it was
// neither, which is worse than the unpinned case that at least says so.
func TestProject_MalformedDigestIsAnError(t *testing.T) {
	file := project(t, "services:\n  bad:\n    image: x/y:1.0.0@sha256:abc\n")

	asked := false
	r := Registry{
		RemoteDigest: func(string, string) (string, error) {
			asked = true
			return "sha256:new", nil
		},
	}
	base := Baseline{}
	f := one(t, mustProject(t, file, r, base))

	if f.Status != StatusError {
		t.Errorf("status = %v, want error", f.Status)
	}
	if !strings.Contains(f.Reason, "sha256:abc") {
		t.Errorf("the reason should name the bad digest, got %q", f.Reason)
	}
	if asked {
		t.Error("no registry should be asked about a reference that cannot be pulled")
	}
	if len(base) != 0 {
		t.Errorf("a malformed digest must not be recorded as a baseline, got %v", base)
	}
}

// The opposite: a well-formed digest is watched exactly as before. A validity
// check that rejected everything would pass the test above.
func TestProject_ValidDigestIsStillWatched(t *testing.T) {
	const good = "sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"
	file := project(t, "services:\n  good:\n    image: x/y:latest@"+good+"\n")

	f := one(t, mustProject(t, file, reg(nil, good, time.Now()), Baseline{"good": good}))
	if f.Status == StatusError {
		t.Fatalf("a valid digest should be watched, got error: %s", f.Reason)
	}
	if f.CurrentDigest != good {
		t.Errorf("CurrentDigest = %q, want the pinned digest", f.CurrentDigest)
	}
}

// NeedsApproval and AutoApplies are how the rest of duva sorts a finding into
// "act on this" or "ask about it", and they are not opposites: a service with
// nothing newer is neither. Untested until now, which gobco noticed before
// statement coverage could -- both conditions ran, neither was ever false.
func TestFindingApprovalAndAutoAreNotOpposites(t *testing.T) {
	cases := []struct {
		name           string
		f              Finding
		approval, auto bool
	}{
		{"available and beyond policy",
			Finding{Status: StatusAvailable, Decision: DecideApprove}, true, false},
		{"available and within policy",
			Finding{Status: StatusAvailable, Decision: DecideAuto}, false, true},

		// A decision on a finding with nothing available is stale: it must not
		// make the service look actionable.
		{"nothing newer, decision left over",
			Finding{Status: StatusUpToDate, Decision: DecideApprove}, false, false},
		{"not watched",
			Finding{Status: StatusSkipped, Decision: DecideApprove}, false, false},
		{"the check failed",
			Finding{Status: StatusError, Decision: DecideAuto}, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.NeedsApproval(); got != c.approval {
				t.Errorf("NeedsApproval() = %v, want %v", got, c.approval)
			}
			if got := c.f.AutoApplies(); got != c.auto {
				t.Errorf("AutoApplies() = %v, want %v", got, c.auto)
			}
		})
	}
}
