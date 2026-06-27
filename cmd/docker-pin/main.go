package main

import (
	"fmt"
	"os"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/docker"
	"github.com/Miista/homebrew-docker-pin/internal/registry"
)

const (
	pluginName = "pin"
	shortDesc  = "Pin a service image to its current tag and SHA digest"
	vendor     = "Miista"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "docker-cli-plugin-metadata" {
		fmt.Printf(`{"SchemaVersion":"0.1.0","Vendor":%q,"Version":%q,"ShortDescription":%q}`+"\n",
			vendor, version, shortDesc)
		return
	}

	args := os.Args[1:]
	if len(args) > 0 && args[0] == pluginName {
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: docker pin <service>")
		os.Exit(1)
	}

	if err := run(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(service string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	composeFile, err := compose.FindFile(wd)
	if err != nil {
		return err
	}
	fmt.Printf("Using: %s\n", composeFile)

	baseImage, tag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return err
	}
	fmt.Printf("Service %q: %s:%s\n", service, baseImage, tag)

	pullRef := baseImage + ":" + tag
	digest, err := docker.GetDigest(pullRef)
	if err != nil {
		fmt.Printf("Image not found locally, pulling %s ...\n", pullRef)
		if err := docker.Pull(pullRef); err != nil {
			return fmt.Errorf("pull: %w", err)
		}
		digest, err = docker.GetDigest(pullRef)
		if err != nil {
			return err
		}
	}
	fmt.Printf("Digest: %s\n", digest)

	pinnedTag, err := resolveTag(baseImage, tag, digest)
	if err != nil {
		return err
	}

	pinned := fmt.Sprintf("%s:%s@%s", baseImage, pinnedTag, digest)
	fmt.Printf("Pinning to: %s\n", pinned)
	return compose.PinImage(composeFile, service, pinned)
}

// resolveTag returns the tag to pin with. If tag is "latest", attempts to
// resolve it to a specific version tag via the registry API.
func resolveTag(baseImage, tag, digest string) (string, error) {
	if tag != "latest" {
		return tag, nil
	}
	fmt.Println("Resolving version tag for latest ...")
	resolved, err := registry.ResolveVersionTag(baseImage, digest)
	if err != nil {
		fmt.Printf("Warning: version tag lookup failed (%v), pinning as latest\n", err)
		return "latest", nil
	}
	if resolved == "" {
		fmt.Println("No matching version tag found, pinning as latest")
		return "latest", nil
	}
	fmt.Printf("Resolved: %s\n", resolved)
	return resolved, nil
}
