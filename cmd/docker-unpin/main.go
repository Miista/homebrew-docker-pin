package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

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

	dryRun, all := false, false
	filtered := args[:0:0]
	for _, a := range args {
		switch a {
		case "--dry-run", "-n":
			dryRun = true
		case "--all", "-a":
			all = true
		default:
			filtered = append(filtered, a)
		}
	}
	args = filtered

	// Anything flag-shaped left over is a typo. Silently ignoring it meant
	// `unpin --all --dry-riun` stripping every digest in the repo for real,
	// because --all was matched before the argument count was checked.
	for _, a := range args {
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "Error: unknown flag %q\n", a)
			fmt.Fprintln(os.Stderr, help.UnpinUsage)
			os.Exit(1)
		}
	}

	if all {
		if len(args) > 0 {
			fmt.Fprintln(os.Stderr, "Error: --all cannot be combined with a service name")
			os.Exit(1)
		}
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

	if _, err := run(args[0], dryRun); err != nil {
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
	type result struct {
		service string
		outcome unpinOutcome
	}
	var results []result
	for _, service := range services {
		outcome, err := run(service, dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error unpinning %s: %v\n", service, err)
			failed = append(failed, service)
			continue
		}
		results = append(results, result{service, outcome})
	}

	if dryRun {
		fmt.Println()
		fmt.Println("Summary:")
		w := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tACTION\tTAG\tSHA")
		for _, r := range results {
			action := "unpin"
			if r.outcome.NotPinned {
				action = "none"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.service, action, r.outcome.Tag, r.outcome.Digest)
		}
		for _, service := range failed {
			fmt.Fprintf(w, "%s\tFAILED\t-\t-\n", service)
		}
		w.Flush()
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to unpin: %s", strings.Join(failed, ", "))
	}
	return nil
}

// unpinOutcome describes what a run call did (or, in dry-run mode, would
// have done).
type unpinOutcome struct {
	OldRaw string
	NewRaw string
	// Tag is the tag the service is (or would be) left at. Digest is the
	// pinned digest being removed (empty if there was none).
	Tag, Digest string
	// NotPinned is true when the service had no digest pin to remove.
	NotPinned bool
}

func run(service string, dryRun bool) (unpinOutcome, error) {
	root, err := compose.Locate()
	if err != nil {
		return unpinOutcome{}, err
	}
	composeFile, err := compose.ResolveServiceIn(root, service)
	if err != nil {
		return unpinOutcome{}, err
	}
	if composeFile != root && !dryRun {
		fmt.Printf("%s is declared in %s (via include:)\n", service, composeFile)
	}

	base, tag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return unpinOutcome{}, err
	}

	rawImage, err := compose.RawImage(composeFile, service)
	if err != nil {
		return unpinOutcome{}, err
	}
	if !strings.Contains(rawImage, "@sha256:") {
		if !dryRun {
			fmt.Printf("%s is not pinned\n", service)
		}
		return unpinOutcome{OldRaw: rawImage, NewRaw: rawImage, Tag: tag, NotPinned: true}, nil
	}

	unpinned := base + ":" + tag
	outcome := unpinOutcome{OldRaw: rawImage, NewRaw: unpinned, Tag: tag, Digest: digestOf(rawImage)}
	if dryRun {
		return outcome, nil
	}
	if err := compose.PinImage(composeFile, service, unpinned); err != nil {
		return outcome, err
	}
	fmt.Printf("Unpinned %s: now at %s\n", service, unpinned)
	return outcome, nil
}

// digestOf extracts the "sha256:..." digest from an "image@sha256:..." ref.
func digestOf(image string) string {
	if i := strings.Index(image, "@"); i != -1 {
		return image[i+1:]
	}
	return ""
}
