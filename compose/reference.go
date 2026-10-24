package compose

import "strings"

// Reading an image reference: the parts of "base:tag@sha256:..." that can be
// answered by looking at the string.
//
// Here rather than in a package about pinning, because extracting a digest
// from a reference is not pinning -- it is reading what a compose file says,
// which is what this package is for. It lived under pin only because pin was
// the first thing that needed it, and that left the decider importing a write
// engine to answer a question about text.

// DigestOf extracts "sha256:..." from an "image@sha256:..." reference, or ""
// if the reference carries no digest.
func DigestOf(image string) string {
	if i := strings.Index(image, "@"); i != -1 {
		return image[i+1:]
	}
	return ""
}

// TagOf extracts the tag from "base:tag" or "base:tag@sha256:...".
func TagOf(image string) string {
	if i := strings.Index(image, "@"); i != -1 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i != -1 {
		// A colon before the last slash is a registry port, not a tag.
		if !strings.Contains(image[i:], "/") {
			return image[i+1:]
		}
	}
	return ""
}

// ShortDigest abbreviates "sha256:<64 hex>" to its first 12 hex characters.

func ShortDigest(digest string) string {
	const prefix = "sha256:"
	if strings.HasPrefix(digest, prefix) && len(digest) > len(prefix)+12 {
		return digest[:len(prefix)+12]
	}
	return digest
}
