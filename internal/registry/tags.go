package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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

	first := strings.SplitN(baseImage, "/", 2)[0]
	if !strings.Contains(first, ".") || strings.HasPrefix(baseImage, "docker.io/") {
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
		return strings.Compare(ma[2], mb[2])
	}
}

// NewestMatching returns the highest tag (per CompareVersions) that matches
// include and is strictly newer than current, or "" when no tag qualifies.
func NewestMatching(tags []string, include *regexp.Regexp, current string) string {
	best := ""
	for _, t := range tags {
		if !include.MatchString(t) {
			continue
		}
		if CompareVersions(t, current) <= 0 {
			continue
		}
		if best == "" || CompareVersions(t, best) > 0 {
			best = t
		}
	}
	return best
}
