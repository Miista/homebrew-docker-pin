package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// githubAnswers stands in for GitHub: 200 for the paths it holds, 404 for
// everything else, and it records what was asked.
type githubAnswers struct {
	exists map[string]bool
	asked  []string
}

func (g *githubAnswers) RoundTrip(r *http.Request) (*http.Response, error) {
	g.asked = append(g.asked, r.Method+" "+r.URL.String())
	code := http.StatusNotFound
	if g.exists[r.URL.Path] {
		code = http.StatusOK
	}
	return &http.Response{StatusCode: code, Body: http.NoBody, Request: r}, nil
}

func withGitHub(t *testing.T, exists ...string) *githubAnswers {
	t.Helper()
	g := &githubAnswers{exists: map[string]bool{}}
	for _, p := range exists {
		g.exists[p] = true
	}
	old := notesClient.Transport
	notesClient.Transport = g
	t.Cleanup(func() { notesClient.Transport = old })
	return g
}

func notesFor(t *testing.T, e Hosted, path string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{Source: &fakeSource{pending: []Hosted{e}}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The release is found by whichever tag form the repository uses: paperless
// tags v2.14.1 for an image tagged 2.14.1, and a link straight to the image's
// tag is GitHub's 404.
func TestReleaseNotesFindTheTagTheRepositoryUses(t *testing.T) {
	g := withGitHub(t, "/paperless-ngx/paperless-ngx/releases/tag/v2.14.1")
	e := entry("paperless", "2.13.0", "2.14.1", version.KindMinor)
	e.ID, e.Source = 7, "https://github.com/paperless-ngx/paperless-ngx"

	if got := toRow(e).Notes; got != "/notes/optiplex/7" {
		t.Fatalf("row's link = %q", got)
	}
	rec := notesFor(t, e, "/notes/optiplex/7")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://github.com/paperless-ngx/paperless-ngx/releases/tag/v2.14.1" {
		t.Errorf("= %d %s, want the v2.14.1 release (asked %v)", rec.Code, rec.Header().Get("Location"), g.asked)
	}
}

// With no release by either form, the newest releases: always there.
func TestReleaseNotesFallBackToTheNewest(t *testing.T) {
	withGitHub(t)
	e := entry("app", "1.0", "1.1", version.KindMinor)
	e.ID, e.Source = 3, "https://github.com/example/app"
	rec := notesFor(t, e, "/notes/optiplex/3")
	if got := rec.Header().Get("Location"); got != "https://github.com/example/app/releases" {
		t.Errorf("Location = %q, want the newest releases", got)
	}
}

// No source, no link -- and no route answers for it either.
func TestNoSourceNoReleaseNotes(t *testing.T) {
	g := withGitHub(t)
	e := entry("app", "1.0", "1.1", version.KindMinor)
	e.ID = 3
	if got := toRow(e).Notes; got != "" {
		t.Errorf("row's link = %q, want none", got)
	}
	if rec := notesFor(t, e, "/notes/optiplex/3"); rec.Code != http.StatusNotFound {
		t.Errorf("= %d, want 404", rec.Code)
	}
	if rec := notesFor(t, e, "/notes/optiplex/999"); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown entry = %d, want 404", rec.Code)
	}
	if len(g.asked) != 0 {
		t.Errorf("asked GitHub %v for a row with no source", g.asked)
	}
}
