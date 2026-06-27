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

	// Filter and sort version tags by specificity, most specific first.
	var versionTags []string
	for _, tag := range tags {
		if isVersionTag(tag) {
			versionTags = append(versionTags, tag)
		}
	}
	sort.Slice(versionTags, func(i, j int) bool {
		di, li := tagSpecificity(versionTags[i])
		dj, lj := tagSpecificity(versionTags[j])
		if di != dj {
			return di > dj
		}
		return li > lj
	})

	result := Result{VersionTagsSeen: len(versionTags)}
	for i, tag := range versionTags {
		if i >= ghcrMaxTagChecks {
			break
		}
		tagDigest, err := ghcrManifestDigestFromBase(client, token, path, tag, baseURL)
		if err != nil {
			continue
		}
		if tagDigest == digest {
			result.Tag = tag
			return result, nil
		}
	}
	return result, nil
}

func ghcrToken(client *http.Client, path string) (string, error) {
	return ghcrTokenFromBase(client, path, "https://ghcr.io")
}

func ghcrTokenFromBase(client *http.Client, path, baseURL string) (string, error) {
	url := fmt.Sprintf("%s/token?scope=repository:%s:pull&service=ghcr.io", baseURL, path)
	resp, err := client.Get(url)
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

func ghcrListTagsFromBase(client *http.Client, token, path, baseURL string) ([]string, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v2/%s/tags/list", baseURL, path), nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var data struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Tags, nil
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

	resp, err := client.Do(req)
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
