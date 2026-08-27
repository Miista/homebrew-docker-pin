package registry

import (
	"fmt"
	"strings"
)

// isDockerHub reports whether an image reference resolves to Docker Hub.
// Docker's rule: the first path segment names a registry host only when it
// contains a dot or a port colon, or is exactly "localhost" — otherwise it is
// a Hub namespace.
func isDockerHub(baseImage string) bool {
	if strings.HasPrefix(baseImage, "docker.io/") {
		return true
	}
	first := strings.SplitN(baseImage, "/", 2)[0]
	return !strings.ContainsAny(first, ".:") && first != "localhost"
}

// registryKind returns a human-readable name for the registry an image lives in,
// used to make resolution feedback explicit about what we're querying.
func registryKind(baseImage string) string {
	if strings.HasPrefix(baseImage, "ghcr.io/") {
		return "GitHub Container Registry"
	}
	if isDockerHub(baseImage) {
		return "Docker Hub"
	}
	return fmt.Sprintf("the OCI registry API at %s", strings.SplitN(baseImage, "/", 2)[0])
}

// RemoteDigest fetches the manifest digest for baseImage:tag directly from
// the registry, with no image pull and no local Docker daemon involved.
// Supports Docker Hub, GHCR, and any registry implementing the OCI
// Distribution Spec.
func RemoteDigest(baseImage, tag string) (string, error) {
	if strings.HasPrefix(baseImage, "ghcr.io/") {
		path := strings.TrimPrefix(baseImage, "ghcr.io/")
		return ghcrTagDigestFromBase(path, tag, "https://ghcr.io")
	}
	if isDockerHub(baseImage) {
		namespace, repo := splitDockerHubImage(baseImage)
		url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/tags/%s", namespace, repo, tag)
		return dockerHubTagDigestFromURL(url)
	}
	host, repo := splitRegistryRepo(baseImage)
	if host == "" {
		return "", fmt.Errorf("could not determine registry host from %q", baseImage)
	}
	return ociTagDigestFromBase("https://"+host, repo, tag)
}
