package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var versionRe = regexp.MustCompile(`^v?\d+(\.\d+)+([.\-][a-zA-Z0-9]+)*$`)

func isVersionTag(tag string) bool {
	return versionRe.MatchString(tag)
}

// tagSpecificity returns a sort key: more dots = more specific, then longer = more specific.
func tagSpecificity(tag string) (int, int) {
	return strings.Count(tag, "."), len(tag)
}

type hubTagsResponse struct {
	Results []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	} `json:"results"`
	Next string `json:"next"`
}

func resolveDockerHub(image, digest string) (Result, error) {
	namespace, repo := splitDockerHubImage(image)
	url := fmt.Sprintf(
		"https://hub.docker.com/v2/repositories/%s/%s/tags?page_size=100&ordering=last_updated",
		namespace, repo,
	)
	return resolveDockerHubFromURL(digest, url)
}

func resolveDockerHubFromURL(digest, url string) (Result, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	var matches []string
	versionTagsSeen := 0
	for page := 0; url != "" && page < hubMaxTagPages; page++ {
		resp, err := getWithRetry(client, url)
		if err != nil {
			return Result{}, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return Result{}, fmt.Errorf("docker hub API: HTTP %d", resp.StatusCode)
		}

		var data hubTagsResponse
		err = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if err != nil {
			return Result{}, err
		}
		for _, tag := range data.Results {
			if !isVersionTag(tag.Name) {
				continue
			}
			versionTagsSeen++
			if tag.Digest == digest {
				matches = append(matches, tag.Name)
			}
		}
		if len(matches) > 0 {
			break // don't paginate further once we have candidates
		}
		url = data.Next
	}

	result := Result{VersionTagsSeen: versionTagsSeen}
	if len(matches) == 0 {
		return result, nil
	}
	sort.Slice(matches, func(i, j int) bool {
		di, li := tagSpecificity(matches[i])
		dj, lj := tagSpecificity(matches[j])
		if di != dj {
			return di > dj
		}
		return li > lj
	})
	result.Tag = matches[0]
	return result, nil
}

// dockerHubTagDigestFromURL fetches a single tag's digest via Docker Hub's
// per-tag endpoint, with no pull and no listing/pagination.
func dockerHubTagDigestFromURL(url string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := getWithRetry(client, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("tag not found (HTTP 404 from %s)", url)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("docker hub API: HTTP %d", resp.StatusCode)
	}
	var data struct {
		Digest string `json:"digest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.Digest == "" {
		return "", fmt.Errorf("response from %s has no digest", url)
	}
	return data.Digest, nil
}

func splitDockerHubImage(image string) (namespace, repo string) {
	image = strings.TrimPrefix(image, "docker.io/")
	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 1 {
		return "library", parts[0]
	}
	return parts[0], parts[1]
}
