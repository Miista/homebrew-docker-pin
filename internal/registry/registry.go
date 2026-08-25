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
	// ChecksFailed counts candidate tags whose manifest digest could not be
	// fetched (network error, throttling, timeout) rather than genuinely not
	// matching. A non-zero count means "no match" is not a confident result
	// — some candidates were never actually checked.
	ChecksFailed int
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

// ResolveOrWarn resolves the version tag for an image pulled by pullTag, printing
// progress and explaining clearly when resolution fails. It returns the tag to
// pin with — the resolved version tag on success, otherwise pullTag unchanged.
//
// service is the compose service name, used only to suggest a follow-up command.
func ResolveOrWarn(baseImage, pullTag, digest, service string) string {
	fmt.Printf("Resolving version tag for %s:%s via %s ...\n", baseImage, pullTag, registryKind(baseImage))
	tag, warning := ResolveTag(baseImage, pullTag, digest, service)
	if warning != "" {
		fmt.Fprint(os.Stderr, warning)
	}
	return tag
}

// ResolveTag is ResolveOrWarn without printing anything: it returns the tag
// to pin with, and separately any warning text that ResolveOrWarn would have
// printed to stderr (empty if resolution succeeded cleanly). For use when
// many services are being resolved concurrently, so callers can buffer
// warnings and print them after their progress counter finishes instead of
// interleaving mid-line.
func ResolveTag(baseImage, pullTag, digest, service string) (tag, warning string) {
	res, err := ResolveVersionTag(baseImage, digest)
	// Each warning is built with a single Sprintf call (not one per line) so
	// concurrent callers resolving different services don't interleave their
	// warnings mid-message if printed immediately; every line is also
	// prefixed with the service name so a warning is attributable even when
	// interleaved with another stream's output (e.g. a progress counter).
	switch {
	case err != nil:
		warning = fmt.Sprintf("%[1]s: Warning: could not resolve version tag via %[2]s (%[3]v).\n%[1]s:          Pinning as %[4]s with the current digest.\n",
			service, registryKind(baseImage), err, pullTag)
	case res.Tag != "":
		return res.Tag, ""
	case res.VersionTagsSeen == 0:
		warning = fmt.Sprintf("%[1]s: Warning: the registry publishes no version tags for this image.\n%[1]s:          Pinning as %[2]s with the current digest.\n",
			service, pullTag)
	default:
		// Version tags exist, but none match the local image — orphaned/stale build,
		// unless some manifest checks failed (throttling, timeouts), in which case
		// "no match" isn't a confident result: those candidates were never checked.
		if res.ChecksFailed > 0 {
			warning = fmt.Sprintf("%[1]s: Warning: could not verify %[2]d of %[3]d version tag(s) in the registry (throttled or timed out) and found no match among the rest.\n%[1]s:          This may be a false negative — retry, or lower --concurrency if this happens often.\n%[1]s:          Pinning the running image as %[4]s; run `docker upgrade %[1]s` to move to the current tagged build.\n",
				service, res.ChecksFailed, res.VersionTagsSeen, pullTag)
		} else {
			warning = fmt.Sprintf("%[1]s: Warning: your local image matches none of the %[2]d version tag(s) in the registry.\n%[1]s:          A newer build has likely replaced the %[3]s tag you pulled earlier.\n%[1]s:          Pinning the running image as %[3]s; run `docker upgrade %[1]s` to move to the current tagged build.\n",
				service, res.VersionTagsSeen, pullTag)
		}
	}
	return pullTag, warning
}
