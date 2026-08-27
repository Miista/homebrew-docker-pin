package docker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Pull pulls the given image reference quietly (-q): docker's layer-progress
// dump would drown the tool's own output. Errors still stream to stderr.
func Pull(imageRef string) error {
	cmd := exec.Command("docker", "pull", "-q", imageRef)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunningDigest returns the repo digest of the image a running container was
// started from, for the compose service in composeFile's project directory.
// An empty digest (with a nil error) means "nothing to go on": no container
// for that service, or its image has no repo digest for baseImage — a locally
// built image, or one whose tag has since been repointed and pruned.
//
// This exists because the tag alone is not enough. `docker pin` is normally
// run after a stack has been up for a while, and in that window something
// else may re-pull the moving tag (a sibling service on the same base image,
// a build, a manual pull). The local image for `nginx:latest` then points at
// a newer digest than the container is actually running, and pinning it would
// record a digest that never ran on this host.
func RunningDigest(composeDir, service, baseImage string) (string, error) {
	// Find the container by compose's own labels rather than by name: a
	// service's container name is configurable (container_name:) and its
	// default form is project-service-N, neither of which we can assume.
	out, err := exec.Command(
		"docker", "ps", "--quiet", "--filter", "label=com.docker.compose.service="+service,
		"--filter", "label=com.docker.compose.project.working_dir="+composeDir,
	).Output()
	if err != nil {
		return "", fmt.Errorf("listing containers for %s: %w", service, err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return "", nil
	}

	imageID, err := exec.Command("docker", "inspect", ids[0], "--format", "{{.Image}}").Output()
	if err != nil {
		return "", fmt.Errorf("inspect container for %s: %w", service, err)
	}

	digests, err := exec.Command(
		"docker", "image", "inspect", strings.TrimSpace(string(imageID)),
		"--format", "{{range .RepoDigests}}{{println .}}{{end}}",
	).Output()
	if err != nil {
		// The image the container runs can be gone from the image store
		// (pruned after the tag moved) -- not an error worth failing on.
		return "", nil
	}

	// An image can carry digests for several repos (retagged/mirrored), so
	// match the one for the image we are actually pinning.
	for _, line := range strings.Fields(string(digests)) {
		repo, digest, ok := strings.Cut(line, "@")
		if ok && repo == baseImage {
			return digest, nil
		}
	}
	return "", nil
}

// GetDigest returns the repo digest (sha256:...) for a locally-present image.
func GetDigest(imageRef string) (string, error) {
	out, err := exec.Command(
		"docker", "image", "inspect", imageRef,
		"--format", "{{index .RepoDigests 0}}",
	).Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", imageRef, err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", fmt.Errorf("no repo digest found for %s", imageRef)
	}
	// RepoDigests format: "image@sha256:abc123" — extract "sha256:abc123"
	if i := strings.Index(raw, "@"); i != -1 {
		return raw[i+1:], nil
	}
	return raw, nil
}
