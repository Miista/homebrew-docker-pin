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

// Docker Hub answers on two hosts: hub.docker.com serves the tag summary that
// most lookups need, and registry-1.docker.io is the OCI registry behind it.
// Variables rather than constants so a test can point them at an httptest
// server -- the interesting behaviour is which one is asked and when, which
// cannot be tested against the real thing.
var (
	hubSummaryBase  = "https://hub.docker.com"
	hubRegistryBase = "https://registry-1.docker.io"
)

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
		url := fmt.Sprintf("%s/v2/repositories/%s/%s/tags/%s", hubSummaryBase, namespace, repo, tag)
		digest, err := dockerHubTagDigestFromURL(url)
		if err == nil {
			return digest, nil
		}
		// Hub's tag summary omits the top-level digest for tags pushed before
		// it started recording one -- willfarrell/autoheal:1.2.0, pushed in
		// 2021, has per-architecture digests under images[] and nothing above
		// them, while the same repository's tags pushed this year have both.
		//
		// The digest is not missing, only unreported: the registry itself
		// serves it. So ask the registry. Not images[], which holds
		// per-architecture digests -- pinning one of those would pin a
		// multi-arch image to a single architecture, which is a worse answer
		// than no answer.
		//
		// Hub's summary endpoint stays the first choice: one request, no
		// bearer-token round trip, and it answers for everything pushed since
		// whenever that changed.
		return ociTagDigestFromBase(hubRegistryBase, namespace+"/"+repo, tag)
	}
	host, repo := splitRegistryRepo(baseImage)
	if host == "" {
		return "", fmt.Errorf("could not determine registry host from %q", baseImage)
	}
	return ociTagDigestFromBase("https://"+host, repo, tag)
}
