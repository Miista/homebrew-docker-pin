package pin

import (
	"regexp"
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
