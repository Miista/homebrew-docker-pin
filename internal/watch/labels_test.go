package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func composeWith(t *testing.T, labels string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "docker-compose.yml")
	body := "services:\n  app:\n    image: example.com/app:1.0.0@sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881\n"
	if labels != "" {
		body += "    labels:\n" + labels
	}
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLabels_ReadsRules(t *testing.T) {
	f := composeWith(t, "      duva.include: '^\\d+$'\n      duva.exclude: 'rc'\n      duva.delay: 7d\n      duva.auto: minor\n")
	r, err := Labels(f, "app")
	if err != nil {
		t.Fatal(err)
	}
	if r.Include != `^\d+$` || r.Exclude != "rc" || r.Delay != "7d" || r.Auto != AutoMinor {
		t.Errorf("got %+v", r)
	}
}

func TestLabels_NoLabelsIsFine(t *testing.T) {
	r, err := Labels(composeWith(t, ""), "app")
	if err != nil {
		t.Fatal(err)
	}
	if r.Auto != AutoNone {
		t.Errorf("Auto = %q, want none by default", r.Auto)
	}
}

// duva validated nothing before this: a misspelled label was silently
// ignored, which looks exactly like duva working.
func TestLabels_RejectsUnknownDuvaLabel(t *testing.T) {
	_, err := Labels(composeWith(t, "      duva.includ: '^1$'\n"), "app")
	if err == nil {
		t.Fatal("expected an error for a misspelled duva label")
	}
	if !strings.Contains(err.Error(), "duva.includ") {
		t.Errorf("error should name the offending label, got: %v", err)
	}
}

// Only duva.* is duva's business; other tools label the same services.
func TestLabels_IgnoresForeignLabels(t *testing.T) {
	f := composeWith(t, "      com.example.thing: 'x'\n      traefik.enable: 'true'\n      duva.auto: patch\n")
	r, err := Labels(f, "app")
	if err != nil {
		t.Fatalf("foreign labels must not be rejected: %v", err)
	}
	if r.Auto != AutoPatch {
		t.Errorf("Auto = %q", r.Auto)
	}
}

func TestLabels_RejectsBadAuto(t *testing.T) {
	if _, err := Labels(composeWith(t, "      duva.auto: sometimes\n"), "app"); err == nil {
		t.Error("expected an error for an unknown duva.auto value")
	}
}

// A delay bounds which candidates qualify, and an exclude filters them: both
// are meaningless with no candidate set to choose from.
func TestLabels_DelayAndExcludeNeedInclude(t *testing.T) {
	if _, err := Labels(composeWith(t, "      duva.delay: 7d\n"), "app"); err == nil {
		t.Error("duva.delay without duva.include should be rejected")
	}
	if _, err := Labels(composeWith(t, "      duva.exclude: rc\n"), "app"); err == nil {
		t.Error("duva.exclude without duva.include should be rejected")
	}
}

// duva.auto IS meaningful without include: it is how a moving-tag service
// opts out of having its digest moves applied unattended.
func TestLabels_AutoWithoutIncludeIsValid(t *testing.T) {
	r, err := Labels(composeWith(t, "      duva.auto: none\n"), "app")
	if err != nil {
		t.Fatalf("duva.auto alone is how a followed tag opts out: %v", err)
	}
	if r.Auto != AutoNone {
		t.Errorf("Auto = %q", r.Auto)
	}
}
