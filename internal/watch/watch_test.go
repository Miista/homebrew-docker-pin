package watch

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/registry"
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
			body:       "services:\n  a:\n    image: x/y:1.0.0@sha256:d\n    labels:\n      duva.auto: wrong\n",
			wantStatus: StatusError,
		},
		{
			name:       "bad include regex is an error",
			body:       "services:\n  a:\n    image: x/y:1.0.0@sha256:d\n    labels:\n      duva.include: '^('\n",
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
    image: x/y:1.0.0@sha256:d
    labels:
      duva.include: '^\d'
  fine:
    image: x/z:latest@sha256:d
`)
	calls := 0
	r := Registry{
		ListMatchingTags: func(string, *regexp.Regexp, *regexp.Regexp, string) ([]string, error) {
			calls++
			return nil, errors.New("registry unreachable")
		},
		RemoteDigest: func(string, string) (string, error) { return "sha256:new", nil },
	}
	findings := mustProject(t, file, r, Baseline{"fine": "sha256:old"})

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
    image: x/y:1.0.0@sha256:d
    labels:
      duva.include: '^\d+\.\d+\.\d+$'
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
    image: x/y:17.0@sha256:d
    labels:
      duva.include: '^17\.\d+$'
`)
	f := one(t, mustProject(t, file, reg([]string{"17.0", "18.0", "18.1"}, "", time.Now()), Baseline{}))
	if f.Available() {
		t.Errorf("18.x must not qualify for a 17.x constraint, got %+v", f)
	}
}

func TestProject_ExcludeDropsCandidates(t *testing.T) {
	file := project(t, `services:
  a:
    image: x/y:1.0.0@sha256:d
    labels:
      duva.include: '^\d+\.\d+\.\d+'
      duva.exclude: 'rc'
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
	file := project(t, "services:\n  a:\n    image: x/y:latest@sha256:pinned\n")
	base := Baseline{}

	// First sight records where the tag points and says nothing.
	if f := one(t, mustProject(t, file, reg(nil, "sha256:aaa", time.Now()), base)); f.Available() {
		t.Fatalf("first check must be silent, got %+v", f)
	}
	if base["a"] != "sha256:aaa" {
		t.Fatalf("baseline = %q", base["a"])
	}

	// Unmoved: still quiet.
	if f := one(t, mustProject(t, file, reg(nil, "sha256:aaa", time.Now()), base)); f.Available() {
		t.Errorf("an unmoved tag must stay quiet, got %+v", f)
	}

	// Moved: reported, as a digest with no bump to classify.
	f := one(t, mustProject(t, file, reg(nil, "sha256:bbb", time.Now()), base))
	if !f.Available() || f.Kind != KindDigest || f.Candidate != "sha256:bbb" {
		t.Fatalf("a moved tag must be reported, got %+v", f)
	}
	if f.Bump != "" {
		t.Errorf("Bump = %q, want empty: a moving tag has no version pair", f.Bump)
	}
	// The baseline must NOT advance on detection alone, or the row would
	// vanish on the next run without anything having been applied.
	if base["a"] != "sha256:aaa" {
		t.Errorf("baseline moved to %q on detection alone", base["a"])
	}
}

func TestProject_MovingTagRegistryFailure(t *testing.T) {
	file := project(t, "services:\n  a:\n    image: x/y:latest@sha256:pinned\n")
	r := Registry{RemoteDigest: func(string, string) (string, error) {
		return "", errors.New("no such host")
	}}
	f := one(t, mustProject(t, file, r, Baseline{"a": "sha256:old"}))
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
	if err := os.WriteFile(sub, []byte("services:\n  a:\n    image: x/y:latest@sha256:d\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := one(t, mustProject(t, root, reg(nil, "sha256:d", time.Now()), Baseline{}))
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
	file := project(t, "services:\n  a:\n    image: ghcr.io/x/y:1.2.3@sha256:abc\n")
	f := one(t, mustProject(t, file, reg(nil, "sha256:abc", time.Now()), Baseline{"a": "sha256:abc"}))
	if f.Image != "ghcr.io/x/y" || f.CurrentTag != "1.2.3" || f.CurrentDigest != "sha256:abc" {
		t.Errorf("finding does not describe the current pin: %+v", f)
	}
}

// Decide is exercised through Project too, so a threshold in a label reaches
// the finding rather than only being unit-tested in isolation.
func TestProject_AppliesPolicyFromLabels(t *testing.T) {
	file := project(t, `services:
  a:
    image: x/y:1.0.0@sha256:d
    labels:
      duva.include: '^\d+\.\d+\.\d+$'
      duva.auto: patch
`)
	f := one(t, mustProject(t, file, reg([]string{"1.0.0", "1.0.1"}, "", time.Now()), Baseline{}))
	if !f.AutoApplies() {
		t.Errorf("a patch within duva.auto: patch should apply, got %+v (%s)", f.Decision, f.Why)
	}
	if f.Bump != registry.KindPatch {
		t.Errorf("Bump = %q", f.Bump)
	}
}
