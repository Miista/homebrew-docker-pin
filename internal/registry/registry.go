package registry

import (
	"fmt"
	"os"
	"strings"
)

// Result describes the outcome of a version-tag resolution attempt.
type Result struct {
	Tag             string // matched version tag; "" if none matched
	VersionTagsSeen int    // number of version tags present in the registry
}

// ResolveVersionTag attempts to find the most specific version tag for an image
// whose manifest digest matches the given digest.
//
// Supports Docker Hub and GHCR. Returns a zero Result for unknown registries.
// The caller should fall back to the pulled tag when Result.Tag is empty, and
// can use Result.VersionTagsSeen to distinguish "no version tags exist" from
// "version tags exist but none match the local image".
func ResolveVersionTag(baseImage, digest string) (Result, error) {
	if strings.HasPrefix(baseImage, "ghcr.io/") {
		return resolveGHCR(baseImage, digest)
	}

	if isDockerHub(baseImage) {
		return resolveDockerHub(baseImage, digest)
	}

	// Any other registry: attempt a generic OCI Distribution lookup.
	return resolveOCI(baseImage, digest)
}

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

// ResolveOrWarn resolves the version tag for an image pulled by pullTag, printing
// progress and explaining clearly when resolution fails. It returns the tag to
// pin with — the resolved version tag on success, otherwise pullTag unchanged.
//
// service is the compose service name, used only to suggest a follow-up command.
func ResolveOrWarn(baseImage, pullTag, digest, service string) string {
	fmt.Printf("Resolving version tag for %s:%s via %s ...\n", baseImage, pullTag, registryKind(baseImage))
	res, err := ResolveVersionTag(baseImage, digest)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "Warning: could not resolve version tag via %s (%v).\n", registryKind(baseImage), err)
		fmt.Fprintf(os.Stderr, "         Pinning as %s with the current digest.\n", pullTag)
	case res.Tag != "":
		return res.Tag
	case res.VersionTagsSeen == 0:
		fmt.Fprintf(os.Stderr, "Warning: the registry publishes no version tags for this image.\n")
		fmt.Fprintf(os.Stderr, "         Pinning as %s with the current digest.\n", pullTag)
	default:
		// Version tags exist, but none match the local image — orphaned/stale build.
		fmt.Fprintf(os.Stderr, "Warning: your local image matches none of the %d version tag(s) in the registry.\n", res.VersionTagsSeen)
		fmt.Fprintf(os.Stderr, "         A newer build has likely replaced the %s tag you pulled earlier.\n", pullTag)
		fmt.Fprintf(os.Stderr, "         Pinning the running image as %s; run `docker upgrade %s` to move to the current tagged build.\n", pullTag, service)
	}
	return pullTag
}
