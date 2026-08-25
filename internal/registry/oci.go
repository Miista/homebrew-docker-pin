package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const ociMaxTagChecks = 20

var bearerParamRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// manifestAccept lists the manifest media types we ask for when resolving a
// tag's digest, covering both OCI and legacy Docker schema 2 (incl. indexes).
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ",")

// splitRegistryRepo splits "host/path/to/repo" into the registry host and the
// repository path.
func splitRegistryRepo(image string) (host, repo string) {
	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", image
}

// resolveOCI resolves a version tag against any registry implementing the OCI
// Distribution Spec, discovering bearer auth via the WWW-Authenticate challenge.
func resolveOCI(image, digest string) (Result, error) {
	host, repo := splitRegistryRepo(image)
	if host == "" {
		return Result{}, fmt.Errorf("could not determine registry host from %q", image)
	}
	return resolveOCIFromBase("https://"+host, repo, digest)
}

func resolveOCIFromBase(baseURL, repo, digest string) (Result, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	tags, err := ociListTags(client, baseURL, repo)
	if err != nil {
		return Result{}, err
	}

	// Sort newest first by actual numeric version, not string specificity —
	// see the equivalent comment in ghcr.go.
	var versionTags []string
	for _, t := range tags {
		if isVersionTag(t) {
			versionTags = append(versionTags, t)
		}
	}
	sort.Slice(versionTags, func(i, j int) bool {
		return CompareVersions(versionTags[i], versionTags[j]) > 0
	})

	result := Result{VersionTagsSeen: len(versionTags)}
	for i, t := range versionTags {
		if i >= ociMaxTagChecks {
			break
		}
		d, err := ociManifestDigest(client, baseURL, repo, t)
		if err != nil {
			result.ChecksFailed++
			continue
		}
		if d == digest {
			result.Tag = t
			return result, nil
		}
	}
	return result, nil
}

// ociTagDigestFromBase fetches a single tag's manifest digest from any
// registry implementing the OCI Distribution Spec, with no pull and no tag
// listing.
func ociTagDigestFromBase(baseURL, repo, tag string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	return ociManifestDigest(client, baseURL, repo, tag)
}

// ociDo performs req, transparently handling a single bearer-auth challenge.
func ociDo(client *http.Client, req *http.Request, accept string) (*http.Response, error) {
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := doWithRetry(func() (*http.Response, error) { return client.Do(req) })
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	if !strings.HasPrefix(challenge, "Bearer ") {
		return nil, fmt.Errorf("registry requires unsupported auth scheme: %q", challenge)
	}

	params := map[string]string{}
	for _, m := range bearerParamRe.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return nil, fmt.Errorf("registry auth challenge missing realm")
	}

	token, err := ociFetchToken(client, realm, params["service"], params["scope"])
	if err != nil {
		return nil, err
	}

	req2, err := http.NewRequest(req.Method, req.URL.String(), nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req2.Header.Set("Accept", accept)
	}
	req2.Header.Set("Authorization", "Bearer "+token)
	return doWithRetry(func() (*http.Response, error) { return client.Do(req2) })
}

func ociFetchToken(client *http.Client, realm, service, scope string) (string, error) {
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("invalid auth realm %q: %w", realm, err)
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	resp, err := getWithRetry(client, u.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth token endpoint returned HTTP %d", resp.StatusCode)
	}
	var data struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.Token != "" {
		return data.Token, nil
	}
	if data.AccessToken != "" {
		return data.AccessToken, nil
	}
	return "", fmt.Errorf("auth token endpoint returned no token")
}

// ociListTags walks every page of the tags/list endpoint via the Link
// response header. See ghcrListTagsFromBase's comment: the OCI Distribution
// Spec mandates lexical tag ordering with no sort option, so there is no
// early-exit possible here — a full walk is the only correct option.
func ociListTags(client *http.Client, baseURL, repo string) ([]string, error) {
	var tags []string
	url := fmt.Sprintf("%s/v2/%s/tags/list", baseURL, repo)
	for url != "" {
		req, _ := http.NewRequest("GET", url, nil)
		resp, err := ociDo(client, req, "")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("tags list returned HTTP %d", resp.StatusCode)
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

// linkNextRe extracts the URL-reference and rel value from an RFC 5988 Link
// header, e.g. `</v2/x/tags/list?last=foo&n=100>; rel="next"`.
var linkNextRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="([^"]+)"`)

// nextPageURL returns the "next" relation's URL reference from a Link
// header, or "" if absent (meaning the current page is the last one — the
// OCI Distribution Spec gives no other way to know that in advance).
func nextPageURL(linkHeader string) string {
	for _, m := range linkNextRe.FindAllStringSubmatch(linkHeader, -1) {
		if m[2] == "next" {
			return m[1]
		}
	}
	return ""
}

func ociManifestDigest(client *http.Client, baseURL, repo, tag string) (string, error) {
	req, _ := http.NewRequest("HEAD", fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, tag), nil)
	resp, err := ociDo(client, req, manifestAccept)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("manifest response missing Docker-Content-Digest header")
	}
	return d, nil
}
