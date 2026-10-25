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
	const sonarr = "https://github.com/linuxserver/docker-sonarr"
	for _, c := range []struct {
		source, version string
		moved           bool
		release         string
		want            []string
	}{
		// A version: the tag as named, then with a v, then the release GitHub
		// marks Latest.
		{pl, "2.14.1", false, "", []string{pl + "/releases/tag/2.14.1", pl + "/releases/tag/v2.14.1", pl + "/releases/latest"}},
		// Named with a v: then without one.
		{pl, "v2.14.1", false, "", []string{pl + "/releases/tag/v2.14.1", pl + "/releases/tag/2.14.1", pl + "/releases/latest"}},
		// A digest move links the release its image says it is: Sonarr's
		// latest went to the releases list, topped by a develop build.
		{sonarr, "sha256:abc", true, "4.0.20.3014-ls1",
			[]string{sonarr + "/releases/tag/4.0.20.3014-ls1", sonarr + "/releases/tag/v4.0.20.3014-ls1", sonarr + "/releases/latest"}},
		// A digest move whose image does not say: the release marked Latest.
		{"https://github.com/Dictionarry-Hub/profilarr", "sha256:abc", true, "",
			[]string{"https://github.com/Dictionarry-Hub/profilarr/releases/latest"}},
		// Not GitHub: the repository.
		{"https://gitlab.com/group/app", "1.2.3", false, "", []string{"https://gitlab.com/group/app"}},
		{"", "1.2.3", false, "", nil},
	} {
		got := ReleaseNotes(c.source, c.version, c.moved, c.release)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("ReleaseNotes(%q, %q, %v, %q) = %q, want %q", c.source, c.version, c.moved, c.release, got, c.want)
		}
	}
}

// A version label is the image's own say and ends up in a URL: only a
// tag-like string is kept.
func TestReleaseOf(t *testing.T) {
	for label, want := range map[string]string{
		"4.0.20.3014-ls1": "4.0.20.3014-ls1", " v2.14.1 ": "v2.14.1", "1.0+build_7": "1.0+build_7",
		"": "", "1.0 beta": "", "../../x": "", "1.0?a=b": "", strings.Repeat("9", 129): "",
	} {
		if got := ReleaseOf(label); got != want {
			t.Errorf("ReleaseOf(%q) = %q, want %q", label, got, want)
		}
	}
}
