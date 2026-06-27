package registry

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	pureVersionRe    = regexp.MustCompile(`^\d+(?:\.\d+)*$`)
	versionVariantRe = regexp.MustCompile(`^\d+(?:\.\d+)*-(.+)$`)
)

// registryEndpoint maps an image reference to the OCI Distribution endpoint and
// repository path used to query manifests.
func registryEndpoint(baseImage string) (baseURL, repo string) {
	if strings.HasPrefix(baseImage, "ghcr.io/") {
		return "https://ghcr.io", strings.TrimPrefix(baseImage, "ghcr.io/")
	}
	first := strings.SplitN(baseImage, "/", 2)[0]
	if !strings.Contains(first, ".") || strings.HasPrefix(baseImage, "docker.io/") {
		img := strings.TrimPrefix(baseImage, "docker.io/")
		if !strings.Contains(img, "/") {
			img = "library/" + img // official images live under library/
		}
		return "https://registry-1.docker.io", img
	}
	host, repo := splitRegistryRepo(baseImage)
	return "https://" + host, repo
}

// tagExists reports whether a tag has a manifest in the registry.
func tagExists(client *http.Client, baseURL, repo, tag string) (bool, error) {
	req, _ := http.NewRequest("HEAD", fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, tag), nil)
	resp, err := ociDo(client, req, manifestAccept)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected HTTP %d checking tag %q", resp.StatusCode, tag)
	}
}

// MovingPullTag determines which moving tag `docker upgrade` should pull to get
// the newest image of the SAME line/variant the service is currently on.
//
// It returns an error (the caller should bail) whenever the variant cannot be
// resolved unambiguously, so an upgrade never silently switches image flavor
// (e.g. alpine -> debian).
func MovingPullTag(baseImage, currentTag string) (string, error) {
	if currentTag == "latest" || pureVersionRe.MatchString(currentTag) {
		return "latest", nil
	}
	if m := versionVariantRe.FindStringSubmatch(currentTag); m != nil {
		variant := m[1]
		tag, ok, err := DiscoverMovingTag(baseImage, variant)
		if err != nil {
			return "", fmt.Errorf("could not query the registry for the %q variant's moving tag: %w", variant, err)
		}
		if !ok {
			return "", fmt.Errorf("could not unambiguously identify the moving tag for variant %q; "+
				"re-run with an explicit version, e.g. `docker upgrade <service> <version>`", variant)
		}
		return tag, nil
	}
	// Bare non-version tag (e.g. alpine, stable) — it is already a moving tag.
	return currentTag, nil
}

// DiscoverMovingTag finds the single moving tag in the registry that corresponds
// to a variant, checking the conventional candidate names. Returns (tag, true)
// only when exactly one candidate exists; ("", false) when zero or more than one
// match, so the caller bails rather than guessing.
func DiscoverMovingTag(baseImage, variant string) (string, bool, error) {
	baseURL, repo := registryEndpoint(baseImage)
	client := &http.Client{Timeout: 15 * time.Second}
	return discoverMovingTagAt(client, baseURL, repo, variant)
}

func discoverMovingTagAt(client *http.Client, baseURL, repo, variant string) (string, bool, error) {
	candidates := []string{variant, "latest-" + variant, variant + "-latest"}
	var found []string
	for _, c := range candidates {
		ok, err := tagExists(client, baseURL, repo, c)
		if err != nil {
			return "", false, err
		}
		if ok {
			found = append(found, c)
		}
	}
	if len(found) == 1 {
		return found[0], true, nil
	}
	return "", false, nil
}
