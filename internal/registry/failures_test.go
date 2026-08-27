package registry

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These cover what a real registry will not produce on demand: being down,
// throttling, returning nonsense, or serving an index full of attestations.
// A fake cannot prove GHCR still speaks the protocol we think it does -- the
// e2e's real fixture is there for that -- but it is the only way to exercise
// the paths that only appear when things go wrong.

// --- registry down / refusing ------------------------------------------

func TestGHCRToken_RegistryDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := ghcrTokenFromBase(srv.Client(), "foo/bar", srv.URL); err == nil {
		t.Error("a 503 from the token endpoint must surface as an error")
	}
}

func TestGHCRListTags_AuthRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := ghcrListTagsFromBase(srv.Client(), "token", "foo/bar", srv.URL); err == nil {
		t.Error("a rejected token must surface as an error, not an empty tag list")
	}
}

// An empty tag list is a legitimate answer (a repository with no tags), not a
// failure -- callers decide what to do with "nothing matches".
func TestGHCRListTags_EmptyIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tags":[]}`)
	}))
	defer srv.Close()

	tags, err := ghcrListTagsFromBase(srv.Client(), "token", "foo/bar", srv.URL)
	if err != nil {
		t.Fatalf("an empty repository is not an error: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v, want none", tags)
	}
}

func TestGHCRListTags_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tags": [ truncated`)
	}))
	defer srv.Close()

	if _, err := ghcrListTagsFromBase(srv.Client(), "token", "foo/bar", srv.URL); err == nil {
		t.Error("malformed JSON must surface as an error rather than an empty result")
	}
}

func TestOCIListTags_RegistryDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	if _, err := ociListTags(srv.Client(), srv.URL, "foo/bar"); err == nil {
		t.Error("a 502 must surface as an error")
	}
}

func TestOCIManifestDigest_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := ociManifestDigest(srv.Client(), srv.URL, "foo/bar", "nope"); err == nil {
		t.Error("a missing tag must surface as an error")
	}
}

// --- throttling ---------------------------------------------------------

// Registries rate-limit, and this tool can ask about many tags in one run.
// A 429 that eventually succeeds must be retried, not treated as a verdict.
func TestOCI_RetriesAfterThrottling(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"tags":["1.0.0","1.1.0"]}`)
	}))
	defer srv.Close()

	tags, err := ociListTags(srv.Client(), srv.URL, "foo/bar")
	if err != nil {
		t.Fatalf("a throttled request that later succeeds must not fail: %v", err)
	}
	if calls < 2 {
		t.Errorf("expected a retry, got %d call(s)", calls)
	}
	if len(tags) != 2 {
		t.Errorf("tags = %v", tags)
	}
}

// --- multi-arch indexes -------------------------------------------------

// A publish date comes from the image config, which for a multi-arch tag
// means descending through the index into a real sub-manifest. Attestation
// manifests carry no config worth reading and must be skipped -- they sort
// first in some registries, so taking the first entry is wrong.
func TestOCITagCreated_DescendsMultiArchIndexSkippingAttestations(t *testing.T) {
	const (
		attestation = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		realImage   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		configBlob  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifests/1.0.0"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			fmt.Fprintf(w, `{"manifests":[
			  {"digest":%q,"platform":{"architecture":"unknown","os":"unknown"},
			   "annotations":{"vnd.docker.reference.type":"attestation-manifest"}},
			  {"digest":%q,"platform":{"architecture":"amd64","os":"linux"}}
			]}`, attestation, realImage)
		case strings.HasSuffix(r.URL.Path, "/manifests/"+realImage):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			fmt.Fprintf(w, `{"config":{"digest":%q}}`, configBlob)
		case strings.HasSuffix(r.URL.Path, "/manifests/"+attestation):
			t.Error("descended into the attestation manifest")
			http.Error(w, "should not be fetched", http.StatusTeapot)
		case strings.HasSuffix(r.URL.Path, "/blobs/"+configBlob):
			fmt.Fprint(w, `{"created":"2026-01-15T10:00:00Z"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got, err := ociTagCreated(srv.Client(), srv.URL, "foo/bar", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Year() != 2026 || got.Month() != 1 || got.Day() != 15 {
		t.Errorf("created = %s, want 2026-01-15", got)
	}
}

// An index with nothing but attestations has no date to report; saying so
// beats returning a zero time that reads as 1 January year 1.
func TestOCITagCreated_IndexWithOnlyAttestations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		fmt.Fprint(w, `{"manifests":[
		  {"digest":"sha256:aaaa","platform":{"architecture":"unknown","os":"unknown"},
		   "annotations":{"vnd.docker.reference.type":"attestation-manifest"}}
		]}`)
	}))
	defer srv.Close()

	if _, err := ociTagCreated(srv.Client(), srv.URL, "foo/bar", "1.0.0"); err == nil {
		t.Error("an index with no real image should be an error, not a zero time")
	}
}

// --- pagination ---------------------------------------------------------

// A registry with hundreds of tags pages them behind Link headers. Stopping
// at the first page silently truncates the candidate list, which looks like
// "no newer version" rather than a bug.
//
// RFC 5988 allows the target to be absolute or relative, and registries do
// both, so both are tested: joining an absolute one onto the base URL used to
// produce a nonsense host and make the service look unreachable.
func TestOCIListTags_FollowsRelativePagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last") == "1.1.0" {
			fmt.Fprint(w, `{"tags":["1.2.0"]}`)
			return
		}
		w.Header().Set("Link", `</v2/foo/bar/tags/list?last=1.1.0>; rel="next"`)
		fmt.Fprint(w, `{"tags":["1.0.0","1.1.0"]}`)
	}))
	defer srv.Close()

	tags, err := ociListTags(srv.Client(), srv.URL, "foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 {
		t.Errorf("tags = %v, want all three pages' worth", tags)
	}
}

func TestOCIListTags_FollowsAbsolutePagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last") == "1.1.0" {
			fmt.Fprint(w, `{"tags":["1.2.0"]}`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/v2/foo/bar/tags/list?last=1.1.0>; rel="next"`, srv.URL))
		fmt.Fprint(w, `{"tags":["1.0.0","1.1.0"]}`)
	}))
	defer srv.Close()

	tags, err := ociListTags(srv.Client(), srv.URL, "foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 3 {
		t.Errorf("tags = %v, want all three pages' worth", tags)
	}
}
