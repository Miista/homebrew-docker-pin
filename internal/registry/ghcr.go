package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const ghcrMaxTagChecks = 20

func resolveGHCR(image, digest string) (Result, error) {
	path := strings.TrimPrefix(image, "ghcr.io/")
	return resolveGHCRFromBase(path, digest, "https://ghcr.io")
}

func resolveGHCRFromBase(path, digest, baseURL string) (Result, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	token, err := ghcrTokenFromBase(client, path, baseURL)
	if err != nil {
		return Result{}, fmt.Errorf("ghcr auth: %w", err)
	}

	tags, err := ghcrListTagsFromBase(client, token, path, baseURL)
	if err != nil {
		return Result{}, fmt.Errorf("ghcr tags: %w", err)
	}

	// Filter and sort version tags newest first by actual numeric version,
	// not string specificity — a registry can have hundreds of version tags
	// (e.g. linuxserver images), and only the top ghcrMaxTagChecks are
	// checked, so the sort order determines whether the real match is ever
	// examined.
	var versionTags []string
	for _, tag := range tags {
		if isVersionTag(tag) {
			versionTags = append(versionTags, tag)
		}
	}
	sort.Slice(versionTags, func(i, j int) bool {
		return CompareVersions(versionTags[i], versionTags[j]) > 0
	})

	result := Result{VersionTagsSeen: len(versionTags)}
	for i, tag := range versionTags {
		if i >= ghcrMaxTagChecks {
			break
		}
		tagDigest, err := ghcrManifestDigestFromBase(client, token, path, tag, baseURL)
		if err != nil {
			result.ChecksFailed++
			continue
		}
		if tagDigest == digest {
			result.Tag = tag
			return result, nil
		}
	}
	return result, nil
}

// ghcrTagDigestFromBase fetches a single tag's manifest digest, with no pull
// and no tag listing — one auth round trip plus one HEAD request.
func ghcrTagDigestFromBase(path, tag, baseURL string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	token, err := ghcrTokenFromBase(client, path, baseURL)
	if err != nil {
		return "", fmt.Errorf("ghcr auth: %w", err)
	}
	return ghcrManifestDigestFromBase(client, token, path, tag, baseURL)
}

func ghcrToken(client *http.Client, path string) (string, error) {
	return ghcrTokenFromBase(client, path, "https://ghcr.io")
}

func ghcrTokenFromBase(client *http.Client, path, baseURL string) (string, error) {
	url := fmt.Sprintf("%s/token?scope=repository:%s:pull&service=ghcr.io", baseURL, path)
	resp, err := getWithRetry(client, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var data struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.Token == "" {
		return "", fmt.Errorf("empty token")
	}
	return data.Token, nil
}

func ghcrListTags(client *http.Client, token, path string) ([]string, error) {
	return ghcrListTagsFromBase(client, token, path, "https://ghcr.io")
}

// ghcrListTagsFromBase walks every page of the OCI Distribution tags/list
// endpoint via the Link: rel="next" response header. The endpoint has no
// sort/order parameter and MUST return tags in lexical (ASCIIbetical) order
// per the OCI Distribution Spec — not chronological, not numeric — so there
// is no early-exit optimization available the way Docker Hub's
// ordering=last_updated allows: seeding the "last" cursor at the current tag
// would silently skip any tag whose version component grows a digit (e.g.
// current "2.9.0" sorts AFTER "2.10.0" lexically, so a cursor at "2.9.0"
// never reaches "2.10.0"). A full walk is the only correct option; see
// README.md's "GHCR/OCI tag listing cost" section for the request-count
// implications this has for tag-heavy images.
func ghcrListTagsFromBase(client *http.Client, token, path, baseURL string) ([]string, error) {
	var tags []string
	url := fmt.Sprintf("%s/v2/%s/tags/list", baseURL, path)
	for url != "" {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := doWithRetry(func() (*http.Response, error) { return client.Do(req) })
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		var data struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		tags = append(tags, data.Tags...)

		next := nextPageURL(resp.Header.Get("Link"))
		if next == "" {
			break
		}
		url = baseURL + next
	}
	return tags, nil
}

func ghcrManifestDigest(client *http.Client, token, path, tag string) (string, error) {
	return ghcrManifestDigestFromBase(client, token, path, tag, "https://ghcr.io")
}

func ghcrManifestDigestFromBase(client *http.Client, token, path, tag, baseURL string) (string, error) {
	req, _ := http.NewRequest("HEAD",
		fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, path, tag), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ","))

	resp, err := doWithRetry(func() (*http.Response, error) { return client.Do(req) })
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("no digest header")
	}
	return d, nil
}
