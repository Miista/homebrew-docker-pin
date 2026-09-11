package compose

import (
	"strings"
	"testing"
)

func TestDigestOfTagOfShortDigest(t *testing.T) {
	const full = "sha256:41b194461e4bae16f9b25d68b0976ed4735b89ca625c89aad88e1c1c3b7e8860"
	for _, tc := range []struct{ in, digest, tag string }{
		{"nginx:latest@" + full, full, "latest"},
		{"nginx:1.26.3", "", "1.26.3"},
		{"registry.example.com:5000/app:v2", "", "v2"},
		{"nginx", "", ""},
	} {
		if got := DigestOf(tc.in); got != tc.digest {
			t.Errorf("DigestOf(%q) = %q, want %q", tc.in, got, tc.digest)
		}
		if got := TagOf(tc.in); got != tc.tag {
			t.Errorf("TagOf(%q) = %q, want %q", tc.in, got, tc.tag)
		}
	}
	if got := ShortDigest(full); got != "sha256:"+strings.TrimPrefix(full, "sha256:")[:12] {
		t.Errorf("ShortDigest = %q", got)
	}
	if got := ShortDigest("sha256:short"); got != "sha256:short" {
		t.Errorf("ShortDigest should pass through short input, got %q", got)
	}
}

// --- error paths ---------------------------------------------------------

// A registry port is a colon that is not a tag separator. There used to be
// four hand-written copies of this split across the two commands and the
// scheduler, and one of them -- docker pin's own tagOf -- had no port guard,
// so it read the tag of "localhost:5555/app" as "5555/app" and printed that in
// its summary table.
func TestTagOfDoesNotMistakeARegistryPortForATag(t *testing.T) {
	cases := []struct{ image, tag string }{
		// The bug: no tag at all, and the port must not become one.
		{"localhost:5555/app", ""},
		{"registry.example.com:5000/app", ""},

		// A port and a tag together: only the tag is the tag.
		{"localhost:5555/app:1.0.1", "1.0.1"},
		{"registry.example.com:5000/app:2.0", "2.0"},

		// Ordinary references still work.
		{"nginx:1.25", "1.25"},
		{"nginx", ""},
		{"ghcr.io/miista/duva:1.0.0", "1.0.0"},
	}
	for _, c := range cases {
		if got := TagOf(c.image); got != c.tag {
			t.Errorf("TagOf(%q) = %q, want %q", c.image, got, c.tag)
		}
	}
}
