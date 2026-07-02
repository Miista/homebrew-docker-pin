package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

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
	case "list":
		if err := runList(args[1:]); err != nil {
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
	fmt.Fprintln(os.Stderr, "       docker pin list [--missing] [-q]")
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

// list

func runList(args []string) error {
	missing, quiet := false, false
	for _, arg := range args {
		switch arg {
		case "--missing":
			missing = true
		case "-q", "--quiet":
			quiet = true
		default:
			fmt.Fprintln(os.Stderr, "Usage: docker pin list [--missing] [-q]")
			os.Exit(1)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	composeFile, err := compose.FindFile(wd)
	if err != nil {
		return err
	}
	unpinned, err := listInFile(composeFile, missing, quiet, os.Stdout)
	if err != nil {
		return err
	}
	// In --missing mode the exit code is the point: non-zero when anything
	// is unpinned, so `docker pin list --missing` works as a CI gate.
	if missing && unpinned > 0 {
		os.Exit(1)
	}
	return nil
}

func listInFile(composeFile string, missing, quiet bool, out io.Writer) (unpinned int, err error) {
	services, err := compose.ListServices(composeFile)
	if err != nil {
		return 0, err
	}
	sort.Strings(services)

	type row struct {
		service, base, tag, digest string
		pinned                     bool
	}
	var rows []row
	for _, service := range services {
		raw, err := compose.RawImage(composeFile, service)
		if err != nil {
			return 0, err
		}
		base, tag, err := compose.ParseImage(composeFile, service)
		if err != nil {
			return 0, err
		}
		digest := digestOf(raw)
		r := row{service: service, base: base, tag: tag, digest: digest, pinned: digest != ""}
		if !r.pinned {
			unpinned++
		}
		if missing && r.pinned {
			continue
		}
		rows = append(rows, r)
	}

	if quiet {
		for _, r := range rows {
			fmt.Fprintln(out, r.service)
		}
		return unpinned, nil
	}

	w := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tIMAGE\tTAG\tDIGEST\tPINNED")
	for _, r := range rows {
		digest, pin := "-", "✗"
		if r.pinned {
			digest, pin = shortDigest(r.digest), "✓"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.service, r.base, r.tag, digest, pin)
	}
	return unpinned, w.Flush()
}

// shortDigest abbreviates "sha256:<64 hex>" to its first 12 hex chars.
func shortDigest(digest string) string {
	h := strings.TrimPrefix(digest, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
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
