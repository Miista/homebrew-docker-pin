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
		{"nginx@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "nginx", "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"nginx:1.25@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "nginx:1.25", "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"localhost:5555/app:1.0.1@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "localhost:5555/app:1.0.1", "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}

	for _, c := range cases {
		name, tag := splitRef(c.ref)
		if name != c.name || tag != c.tag {
			t.Errorf("splitRef(%q) = (%q, %q), want (%q, %q)",
				c.ref, name, tag, c.name, c.tag)
		}
	}
}

// registryOf decides which registry a password is sent to, so the cases that
// matter are the ones where a private host could be mistaken for Docker Hub.
// These follow docker's own splitDockerDomain; two of them were wrong here.
func TestRegistryOfDoesNotLeakPrivateHostsToHub(t *testing.T) {
	const hub = "https://index.docker.io/v1/"

	cases := []struct{ image, want string }{
		// Was broken: localhost is a reserved namespace and always a host,
		// with or without a port. Without one it has no dot and no colon, so
		// the dot-or-colon rule alone sent its credential to Docker Hub.
		{"localhost/app", "localhost"},
		{"localhost:5555/app", "localhost:5555"},

		// Was broken: a namespace must be lowercase, so a first segment that
		// is not lowercase is a hostname.
		{"Registry.Example.COM/app", "Registry.Example.COM"},

		// Unambiguous hosts, by dot, by port, by both.
		{"ghcr.io/miista/duva", "ghcr.io"},
		{"127.0.0.1:5000/app", "127.0.0.1:5000"},

		// Genuinely Docker Hub: a bare name, and a plain lowercase namespace.
		{"nginx", hub},
		{"miista/duva", hub},
	}

	for _, c := range cases {
		if got := registryOf(c.image); got != c.want {
			t.Errorf("registryOf(%q) = %q, want %q", c.image, got, c.want)
		}
	}
}

// Every reference duva watches carries a digest -- that is what pinning means
// -- so the digest-bearing form is the one that has to work, not the plain one.
//
// It is a silent failure if it does not. The parser rejects a digest whose
// algorithm is unavailable, which depends on crypto/sha256 being linked into
// the binary; registryOf cannot tell that from "this image is on Docker Hub",
// so a private registry's password would go to Hub. Nothing about the pull
// would look wrong until it was refused.
func TestRegistryOfHandlesDigestPinnedReferences(t *testing.T) {
	const hub = "https://index.docker.io/v1/"

	// A real digest: the parser validates the encoding, so a made-up one is
	// rejected for the wrong reason and would pass this test vacuously.
	const digest = "@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"

	cases := []struct{ image, want string }{
		{"ghcr.io/miista/duva:1.0.0" + digest, "ghcr.io"},
		{"localhost:5555/app:1.0.1" + digest, "localhost:5555"},
		{"localhost/app" + digest, "localhost"},
		{"registry.example.com:5000/app:1.0" + digest, "registry.example.com:5000"},

		// Digest with no tag at all, which is a legal reference.
		{"ghcr.io/miista/duva" + digest, "ghcr.io"},

		// Hub, still Hub with a digest on it.
		{"nginx:1.25" + digest, hub},
		{"miista/duva" + digest, hub},
	}

	for _, c := range cases {
		if got := registryOf(c.image); got != c.want {
			t.Errorf("registryOf(%q) = %q, want %q\n"+
				"a digest-pinned reference must resolve to its own registry",
				c.image, got, c.want)
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
