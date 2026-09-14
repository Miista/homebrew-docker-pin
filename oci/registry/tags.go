package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Miista/homebrew-docker-pin/oci/version"
)

// ListTags returns every tag the registry publishes for an image, dispatching
// by registry the same way ResolveVersionTag does.
func ListTags(baseImage string) ([]string, error) {
	if strings.HasPrefix(baseImage, "ghcr.io/") {
		client := httpClient()
		path := strings.TrimPrefix(baseImage, "ghcr.io/")
		token, err := ghcrToken(client, path)
		if err != nil {
			return nil, fmt.Errorf("ghcr auth: %w", err)
		}
		return ghcrListTags(client, token, path)
	}

	if isDockerHub(baseImage) {
		namespace, repo := splitDockerHubImage(baseImage)
		url := fmt.Sprintf(
			"https://hub.docker.com/v2/repositories/%s/%s/tags?page_size=100&ordering=last_updated",
			namespace, repo,
		)
		return listDockerHubTags(url)
	}

	host, repo := splitRegistryRepo(baseImage)
	if host == "" {
		return nil, fmt.Errorf("could not determine registry host from %q", baseImage)
	}
	client := httpClient()
	return ociListTags(client, "https://"+host, repo)
}

// ListMatchingTags is ListTags for a caller that will immediately narrow the
// result with MatchingCandidates(tags, include, exclude, current): on Docker
// Hub it stops paginating once a page contributes no further match, avoiding
// up to hubMaxTagPages-1 unnecessary requests for images with many tags.
// GHCR and generic OCI registries return their whole tag list in one
// unpaginated response, so there is nothing to cut short there — those paths
// are identical to ListTags.
func ListMatchingTags(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error) {
	if isDockerHub(baseImage) {
		namespace, repo := splitDockerHubImage(baseImage)
		url := fmt.Sprintf(
			"https://hub.docker.com/v2/repositories/%s/%s/tags?page_size=100&ordering=last_updated",
			namespace, repo,
		)
		return listDockerHubTagsMatching(url, include, exclude, current)
	}
	return ListTags(baseImage)
}

// hubMaxTagPages bounds Docker Hub pagination (100 tags per page).
const hubMaxTagPages = 10

func listDockerHubTags(url string) ([]string, error) {
	tags, err := listDockerHubTagsMatching(url, nil, nil, "")
	return tags, err
}

// listDockerHubTagsMatching paginates Docker Hub's tags API (newest-pushed
// first, per ordering=last_updated) and stops as soon as a page contains a
// tag that matches include/exclude and is newer than current per
// CompareVersions — once found, older (later) pages can only contain older
// pushes, so they're not fetched. Correctness relies on tags getting
// monotonically newer over time, which holds for the near-universal case of
// ordinary releases; it doesn't protect against a deliberate old-line
// backport pushed out of order, but neither does anything else in this
// package. include == nil disables the early exit (matches every tag), for
// callers that want the unfiltered list regardless of current.
func listDockerHubTagsMatching(url string, include, exclude *regexp.Regexp, current string) ([]string, error) {
	client := httpClient()
	var tags []string
	for page := 0; url != "" && page < hubMaxTagPages; page++ {
		resp, err := getWithRetry(client, url)
		if err != nil {
			return nil, err
		}
		var data hubTagsResponse
		err = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("docker hub API: HTTP %d", resp.StatusCode)
		}
		if err != nil {
			return nil, err
		}

		pageHasMatch := false
		for _, t := range data.Results {
			tags = append(tags, t.Name)
			if include != nil &&
				include.MatchString(t.Name) &&
				(exclude == nil || !exclude.MatchString(t.Name)) &&
				version.CompareVersions(t.Name, current) > 0 {
				pageHasMatch = true
			}
		}
		if include != nil && pageHasMatch {
			break
		}
		url = data.Next
	}
	return tags, nil
}

func NewestMatching(tags []string, include *regexp.Regexp, current string) string {
	if c := MatchingCandidates(tags, include, nil, current); len(c) > 0 {
		return c[0]
	}
	return ""
}

// MatchingCandidates returns every tag matching include (and not matching the
// optional exclude) that is strictly newer than current, sorted newest first.
func MatchingCandidates(tags []string, include, exclude *regexp.Regexp, current string) []string {
	var out []string
	for _, t := range tags {
		if !include.MatchString(t) {
			continue
		}
		if exclude != nil && exclude.MatchString(t) {
			continue
		}
		if version.CompareVersions(t, current) <= 0 {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return version.CompareVersions(out[i], out[j]) > 0 })
	return out
}

// TagCreated returns when a tag's image was published. Docker Hub reports the
// push time directly; for every other registry the image config blob's
// "created" timestamp (the build time) is used, which for CI-published images
// is effectively the release time.
func TagCreated(baseImage, tag string) (time.Time, error) {
	if isDockerHub(baseImage) {
		return dockerHubTagCreated(baseImage, tag)
	}
	host, repo := splitRegistryRepo(baseImage)
	if host == "" {
		return time.Time{}, fmt.Errorf("could not determine registry host from %q", baseImage)
	}
	client := httpClient()
	return ociTagCreated(client, "https://"+host, repo, tag)
}

func dockerHubTagCreated(baseImage, tag string) (time.Time, error) {
	namespace, repo := splitDockerHubImage(baseImage)
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/tags/%s", namespace, repo, tag)
	client := httpClient()
	resp, err := getWithRetry(client, url)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("docker hub tag API: HTTP %d", resp.StatusCode)
	}
	var data struct {
		TagLastPushed time.Time `json:"tag_last_pushed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return time.Time{}, err
	}
	if data.TagLastPushed.IsZero() {
		return time.Time{}, fmt.Errorf("docker hub reports no push time for %s:%s", baseImage, tag)
	}
	return data.TagLastPushed, nil
}

// ociTagCreated walks manifest (or index -> first sub-manifest) -> config
// blob -> "created" for any OCI Distribution registry, including GHCR.
//
// It authenticates once, up front, and carries that token through all three
// fetches. A pull scope is predictable, so waiting to be challenged on each
// one turned three requests into nine -- which is what made a tag-heavy
// service take a minute and a half to check, and made a scheduled run look
// like a flood to the resolver.
func ociTagCreated(client *http.Client, baseURL, repo, tag string) (time.Time, error) {
	token, _ := pullToken(client, baseURL, repo)

	manifest, err := ociGetJSONAuth(client, fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, tag), manifestAccept, token)
	if err != nil {
		return time.Time{}, err
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return time.Time{}, err
	}
	if m.Config.Digest == "" {
		// Multi-arch index: descend into the first real (non-attestation) manifest.
		sub := ""
		for _, mm := range m.Manifests {
			if mm.Platform.OS != "unknown" && mm.Digest != "" {
				sub = mm.Digest
				break
			}
		}
		if sub == "" {
			return time.Time{}, fmt.Errorf("manifest for %s has no config and no sub-manifests", tag)
		}
		manifest, err = ociGetJSONAuth(client, fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, sub), manifestAccept, token)
		if err != nil {
			return time.Time{}, err
		}
		if err := json.Unmarshal(manifest, &m); err != nil {
			return time.Time{}, err
		}
		if m.Config.Digest == "" {
			return time.Time{}, fmt.Errorf("sub-manifest for %s has no config", tag)
		}
	}
	blob, err := ociGetJSONAuth(client, fmt.Sprintf("%s/v2/%s/blobs/%s", baseURL, repo, m.Config.Digest), "", token)
	if err != nil {
		return time.Time{}, err
	}
	var cfg struct {
		Created time.Time `json:"created"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return time.Time{}, err
	}
	if cfg.Created.IsZero() {
		return time.Time{}, fmt.Errorf("image config for %s:%s carries no created timestamp", repo, tag)
	}
	return cfg.Created, nil
}

func ociGetJSON(client *http.Client, url, accept string) ([]byte, error) {
	return ociGetJSONAuth(client, url, accept, "")
}

// ociGetJSONAuth is ociGetJSON with a token the caller already holds.
//
// An empty token means "discover it", which is ociDo's challenge path and the
// correct behaviour for a registry this package has not been taught about. A
// non-empty one skips the 401 entirely.
func ociGetJSONAuth(client *http.Client, url, accept, token string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ociDo(client, req, accept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// DatedTag is a tag and when it was published.
type DatedTag struct {
	Name      string
	Published time.Time
}

// ListTagsWithDates returns every tag of an image, with its publish time
// where the registry gives one cheaply.
//
// Docker Hub returns the date with the listing, so the whole answer costs one
// request per page. Everywhere else it does not, and Published is left zero
// -- the caller then decides whether to pay for a date per tag or do without.
// That is deliberately the caller's choice: this package should not silently
// turn one call into two hundred.
func ListTagsWithDates(baseImage string) ([]DatedTag, error) {
	if isDockerHub(baseImage) {
		namespace, repo := splitDockerHubImage(baseImage)
		url := fmt.Sprintf(
			"https://hub.docker.com/v2/repositories/%s/%s/tags?page_size=100&ordering=last_updated",
			namespace, repo,
		)
		return listDockerHubDatedTags(url)
	}

	// No cheap dates here: return the names and let the caller fetch what it
	// needs.
	names, err := ListTags(baseImage)
	if err != nil {
		return nil, err
	}
	out := make([]DatedTag, 0, len(names))
	for _, n := range names {
		out = append(out, DatedTag{Name: n})
	}
	return out, nil
}

func listDockerHubDatedTags(url string) ([]DatedTag, error) {
	client := httpClient()
	var tags []DatedTag
	for page := 0; url != "" && page < hubMaxTagPages; page++ {
		resp, err := getWithRetry(client, url)
		if err != nil {
			return nil, err
		}
		var data hubTagsResponse
		err = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("docker hub API: HTTP %d", resp.StatusCode)
		}
		if err != nil {
			return nil, err
		}
		for _, t := range data.Results {
			tags = append(tags, DatedTag{Name: t.Name, Published: t.LastUpdated})
		}
		url = data.Next
	}
	return tags, nil
}
