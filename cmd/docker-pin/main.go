package main

import (
	"fmt"
	"os"
	"strings"

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

	baseImage, tag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return err
	}

	raw, err := compose.RawImage(composeFile, service)
	if err != nil {
		return err
	}
	if strings.Contains(raw, "@sha256:") {
		fmt.Printf("%s is already pinned to %s\n", service, raw)
		fmt.Println("Run `docker unpin` first, or `docker upgrade` to move to a new version.")
		return nil
	}

	pullRef := baseImage + ":" + tag
	digest, err := docker.GetDigest(pullRef)
	if err != nil {
		fmt.Printf("Image not found locally, pulling %s ...\n", pullRef)
		if err := docker.Pull(pullRef); err != nil {
			return fmt.Errorf("pull failed: %w", err)
		}
		digest, err = docker.GetDigest(pullRef)
		if err != nil {
			return err
		}
	}

	pinnedTag := tag
	if tag == "latest" {
		fmt.Printf("Resolving version tag for %s ...\n", pullRef)
		resolved, err := registry.ResolveVersionTag(baseImage, digest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not resolve version tag (%v), pinning as latest\n", err)
		} else if resolved != "" {
			pinnedTag = resolved
		} else {
			fmt.Fprintf(os.Stderr, "Warning: no matching version tag found in registry, pinning as latest\n")
		}
	}

	pinned := fmt.Sprintf("%s:%s@%s", baseImage, pinnedTag, digest)
	if err := compose.PinImage(composeFile, service, pinned); err != nil {
		return err
	}
	fmt.Printf("Pinned %s to %s\n", service, pinned)
	return nil
}
