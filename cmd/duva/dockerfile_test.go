package main

import (
	"os"
	"strings"
	"testing"
)

// The image the integration suites build and the image that ships must be the
// same image. They are two files only because goreleaser cross-compiles the
// binary per architecture and copies it in, while the other builds from
// source -- so the build stage differs and nothing else may.
//
// This is not hypothetical. The released image was distroless-static long
// after duva started needing git and the docker CLI at runtime: every test
// passed against an image nobody shipped, and the one that shipped could not
// have worked.

func TestDockerfilesShipTheSameImage(t *testing.T) {
	tested := read(t, "Dockerfile")
	released := read(t, "Dockerfile.goreleaser")

	// The base is the whole point: same image, same digest.
	if got, want := finalFrom(t, released), finalFrom(t, tested); got != want {
		t.Errorf("the released image is built on a different base:\n  ships  %s\n  tested %s", got, want)
	}

	// What the image contains beyond the binary. A directory or a port in one
	// and not the other is a difference an operator meets and no test does.
	for _, line := range []string{"RUN mkdir -p /data", "EXPOSE 8080",
		`ENTRYPOINT ["/duva"]`, `CMD ["serve"]`} {
		if !strings.Contains(tested, line) {
			t.Errorf("Dockerfile is missing %q", line)
		}
		if !strings.Contains(released, line) {
			t.Errorf("Dockerfile.goreleaser is missing %q", line)
		}
	}
}

// Every base image carries a digest, so that a rebuild produces what was
// built before -- which is what this tool exists to ask of everyone else.
func TestBaseImagesArePinned(t *testing.T) {
	for _, name := range []string{"Dockerfile", "Dockerfile.goreleaser"} {
		for _, line := range strings.Split(read(t, name), "\n") {
			if !strings.HasPrefix(line, "FROM ") {
				continue
			}
			if !strings.Contains(line, "@sha256:") {
				t.Errorf("%s: unpinned base image, so a rebuild is not reproducible:\n  %s",
					name, line)
			}
		}
	}
}

// finalFrom is the image the last stage is built on: what actually ships.
func finalFrom(t *testing.T, dockerfile string) string {
	t.Helper()
	var last string
	for _, line := range strings.Split(dockerfile, "\n") {
		if strings.HasPrefix(line, "FROM ") {
			last = strings.TrimSpace(strings.TrimPrefix(line, "FROM "))
		}
	}
	if last == "" {
		t.Fatal("no FROM line")
	}
	// A build stage is named; the final one is not.
	if name, _, found := strings.Cut(last, " AS "); found {
		return name
	}
	return last
}

func read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(raw)
}
