package main

import "testing"

// splitRef separates what to pull from what to pull it by. Getting it wrong is
// quiet: a reference split at the wrong colon still looks like a reference, and
// the pull fails somewhere else entirely -- or worse, succeeds against the
// wrong tag.
func TestSplitRef(t *testing.T) {
	cases := []struct {
		ref, name, tag string
	}{
		{"nginx", "nginx", "latest"},
		{"nginx:1.25", "nginx", "1.25"},
		{"library/nginx:1.25", "library/nginx", "1.25"},
		{"ghcr.io/miista/duva:1.0.0", "ghcr.io/miista/duva", "1.0.0"},

		// The case the colon rule exists for: a registry port is a colon that
		// is not a tag separator. Splitting at the last colon only works
		// because what follows a port still contains a slash.
		{"localhost:5555/app", "localhost:5555/app", "latest"},
		{"localhost:5555/app:1.0.1", "localhost:5555/app", "1.0.1"},

		// A digest wins over any tag, and is returned whole: everything after
		// the @ is the tag return value, algorithm included.
		{"nginx@sha256:abc", "nginx", "sha256:abc"},
		{"nginx:1.25@sha256:abc", "nginx:1.25", "sha256:abc"},
		{"localhost:5555/app:1.0.1@sha256:abc", "localhost:5555/app:1.0.1", "sha256:abc"},
	}

	for _, c := range cases {
		name, tag := splitRef(c.ref)
		if name != c.name || tag != c.tag {
			t.Errorf("splitRef(%q) = (%q, %q), want (%q, %q)",
				c.ref, name, tag, c.name, c.tag)
		}
	}
}

// registryAuth is what the daemon is handed for a pull. Anonymous is the
// common case and must stay anonymous: a half-configured credential silently
// becoming a real one is how a wrong password reaches a registry.
func TestRegistryAuthIsAnonymousWithoutBothHalves(t *testing.T) {
	cases := []struct{ user, pass string }{
		{"", ""},
		{"someone", ""},
		{"", "secret"},
	}

	for _, c := range cases {
		t.Setenv("DUVA_REGISTRY_USER", c.user)
		t.Setenv("DUVA_REGISTRY_PASSWORD", c.pass)

		if got := registryAuth("ghcr.io/miista/duva"); got != "" {
			t.Errorf("user=%q password=%q should pull anonymously, got %q",
				c.user, c.pass, got)
		}
	}
}
