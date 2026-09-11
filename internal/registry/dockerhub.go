package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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
		// LastUpdated is when the tag was last pushed. Docker Hub returns it
		// with the listing, so a caller that wants dates for many tags gets
		// them for free rather than paying a request each -- which for a
		// repository with hundreds of tags is the difference between one
		// call and a minute of them.
		LastUpdated time.Time `json:"tag_last_pushed"`
	} `json:"results"`
	Next string `json:"next"`
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
