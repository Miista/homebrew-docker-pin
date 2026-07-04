package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ListTags returns every tag the registry publishes for an image, dispatching
// by registry the same way ResolveVersionTag does.
func ListTags(baseImage string) ([]string, error) {
	if strings.HasPrefix(baseImage, "ghcr.io/") {
		client := &http.Client{Timeout: 15 * time.Second}
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
	client := &http.Client{Timeout: 15 * time.Second}
	return ociListTags(client, "https://"+host, repo)
}

// hubMaxTagPages bounds Docker Hub pagination (100 tags per page).
const hubMaxTagPages = 10

func listDockerHubTags(url string) ([]string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	var tags []string
	for page := 0; url != "" && page < hubMaxTagPages; page++ {
		resp, err := client.Get(url)
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
			tags = append(tags, t.Name)
		}
		url = data.Next
	}
	return tags, nil
}

// versionCoreRe splits a tag into its numeric dotted core and the rest,
// tolerating a leading "v".
var versionCoreRe = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)(.*)$`)

// CompareVersions orders two version-ish tags: the dotted numeric cores are
// compared numerically segment by segment (missing segments count as 0), and
// on equal cores a tag with a suffix ranks BELOW the bare tag, so a build or
// prerelease like 2026.6.1-g8487590 sorts before 2026.6.1. Differing suffixes
// compare lexically. Tags without a numeric core compare lexically as a whole.
// Returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	ma, mb := versionCoreRe.FindStringSubmatch(a), versionCoreRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return strings.Compare(a, b)
	}
	sa, sb := strings.Split(ma[1], "."), strings.Split(mb[1], ".")
	for i := 0; i < len(sa) || i < len(sb); i++ {
		var na, nb int
		if i < len(sa) {
			na, _ = strconv.Atoi(sa[i])
		}
		if i < len(sb) {
			nb, _ = strconv.Atoi(sb[i])
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	switch {
	case ma[2] == mb[2]:
		return 0
	case ma[2] == "":
		return 1 // bare release beats any suffixed build of the same core
	case mb[2] == "":
		return -1
	default:
		return compareSuffixes(ma[2], mb[2])
	}
}

var suffixRunRe = regexp.MustCompile(`\d+|\D+`)

// compareSuffixes orders version suffixes like rpm/dpkg do: split into
// alternating numeric and non-numeric runs, comparing numeric runs
// numerically — so -ls100 > -ls99 and -r10 > -r2, where a plain lexical
// compare would invert them.
func compareSuffixes(a, b string) int {
	ra, rb := suffixRunRe.FindAllString(a, -1), suffixRunRe.FindAllString(b, -1)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		na, ea := strconv.Atoi(ra[i])
		nb, eb := strconv.Atoi(rb[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		default:
			if c := strings.Compare(ra[i], rb[i]); c != 0 {
				return c
			}
		}
	}
	// Equal prefix runs: the longer suffix ranks higher (more specific build).
	switch {
	case len(ra) == len(rb):
		return 0
	case len(ra) < len(rb):
		return -1
	default:
		return 1
	}
}

// NewestMatching returns the highest tag (per CompareVersions) that matches
// include and is strictly newer than current, or "" when no tag qualifies.
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
		if CompareVersions(t, current) <= 0 {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return CompareVersions(out[i], out[j]) > 0 })
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
	client := &http.Client{Timeout: 15 * time.Second}
	return ociTagCreated(client, "https://"+host, repo, tag)
}

func dockerHubTagCreated(baseImage, tag string) (time.Time, error) {
	namespace, repo := splitDockerHubImage(baseImage)
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/tags/%s", namespace, repo, tag)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
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
func ociTagCreated(client *http.Client, baseURL, repo, tag string) (time.Time, error) {
	manifest, err := ociGetJSON(client, fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, tag), manifestAccept)
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
		manifest, err = ociGetJSON(client, fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, sub), manifestAccept)
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
	blob, err := ociGetJSON(client, fmt.Sprintf("%s/v2/%s/blobs/%s", baseURL, repo, m.Config.Digest), "")
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
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
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
