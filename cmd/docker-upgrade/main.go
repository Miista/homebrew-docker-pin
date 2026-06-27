package main

import (
	"fmt"
	"os"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/docker"
	"github.com/Miista/homebrew-docker-pin/internal/registry"
)

const (
	pluginName = "upgrade"
	shortDesc  = "Upgrade a service image and pin it to a specific tag and SHA digest"
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
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "Usage: docker upgrade <service> [version]")
		os.Exit(1)
	}

	service := args[0]
	targetVersion := ""
	if len(args) == 2 {
		targetVersion = args[1]
	}

	if err := run(service, targetVersion); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(service, targetVersion string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	composeFile, err := compose.FindFile(wd)
	if err != nil {
		return err
	}

	baseImage, _, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return err
	}

	oldRaw, err := compose.RawImage(composeFile, service)
	if err != nil {
		return err
	}

	pullTag := "latest"
	if targetVersion != "" {
		pullTag = targetVersion
	}

	pullRef := baseImage + ":" + pullTag
	fmt.Printf("Pulling %s ...\n", pullRef)
	if err := docker.Pull(pullRef); err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}

	digest, err := docker.GetDigest(pullRef)
	if err != nil {
		return err
	}

	pinnedTag := pullTag
	if pullTag == "latest" {
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
	fmt.Printf("Upgraded %s: %s -> %s\n", service, oldRaw, pinned)
	fmt.Printf("Pinned to %s\n", digest)
	return nil
}
