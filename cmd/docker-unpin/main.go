package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/help"
)

const (
	pluginName = "unpin"
	shortDesc  = "Remove the SHA digest pin from a service image, keeping its tag"
	vendor     = "Miista"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "docker-cli-plugin-metadata" {
		fmt.Printf(`{"SchemaVersion":"0.1.0","Vendor":%q,"Version":%q,"ShortDescription":%q}`+"\n",
			vendor, version, shortDesc)
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "__complete" || os.Args[1] == "__completeNoDesc") {
		runComplete(os.Args[2:])
		return
	}

	args := os.Args[1:]
	if len(args) > 0 && args[0] == pluginName {
		args = args[1:]
	}

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, help.UnpinUsage)
		os.Exit(1)
	}

	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Fprintln(os.Stderr, help.UnpinUsage)
			return
		}
	}

	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		fmt.Println("docker-unpin", version)
		return
	}

	dryRun := false
	filtered := args[:0:0]
	for _, a := range args {
		if a == "--dry-run" || a == "-n" {
			dryRun = true
			continue
		}
		filtered = append(filtered, a)
	}
	args = filtered
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, help.UnpinUsage)
		os.Exit(1)
	}

	if args[0] == "--all" || args[0] == "-a" {
		if err := runAll(dryRun); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, help.UnpinUsage)
		os.Exit(1)
	}

	if err := run(args[0], dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runAll(dryRun bool) error {
	composeFile, err := compose.Locate()
	if err != nil {
		return err
	}
	services, err := compose.ListServices(composeFile)
	if err != nil {
		return err
	}
	var failed []string
	for _, service := range services {
		if err := run(service, dryRun); err != nil {
			fmt.Fprintf(os.Stderr, "Error unpinning %s: %v\n", service, err)
			failed = append(failed, service)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed to unpin: %s", strings.Join(failed, ", "))
	}
	return nil
}

func run(service string, dryRun bool) error {
	composeFile, err := compose.ResolveService(service)
	if err != nil {
		return err
	}

	base, tag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return err
	}

	rawImage, err := compose.RawImage(composeFile, service)
	if err != nil {
		return err
	}
	if !strings.Contains(rawImage, "@sha256:") {
		fmt.Printf("%s is not pinned\n", service)
		return nil
	}

	unpinned := base + ":" + tag
	if dryRun {
		fmt.Printf("Would unpin %s: %s -> %s\n", service, rawImage, unpinned)
		return nil
	}
	if err := compose.PinImage(composeFile, service, unpinned); err != nil {
		return err
	}
	fmt.Printf("Unpinned %s: now at %s\n", service, unpinned)
	return nil
}
