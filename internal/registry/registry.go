package registry

import "strings"

// ResolveVersionTag attempts to find the most specific version tag for an image
// whose manifest digest matches the given digest.
//
// Supports Docker Hub and GHCR. Returns ("", nil) for unknown registries or when
// no matching version tag is found — the caller should fall back to the pulled tag.
func ResolveVersionTag(baseImage, digest string) (string, error) {
	if strings.HasPrefix(baseImage, "ghcr.io/") {
		return resolveGHCR(baseImage, digest)
	}

	// Docker Hub: no dots in the first path component (or explicit docker.io/ prefix)
	first := strings.SplitN(baseImage, "/", 2)[0]
	if !strings.Contains(first, ".") || strings.HasPrefix(baseImage, "docker.io/") {
		return resolveDockerHub(baseImage, digest)
	}

	return "", nil // unknown registry
}
