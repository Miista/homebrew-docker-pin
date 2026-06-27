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

	usage := func() {
		fmt.Fprintln(os.Stderr, "Usage: docker upgrade <service> [version]")
		fmt.Fprintln(os.Stderr, "       docker upgrade --all")
		os.Exit(1)
	}

	if len(args) == 0 {
		usage()
	}

	if args[0] == "--all" {
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "Error: --all cannot be combined with a version")
			os.Exit(1)
		}
		if err := runAll(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(args) > 2 {
		usage()
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

func runAll() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	composeFile, err := compose.FindFile(wd)
	if err != nil {
		return err
	}
	services, err := compose.ListServices(composeFile)
	if err != nil {
		return err
	}
	var failed []string
	for _, service := range services {
		if err := run(service, ""); err != nil {
			fmt.Fprintf(os.Stderr, "Error upgrading %s: %v\n", service, err)
			failed = append(failed, service)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed to upgrade: %s", strings.Join(failed, ", "))
	}
	return nil
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

	baseImage, currentTag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return err
	}

	oldRaw, err := compose.RawImage(composeFile, service)
	if err != nil {
		return err
	}

	// With no explicit version, derive the moving tag for the line/variant the
	// service is currently on, so we never silently switch image flavor.
	pullTag := targetVersion
	if pullTag == "" {
		pullTag, err = registry.MovingPullTag(baseImage, currentTag)
		if err != nil {
			return err
		}
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

	// When we derived a moving tag (no explicit version), resolve it back to a
	// specific version tag for the pin.
	pinnedTag := pullTag
	if targetVersion == "" {
		pinnedTag = registry.ResolveOrWarn(baseImage, pullTag, digest, service)
	}

	pinned := fmt.Sprintf("%s:%s@%s", baseImage, pinnedTag, digest)
	if err := compose.PinImage(composeFile, service, pinned); err != nil {
		return err
	}
	fmt.Printf("Upgraded %s: %s -> %s\n", service, oldRaw, pinned)
	fmt.Printf("Pinned to %s\n", digest)
	return nil
}
