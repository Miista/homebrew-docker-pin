package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
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

	args := os.Args[1:]
	if len(args) > 0 && args[0] == pluginName {
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: docker unpin <service>")
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
	if err := compose.PinImage(composeFile, service, unpinned); err != nil {
		return err
	}
	fmt.Printf("Unpinned %s: now at %s\n", service, unpinned)
	return nil
}
