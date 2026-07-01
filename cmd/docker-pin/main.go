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

type dockerFuncs struct {
	getDigest func(ref string) (string, error)
	pull      func(ref string) error
}

var realDocker = dockerFuncs{
	getDigest: docker.GetDigest,
	pull:      docker.Pull,
}

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

	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "upgrade":
		if err := runUpgrade(args[1:], realDocker); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	default:
		if err := runPin(args, realDocker); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: docker pin <service>")
	fmt.Fprintln(os.Stderr, "       docker pin --all")
	fmt.Fprintln(os.Stderr, "       docker pin upgrade <service> [version]")
	fmt.Fprintln(os.Stderr, "       docker pin upgrade --all")
}

// pin

func runPin(args []string, d dockerFuncs) error {
	if args[0] == "--all" {
		return pinAll(d)
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: docker pin <service>")
		fmt.Fprintln(os.Stderr, "       docker pin --all")
		os.Exit(1)
	}
	return pin(args[0], d)
}

func pinAll(d dockerFuncs) error {
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
		if err := pin(service, d); err != nil {
			fmt.Fprintf(os.Stderr, "Error pinning %s: %v\n", service, err)
			failed = append(failed, service)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed to pin: %s", strings.Join(failed, ", "))
	}
	return nil
}

func pin(service string, d dockerFuncs) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	composeFile, err := compose.FindFile(wd)
	if err != nil {
		return err
	}
	return pinInFile(composeFile, service, d)
}

func pinInFile(composeFile, service string, d dockerFuncs) error {
	baseImage, tag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return err
	}

	raw, err := compose.RawImage(composeFile, service)
	if err != nil {
		return err
	}

	fmt.Printf("Read tag from compose file: %s\n", tag)

	if strings.Contains(raw, "@sha256:") {
		fmt.Printf("%s is already pinned to %s\n", service, raw)
		fmt.Println("Run `docker unpin` first, or `docker pin upgrade` to move to a new version.")
		return nil
	}

	pullRef := baseImage + ":" + tag
	digest, err := d.getDigest(pullRef)
	if err != nil {
		fmt.Printf("Image not found locally, pulling %s ...\n", pullRef)
		if err := d.pull(pullRef); err != nil {
			return fmt.Errorf("pull failed: %w", err)
		}
		digest, err = d.getDigest(pullRef)
		if err != nil {
			return err
		}
		fmt.Printf("Using digest from pulled image: %s\n", digest)
	} else {
		fmt.Printf("Using digest from local image: %s\n", digest)
	}

	pinnedTag := tag
	if tag == "latest" {
		pinnedTag = registry.ResolveOrWarn(baseImage, tag, digest, service)
	}

	pinned := fmt.Sprintf("%s:%s@%s", baseImage, pinnedTag, digest)
	if err := compose.PinImage(composeFile, service, pinned); err != nil {
		return err
	}
	fmt.Printf("Pinned %s to %s\n", service, pinned)
	return nil
}

// upgrade

func runUpgrade(args []string, d dockerFuncs) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: docker pin upgrade <service> [version]")
		fmt.Fprintln(os.Stderr, "       docker pin upgrade --all")
		os.Exit(1)
	}

	if args[0] == "--all" {
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "Error: --all cannot be combined with a version")
			os.Exit(1)
		}
		return upgradeAll(d)
	}

	if len(args) > 2 {
		fmt.Fprintln(os.Stderr, "Usage: docker pin upgrade <service> [version]")
		fmt.Fprintln(os.Stderr, "       docker pin upgrade --all")
		os.Exit(1)
	}

	service := args[0]
	targetVersion := ""
	if len(args) == 2 {
		targetVersion = args[1]
	}
	return upgrade(service, targetVersion, d)
}

func upgradeAll(d dockerFuncs) error {
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
		if err := upgrade(service, "", d); err != nil {
			fmt.Fprintf(os.Stderr, "Error upgrading %s: %v\n", service, err)
			failed = append(failed, service)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed to upgrade: %s", strings.Join(failed, ", "))
	}
	return nil
}

func upgrade(service, targetVersion string, d dockerFuncs) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	composeFile, err := compose.FindFile(wd)
	if err != nil {
		return err
	}
	return upgradeInFile(composeFile, service, targetVersion, d)
}

func upgradeInFile(composeFile, service, targetVersion string, d dockerFuncs) error {
	baseImage, currentTag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return err
	}

	oldRaw, err := compose.RawImage(composeFile, service)
	if err != nil {
		return err
	}

	pullTag := targetVersion
	if pullTag == "" {
		pullTag, err = registry.MovingPullTag(baseImage, currentTag)
		if err != nil {
			return err
		}
	}

	pullRef := baseImage + ":" + pullTag
	fmt.Printf("Pulling %s ...\n", pullRef)
	if err := d.pull(pullRef); err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}

	digest, err := d.getDigest(pullRef)
	if err != nil {
		return err
	}

	if oldDigest := digestOf(oldRaw); oldDigest != "" && oldDigest == digest {
		fmt.Printf("%s is already up to date (%s)\n", service, oldRaw)
		return nil
	}

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

func digestOf(image string) string {
	if i := strings.Index(image, "@"); i != -1 {
		return image[i+1:]
	}
	return ""
}
