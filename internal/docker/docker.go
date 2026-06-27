package docker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Pull pulls the given image reference, streaming output to the terminal.
func Pull(imageRef string) error {
	cmd := exec.Command("docker", "pull", imageRef)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
