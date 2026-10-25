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

// PreferSource is the source to record for a release: the one its own image
// names, else the one the running container does.
//
// The release's first. A container has only the labels of the image it was
// created from, which is the release before this one: it says nothing until
// something has been updated to an image that carries the label, and then is
// one release behind a publisher who changes it. Both are held to SourceOf,
// so the registry's word is no more trusted than the container's.
func PreferSource(release, running string) string {
	if s := SourceOf(release); s != "" {
		return s
	}
	return SourceOf(running)
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
func ReleaseNotes(source, version string, moved bool, release string) []string {
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
	// The release GitHub marks Latest, not the releases list: the list is
	// newest first, and a publisher that also releases develop builds puts
	// one of those on top.
	latest := base + "/releases/latest"
	// A digest move has no version of its own: the image's own word for
	// which release it is, when it says.
	if moved {
		version = release
	}
	if version == "" {
		return []string{latest}
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
	return append(out, latest)
}

// ReleaseOf is an image's version label as a release to link by: a tag-like
// string -- letters, digits, ".", "-", "_", "+" -- of a sensible length, else
// empty. A label is the image's own say, and ends up in a URL.
func ReleaseOf(label string) string {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 128 {
		return ""
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '+':
		default:
			return ""
		}
	}
	return label
}
