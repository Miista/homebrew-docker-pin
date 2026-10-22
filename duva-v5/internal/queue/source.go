package queue

import (
	"net/url"
	"strings"
)

// SourceOf is an image's org.opencontainers.image.source label as a link to
// its repository: an http(s) URL with a host, without a trailing ".git" or
// "/". Anything else is empty -- a label is the image's own say, and the page
// renders it as a link a person follows, so it is not passed through on
// trust.
func SourceOf(label string) string {
	u, err := url.Parse(strings.TrimSpace(label))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	return u.String()
}

// ReleaseNotes is where a person reads what a release changes, from the
// repository its image names, as the places to try in order: the last is
// always there to fall back to.
//
// On GitHub, the release itself, by its tag as the image names it and then
// with or without a leading v -- a repository tags 3.3.3 as v3.3.3 as often as
// not, and which it does cannot be told from the image -- then its newest
// releases. Not GitHub's release search: it matches the text of the notes,
// so a search for 2.14.1 finds every later release that mentions it. A
// digest move has no version, so only the newest releases. Elsewhere, the
// repository itself. None when the image names no source.
func ReleaseNotes(source, version string, moved bool) []string {
	if source == "" {
		return nil
	}
	u, err := url.Parse(source)
	if err != nil {
		return nil
	}
	if u.Host != "github.com" {
		return []string{source}
	}
	base := strings.TrimSuffix(source, "/")
	newest := base + "/releases"
	if moved || version == "" {
		return []string{newest}
	}
	tags := []string{version}
	if bare, ok := strings.CutPrefix(version, "v"); ok {
		tags = append(tags, bare)
	} else {
		tags = append(tags, "v"+version)
	}
	var out []string
	for _, t := range tags {
		out = append(out, base+"/releases/tag/"+url.PathEscape(t))
	}
	return append(out, newest)
}
