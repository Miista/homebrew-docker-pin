package queue

import (
	"strings"
	"testing"
)

func TestSourceOf(t *testing.T) {
	for label, want := range map[string]string{
		"https://github.com/Dictionarry-Hub/profilarr":    "https://github.com/Dictionarry-Hub/profilarr",
		"https://github.com/paperless-ngx/paperless-ngx/": "https://github.com/paperless-ngx/paperless-ngx",
		"https://github.com/seerr-team/seerr.git":         "https://github.com/seerr-team/seerr",
		" https://gitlab.com/group/app ":                  "https://gitlab.com/group/app",
		"":                                                "",
		"git@github.com:org/app.git":                      "",
		"javascript:alert(1)":                             "",
		"https://user:pass@github.com/org/app":            "",
		"ftp://example.com/app":                           "",
		"https://github.com/org/app?ref=main#readme":      "https://github.com/org/app",
	} {
		if got := SourceOf(label); got != want {
			t.Errorf("SourceOf(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestReleaseNotes(t *testing.T) {
	const pl = "https://github.com/paperless-ngx/paperless-ngx"
	for _, c := range []struct {
		source, version string
		moved           bool
		want            []string
	}{
		// A version: the tag as named, then with a v, then the newest.
		{pl, "2.14.1", false, []string{pl + "/releases/tag/2.14.1", pl + "/releases/tag/v2.14.1", pl + "/releases"}},
		// Named with a v: then without one.
		{pl, "v2.14.1", false, []string{pl + "/releases/tag/v2.14.1", pl + "/releases/tag/2.14.1", pl + "/releases"}},
		// A digest move has no version: the newest releases.
		{"https://github.com/Dictionarry-Hub/profilarr", "sha256:abc", true,
			[]string{"https://github.com/Dictionarry-Hub/profilarr/releases"}},
		// Not GitHub: the repository.
		{"https://gitlab.com/group/app", "1.2.3", false, []string{"https://gitlab.com/group/app"}},
		{"", "1.2.3", false, nil},
	} {
		got := ReleaseNotes(c.source, c.version, c.moved)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("ReleaseNotes(%q, %q, %v) = %q, want %q", c.source, c.version, c.moved, got, c.want)
		}
	}
}
