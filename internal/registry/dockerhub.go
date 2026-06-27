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

var versionRe = regexp.MustCompile(`^\d+(\.\d+)+([.\-][a-zA-Z0-9]+)*$`)

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

func resolveDockerHub(image, digest string) (string, error) {
	namespace, repo := splitDockerHubImage(image)
	url := fmt.Sprintf(
		"https://hub.docker.com/v2/repositories/%s/%s/tags?page_size=100&ordering=last_updated",
		namespace, repo,
	)
	return resolveDockerHubFromURL(digest, url)
}

func resolveDockerHubFromURL(digest, url string) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	var matches []string
	for url != "" {
		resp, err := client.Get(url)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("docker hub API: HTTP %d", resp.StatusCode)
		}

		var data hubTagsResponse
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return "", err
		}
		for _, tag := range data.Results {
			if tag.Digest == digest && isVersionTag(tag.Name) {
				matches = append(matches, tag.Name)
			}
		}
		if len(matches) > 0 {
			break // don't paginate further once we have candidates
		}
		url = data.Next
	}

	if len(matches) == 0 {
		return "", nil
	}
	sort.Slice(matches, func(i, j int) bool {
		di, li := tagSpecificity(matches[i])
		dj, lj := tagSpecificity(matches[j])
		if di != dj {
			return di > dj
		}
		return li > lj
	})
	return matches[0], nil
}

func splitDockerHubImage(image string) (namespace, repo string) {
	image = strings.TrimPrefix(image, "docker.io/")
	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 1 {
		return "library", parts[0]
	}
	return parts[0], parts[1]
}
