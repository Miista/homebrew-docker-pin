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

// The release's own label wins over the running container's, which is the
// previous release's and, for an image first labelled in this release, empty.
func TestPreferSource(t *testing.T) {
	const up, wrap = "https://github.com/cloudflare/cloudflared", "https://github.com/Miista/cloudflared-wrapper"
	for _, c := range []struct{ name, release, running, want string }{
		{"the release's", up, wrap, up},
		{"the container's when the release says nothing", "", wrap, wrap},
		{"the release's when the container says nothing", up, "", up},
		{"the container's when the release's is not a link", "javascript:alert(1)", wrap, wrap},
		{"neither", "", "", ""},
	} {
		if got := PreferSource(c.release, c.running); got != c.want {
			t.Errorf("%s: PreferSource(%q, %q) = %q, want %q", c.name, c.release, c.running, got, c.want)
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
