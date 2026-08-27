package pin

import (
	"errors"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// fakeReg serves a fixed tag list and publish dates, so selection can be
// tested without touching a registry.
func fakeReg(tags []string, ages map[string]time.Duration) Registry {
	return Registry{
		ListMatchingTags: func(_ string, include, exclude *regexp.Regexp, _ string) ([]string, error) {
			var out []string
			for _, t := range tags {
				if include.MatchString(t) && (exclude == nil || !exclude.MatchString(t)) {
					out = append(out, t)
				}
			}
			return out, nil
		},
		TagCreated: func(_, tag string) (time.Time, error) {
			return time.Now().Add(-ages[tag]), nil
		},
	}
}

func TestSelectCandidate_NewestMatching(t *testing.T) {
	f := writeCompose(t, "services:\n  db:\n    image: postgres:17.10-alpine@sha256:x\n")
	reg := fakeReg([]string{"17.10-alpine", "17.11-alpine", "17.12-alpine", "18.0-alpine"}, nil)

	c, err := SelectCandidate(f, "db", Rules{Include: `^17\.\d+-alpine$`}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tag != "17.12-alpine" {
		t.Errorf("Tag = %q, want 17.12-alpine", c.Tag)
	}
}

// The include regex is a structural guardrail: a service constrained to the
// 17.x line must never be offered 18, even though it is newer.
func TestSelectCandidate_NeverEscapesTheConstraint(t *testing.T) {
	f := writeCompose(t, "services:\n  db:\n    image: postgres:17.10-alpine@sha256:x\n")
	reg := fakeReg([]string{"18.0-alpine", "18.1-alpine"}, nil)

	c, err := SelectCandidate(f, "db", Rules{Include: `^17\.\d+-alpine$`}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tag != "" {
		t.Errorf("Tag = %q, want none — 18.x must not qualify", c.Tag)
	}
	if c.Hold == "" {
		t.Error("expected a hold reason")
	}
}

func TestSelectCandidate_Exclude(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	reg := fakeReg([]string{"1.0.0", "1.1.0", "1.2.0-rc1"}, nil)

	c, err := SelectCandidate(f, "app", Rules{Include: `^\d+\.\d+\.\d+`, Exclude: `rc`}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tag != "1.1.0" {
		t.Errorf("Tag = %q, want 1.1.0 (rc excluded)", c.Tag)
	}
}

// The soak walks past candidates that have not been published long enough and
// reports them, so a caller can say what it is waiting on.
func TestSelectCandidate_DelaySkipsTooFresh(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	reg := fakeReg([]string{"1.0.0", "1.1.0", "1.2.0"}, map[string]time.Duration{
		"1.2.0": 1 * time.Hour,       // too fresh
		"1.1.0": 30 * 24 * time.Hour, // soaked
	})

	c, err := SelectCandidate(f, "app", Rules{Include: `^\d`, Delay: "7d"}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tag != "1.1.0" {
		t.Errorf("Tag = %q, want 1.1.0", c.Tag)
	}
	if len(c.TooFresh) != 1 || c.TooFresh[0].Tag != "1.2.0" {
		t.Errorf("TooFresh = %+v, want just 1.2.0", c.TooFresh)
	}
}

func TestSelectCandidate_AllTooFresh(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	reg := fakeReg([]string{"1.0.0", "1.1.0"}, map[string]time.Duration{"1.1.0": time.Hour})

	c, err := SelectCandidate(f, "app", Rules{Include: `^\d`, Delay: "7d"}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tag != "" {
		t.Errorf("Tag = %q, want none", c.Tag)
	}
	if c.Hold == "" {
		t.Error("expected a hold explaining the soak")
	}
}

func TestSelectCandidate_BadRegex(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	if _, err := SelectCandidate(f, "app", Rules{Include: `^(`}, fakeReg(nil, nil)); err == nil {
		t.Error("expected an error for an invalid include regex")
	}
}

// --- error paths ---------------------------------------------------------

func TestSelectCandidate_BadExcludeRegex(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	_, err := SelectCandidate(f, "app", Rules{Include: `^\d`, Exclude: `^(`}, fakeReg(nil, nil))
	if err == nil {
		t.Error("an invalid exclude regex must be reported")
	}
}

func TestSelectCandidate_BadDelay(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	reg := fakeReg([]string{"1.0.0", "1.1.0"}, nil)
	if _, err := SelectCandidate(f, "app", Rules{Include: `^\d`, Delay: "soon"}, reg); err == nil {
		t.Error("an unparseable delay must be reported, not ignored")
	}
}

func TestSelectCandidate_UnknownService(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	if _, err := SelectCandidate(f, "nope", Rules{Include: `^\d`}, fakeReg(nil, nil)); err == nil {
		t.Error("an unknown service must be an error")
	}
}

func TestSelectCandidate_RegistryFailure(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	reg := Registry{ListMatchingTags: func(string, *regexp.Regexp, *regexp.Regexp, string) ([]string, error) {
		return nil, errors.New("registry unreachable")
	}}
	if _, err := SelectCandidate(f, "app", Rules{Include: `^\d`}, reg); err == nil {
		t.Error("a registry failure must surface, not read as 'nothing newer'")
	}
}

// Asking for a publish date can fail independently of listing tags.
func TestSelectCandidate_TagCreatedFailure(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	reg := Registry{
		ListMatchingTags: func(string, *regexp.Regexp, *regexp.Regexp, string) ([]string, error) {
			return []string{"1.0.0", "1.1.0"}, nil
		},
		TagCreated: func(string, string) (time.Time, error) {
			return time.Time{}, errors.New("manifest unavailable")
		},
	}
	if _, err := SelectCandidate(f, "app", Rules{Include: `^\d`, Delay: "7d"}, reg); err == nil {
		t.Error("a failed publish-date lookup must surface")
	}
}

// The soak walks at most MaxDelayChecks candidates: a service whose newest
// tags are all too fresh must not make one registry call per tag forever.
func TestSelectCandidate_DelayChecksAreBounded(t *testing.T) {
	f := writeCompose(t, "services:\n  app:\n    image: app:1.0.0@sha256:x\n")
	many := make([]string, 0, 50)
	for i := 50; i > 0; i-- {
		many = append(many, "1."+strconv.Itoa(i)+".0")
	}
	calls := 0
	reg := Registry{
		ListMatchingTags: func(string, *regexp.Regexp, *regexp.Regexp, string) ([]string, error) {
			return many, nil
		},
		TagCreated: func(string, string) (time.Time, error) {
			calls++
			return time.Now(), nil // always too fresh
		},
	}
	if _, err := SelectCandidate(f, "app", Rules{Include: `^\d`, Delay: "7d"}, reg); err != nil {
		t.Fatal(err)
	}
	if calls > MaxDelayChecks {
		t.Errorf("made %d publish-date calls, want at most %d", calls, MaxDelayChecks)
	}
}
