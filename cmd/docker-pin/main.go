package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/Miista/homebrew-docker-pin/internal/compose"
	"github.com/Miista/homebrew-docker-pin/internal/docker"
	"github.com/Miista/homebrew-docker-pin/internal/help"
	"github.com/Miista/homebrew-docker-pin/internal/registry"
)

const (
	pluginName = "pin"
	shortDesc  = "Pin a service image to its current tag and SHA digest"
	vendor     = "Miista"
)

var version = "dev"

type dockerFuncs struct {
	getDigest        func(ref string) (string, error)
	pull             func(ref string) error
	resolve          func(baseImage, pullTag, digest, service string) string
	listMatchingTags func(baseImage string, include, exclude *regexp.Regexp, current string) ([]string, error)
	tagCreated       func(baseImage, tag string) (time.Time, error)
}

var realDocker = dockerFuncs{
	getDigest:        docker.GetDigest,
	pull:             docker.Pull,
	resolve:          registry.ResolveOrWarn,
	listMatchingTags: registry.ListMatchingTags,
	tagCreated:       registry.TagCreated,
}

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
		printUsage()
		os.Exit(1)
	}

	// `help [<command>]` and -h/--help anywhere print help and exit 0.
	if done := maybeHelp(args); done {
		return
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println("docker-pin", version)
	case "upgrade":
		if err := runUpgrade(args[1:], realDocker); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "schedule":
		if err := runSchedule(args[1:], realDocker, realSys); err != nil {
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
	fmt.Fprintln(os.Stderr, help.PinUsage)
}

// maybeHelp handles `help [<command>]` and -h/--help in any position.
// Returns true (after printing) if help was requested.
func maybeHelp(args []string) bool {
	want := args[0] == "help"
	topic := ""
	if want && len(args) > 1 {
		topic = args[1]
		if topic == "schedule" && len(args) > 2 {
			switch args[2] {
			case "apply", "status", "remove", "run":
				topic = "schedule " + args[2]
			}
		}
	}
	for _, a := range args {
		if a == "-h" || a == "--help" {
			want = true
		}
	}
	if !want {
		return false
	}
	if topic == "" {
		switch args[0] {
		case "upgrade", "list", "version":
			topic = args[0]
		case "schedule":
			topic = "schedule"
			if len(args) > 1 {
				switch args[1] {
				case "apply", "status", "remove", "run":
					topic = "schedule " + args[1]
				}
			}
		case "help":
			// bare `help`: fall through to full usage
		case "-h", "--help":
			// bare `docker pin --help` / `-h`: fall through to full usage
		default:
			topic = "pin" // `docker pin <service> --help`
		}
	}
	if text, ok := help.For(help.PinTopics, topic); ok {
		fmt.Fprintln(os.Stderr, text)
	} else {
		printUsage()
	}
	return true
}

// pin

func runPin(args []string, d dockerFuncs) error {
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
		fmt.Fprintln(os.Stderr, "Usage: docker pin <service>")
		fmt.Fprintln(os.Stderr, "       docker pin --all")
		os.Exit(1)
	}
	if args[0] == "--all" || args[0] == "-a" {
		return pinAll(d, dryRun)
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: docker pin <service>")
		fmt.Fprintln(os.Stderr, "       docker pin --all")
		os.Exit(1)
	}
	_, err := pin(args[0], d, dryRun)
	return err
}

func pinAll(d dockerFuncs, dryRun bool) error {
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
		outcome pinOutcome
	}
	var results []result
	for _, service := range services {
		outcome, err := pin(service, d, dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error pinning %s: %v\n", service, err)
			failed = append(failed, service)
			continue
		}
		results = append(results, result{service, outcome})
	}

	if dryRun {
		fmt.Println()
		fmt.Println("Summary:")
		w := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tACTION\tSHA")
		for _, r := range results {
			action := "pin"
			if r.outcome.AlreadyPinned {
				action = "none"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.service, action, r.outcome.Digest)
		}
		for _, service := range failed {
			fmt.Fprintf(w, "%s\tFAILED\t-\n", service)
		}
		w.Flush()
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to pin: %s", strings.Join(failed, ", "))
	}
	return nil
}

func pin(service string, d dockerFuncs, dryRun bool) (pinOutcome, error) {
	root, err := compose.Locate()
	if err != nil {
		return pinOutcome{}, err
	}
	composeFile, err := compose.ResolveServiceIn(root, service)
	if err != nil {
		return pinOutcome{}, err
	}
	if composeFile != root && !dryRun {
		fmt.Printf("%s is declared in %s (via include:)\n", service, composeFile)
	}
	return pinInFile(composeFile, service, d, dryRun)
}

// pinOutcome describes what a pinInFile call did (or, in dry-run mode,
// would have done).
type pinOutcome struct {
	OldRaw string
	NewRaw string
	// Tag and Digest are the tag and digest the service is (or would be)
	// pinned to.
	Tag, Digest string
	// AlreadyPinned is true when the service was already digest-pinned, so
	// no pin (or would-be pin) happened.
	AlreadyPinned bool
}

func pinInFile(composeFile, service string, d dockerFuncs, dryRun bool) (pinOutcome, error) {
	baseImage, tag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return pinOutcome{}, err
	}

	raw, err := compose.RawImage(composeFile, service)
	if err != nil {
		return pinOutcome{}, err
	}

	if !dryRun {
		fmt.Printf("Read tag from compose file: %s\n", tag)
	}

	if strings.Contains(raw, "@sha256:") {
		if !dryRun {
			fmt.Printf("%s is already pinned to %s\n", service, raw)
			fmt.Println("Run `docker unpin` first, or `docker pin upgrade` to move to a new version.")
		}
		return pinOutcome{OldRaw: raw, NewRaw: raw, Tag: tag, Digest: digestOf(raw), AlreadyPinned: true}, nil
	}

	pullRef := baseImage + ":" + tag
	digest, err := d.getDigest(pullRef)
	if err != nil {
		fmt.Printf("Image not found locally, pulling %s ...\n", pullRef)
		if err := d.pull(pullRef); err != nil {
			return pinOutcome{}, fmt.Errorf("pull failed: %w", err)
		}
		digest, err = d.getDigest(pullRef)
		if err != nil {
			return pinOutcome{}, err
		}
		if !dryRun {
			fmt.Printf("Using digest from pulled image: %s\n", digest)
		}
	} else if !dryRun {
		fmt.Printf("Using digest from local image: %s\n", digest)
	}

	// Resolving `latest` to a version tag is purely cosmetic (the digest
	// pinned is the same either way), so skip the registry round-trip in
	// dry-run mode -- it's only worth paying for when we're about to write.
	pinnedTag := tag
	if tag == "latest" && !dryRun {
		pinnedTag = d.resolve(baseImage, tag, digest, service)
	}

	pinned := fmt.Sprintf("%s:%s@%s", baseImage, pinnedTag, digest)
	outcome := pinOutcome{OldRaw: raw, NewRaw: pinned, Tag: pinnedTag, Digest: digest}
	if dryRun {
		return outcome, nil
	}
	if err := compose.PinImage(composeFile, service, pinned); err != nil {
		return outcome, err
	}
	fmt.Printf("Pinned %s to %s\n", service, pinned)
	return outcome, nil
}

// list

func runList(args []string) error {
	missing, quiet := false, false
	for _, arg := range args {
		switch arg {
		case "-m", "--missing":
			missing = true
		case "-q", "--quiet":
			quiet = true
		default:
			fmt.Fprintln(os.Stderr, "Usage: docker pin list [--missing] [-q]")
			os.Exit(1)
		}
	}
	composeFile, err := compose.Locate()
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
		serviceFile, err := compose.ResolveServiceIn(composeFile, service)
		if err != nil {
			return 0, err
		}
		raw, err := compose.RawImage(serviceFile, service)
		if err != nil {
			return 0, err
		}
		base, tag, err := compose.ParseImage(serviceFile, service)
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
	dryRun := false
	concurrency := upgradeAllConcurrency
	filtered := args[:0:0]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--dry-run" || a == "-n":
			dryRun = true
		case a == "--concurrency" || a == "-j":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", a)
				os.Exit(1)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "Error: invalid --concurrency value %q\n", args[i])
				os.Exit(1)
			}
			concurrency = n
		default:
			filtered = append(filtered, a)
		}
	}
	args = filtered

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: docker pin upgrade <service> [version]")
		fmt.Fprintln(os.Stderr, "       docker pin upgrade --all [--concurrency N]")
		os.Exit(1)
	}

	if args[0] == "--all" || args[0] == "-a" {
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "Error: --all cannot be combined with a version")
			os.Exit(1)
		}
		return upgradeAll(d, dryRun, concurrency)
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
	_, err := upgrade(service, targetVersion, d, dryRun)
	return err
}

// upgradeAllConcurrency is the default cap on how many services pull/resolve
// at once. Kept modest because registries (esp. GHCR under load) throttle
// bursts of manifest-check requests; a resolve failure from throttling can
// masquerade as "no matching tag" (see registry.Result.ChecksFailed).
// Override with --concurrency/-j.
const upgradeAllConcurrency = 2

func upgradeAll(d dockerFuncs, dryRun bool, concurrency int) error {
	root, err := compose.Locate()
	if err != nil {
		return err
	}
	services, err := compose.ListServices(root)
	if err != nil {
		return err
	}

	type pulled struct {
		service     string
		composeFile string
		pullRef     string
		targetVer   string
		err         error
	}

	// Phase 1: resolve each service's pull ref and pull it, concurrently.
	// This touches only the registry and local docker state, never the
	// compose file, so it's safe to parallelize. All per-service detail is
	// suppressed in favor of a single progress counter; nothing here decides
	// whether a service actually changed -- that happens in phase 2.
	pulls := make([]pulled, len(services))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var done int32
	for i, service := range services {
		wg.Add(1)
		go func(i int, service string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			composeFile, err := compose.ResolveServiceIn(root, service)
			if err != nil {
				pulls[i] = pulled{service: service, err: err}
			} else if pullRef, targetVer, err := resolvePullRef(composeFile, service, "", dryRun, true); err != nil {
				pulls[i] = pulled{service: service, composeFile: composeFile, err: err}
			} else if err := d.pull(pullRef); err != nil {
				pulls[i] = pulled{service: service, composeFile: composeFile, err: fmt.Errorf("pull failed: %w", err)}
			} else {
				pulls[i] = pulled{service: service, composeFile: composeFile, pullRef: pullRef, targetVer: targetVer}
			}

			n := atomic.AddInt32(&done, 1)
			fmt.Printf("\rPulling %d of %d images...          ", n, len(services))
		}(i, service)
	}
	wg.Wait()
	fmt.Println()

	type computed struct {
		service     string
		composeFile string
		outcome     upgradeOutcome
		err         error
	}

	// Phase 2: work out each service's outcome (digest compare + moving-tag
	// resolution) concurrently -- this only hits local docker state and the
	// registry, never the compose file. Per-service progress lines are
	// suppressed in favor of a counter, same as phase 1; warnings still
	// print to stderr since they're rare and worth surfacing immediately.
	computedResults := make([]computed, len(pulls))
	done = 0
	var wg2 sync.WaitGroup
	for i, p := range pulls {
		if p.err != nil {
			computedResults[i] = computed{service: p.service, err: p.err}
			continue
		}
		wg2.Add(1)
		go func(i int, p pulled) {
			defer wg2.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			outcome, err := computeUpgrade(p.composeFile, p.service, p.targetVer, p.pullRef, d, dryRun, true)
			computedResults[i] = computed{service: p.service, composeFile: p.composeFile, outcome: outcome, err: err}

			n := atomic.AddInt32(&done, 1)
			fmt.Printf("\rResolving %d of %d images...          ", n, len(pulls))
		}(i, p)
	}
	wg2.Wait()
	if len(pulls) > 0 {
		fmt.Println()
	}

	// Phase 3: apply the compose file writes sequentially, in service order,
	// so concurrent resolves never race on the same file.
	var failed []string
	type result struct {
		service string
		outcome upgradeOutcome
	}
	var results []result
	for _, c := range computedResults {
		if c.err != nil {
			fmt.Fprintf(os.Stderr, "Error upgrading %s: %v\n", c.service, c.err)
			failed = append(failed, c.service)
			continue
		}
		results = append(results, result{c.service, c.outcome})
		if !dryRun && c.outcome.Changed {
			if err := applyUpgrade(c.composeFile, c.service, c.outcome); err != nil {
				fmt.Fprintf(os.Stderr, "Error upgrading %s: %v\n", c.service, err)
				failed = append(failed, c.service)
				continue
			}
			fmt.Printf("Upgraded %s: %s -> %s\n", c.service, c.outcome.OldRaw, c.outcome.NewRaw)
			fmt.Printf("Pinned to %s\n", c.outcome.Digest)
		}
	}

	if dryRun {
		fmt.Println()
		fmt.Println("Summary:")
		w := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tACTION\tTAG\tSHA")
		for _, r := range results {
			action := "none"
			tag := tagOf(r.outcome.OldRaw)
			if r.outcome.Changed {
				action = "upgrade"
				if newTag := tagOf(r.outcome.NewRaw); newTag != tag {
					tag = tag + " -> " + newTag
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.service, action, tag, r.outcome.Digest)
		}
		for _, service := range failed {
			fmt.Fprintf(w, "%s\tFAILED\t-\t-\n", service)
		}
		w.Flush()
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to upgrade: %s", strings.Join(failed, ", "))
	}
	return nil
}

func upgrade(service, targetVersion string, d dockerFuncs, dryRun bool) (upgradeOutcome, error) {
	root, err := compose.Locate()
	if err != nil {
		return upgradeOutcome{}, err
	}
	composeFile, err := compose.ResolveServiceIn(root, service)
	if err != nil {
		return upgradeOutcome{}, err
	}
	if composeFile != root && !dryRun {
		fmt.Printf("%s is declared in %s (via include:)\n", service, composeFile)
	}
	return upgradeInFile(composeFile, service, targetVersion, d, dryRun)
}

// upgradeOutcome describes what an upgradeInFile call did (or, in dry-run
// mode, would have done).
type upgradeOutcome struct {
	OldRaw string
	NewRaw string
	// Tag and Digest are the tag and digest the service is (or would be)
	// pinned to.
	Tag, Digest string
	// Changed is true when the pin moved (or would move, in a dry run).
	Changed bool
}

// upgradeInFile pulls the target (or discovered moving) tag and pins service
// to the pulled digest. With dryRun it does everything except rewrite the
// compose file, printing what the upgrade would be instead.
func upgradeInFile(composeFile, service, targetVersion string, d dockerFuncs, dryRun bool) (upgradeOutcome, error) {
	pullRef, targetVersion, err := resolvePullRef(composeFile, service, targetVersion, dryRun, false)
	if err != nil {
		return upgradeOutcome{}, err
	}
	fmt.Printf("Pulling %s ...\n", pullRef)
	if err := d.pull(pullRef); err != nil {
		return upgradeOutcome{}, fmt.Errorf("pull failed: %w", err)
	}

	outcome, err := computeUpgrade(composeFile, service, targetVersion, pullRef, d, dryRun, false)
	if err != nil || dryRun || !outcome.Changed {
		return outcome, err
	}
	if err := applyUpgrade(composeFile, service, outcome); err != nil {
		return outcome, err
	}
	fmt.Printf("Upgraded %s: %s -> %s\n", service, outcome.OldRaw, outcome.NewRaw)
	fmt.Printf("Pinned to %s\n", outcome.Digest)
	return outcome, nil
}

// applyUpgrade rewrites the compose file's image line for service to the
// pinned image computed by computeUpgrade. Kept separate from computeUpgrade
// so callers (e.g. upgradeAll) can apply the file writes sequentially.
func applyUpgrade(composeFile, service string, outcome upgradeOutcome) error {
	return compose.PinImage(composeFile, service, outcome.NewRaw)
}

// resolvePullRef works out which image ref to pull for service: either the
// explicit targetVersion, or (if empty) the discovered moving tag for its
// current tag. Returns the full pull ref and the resolved targetVersion
// (unchanged if it was already explicit). Does no I/O beyond the registry
// lookup needed to discover the moving tag, and never pulls — safe to run
// concurrently across services so all pulls can be kicked off in parallel.
// quiet suppresses the "checking whether..." progress line, for use during
// upgradeAll's parallel pull phase where per-service lines would race with
// the shared progress counter.
func resolvePullRef(composeFile, service, targetVersion string, dryRun, quiet bool) (pullRef, resolvedVersion string, err error) {
	baseImage, currentTag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return "", "", err
	}

	pullTag := targetVersion
	if pullTag == "" {
		pullTag, err = registry.MovingPullTag(baseImage, currentTag)
		if err != nil {
			return "", "", err
		}
		if !dryRun && !quiet {
			fmt.Printf("%s: on %s:%s — checking whether the %q moving tag has a newer build ...\n",
				service, baseImage, currentTag, pullTag)
		}
	}
	return baseImage + ":" + pullTag, targetVersion, nil
}

// computeUpgrade works out what the new pinned image reference would be for
// a service whose target pullRef has already been pulled. Must run after the
// pull. quiet suppresses per-service progress lines (but not warnings, which
// still go to stderr) for use when many services are computed concurrently.
func computeUpgrade(composeFile, service, targetVersion, pullRef string, d dockerFuncs, dryRun, quiet bool) (upgradeOutcome, error) {
	baseImage, currentTag, err := compose.ParseImage(composeFile, service)
	if err != nil {
		return upgradeOutcome{}, err
	}

	oldRaw, err := compose.RawImage(composeFile, service)
	if err != nil {
		return upgradeOutcome{}, err
	}
	outcome := upgradeOutcome{OldRaw: oldRaw, NewRaw: oldRaw, Tag: currentTag, Digest: digestOf(oldRaw)}

	digest, err := d.getDigest(pullRef)
	if err != nil {
		return outcome, err
	}

	if oldDigest := digestOf(oldRaw); oldDigest != "" && oldDigest == digest {
		if !dryRun && !quiet {
			fmt.Printf("%s: up to date — %s still points at the pinned digest (%s)\n", service, pullRef, shortDigest(oldDigest))
		}
		return outcome, nil
	}

	pullTag := strings.TrimPrefix(pullRef, baseImage+":")

	// Resolve the moving tag (e.g. "latest") to the specific version tag it
	// matches, so both the write and the dry-run summary show what's
	// actually being pinned to, not just the moving tag name.
	pinnedTag := pullTag
	if targetVersion == "" {
		resolve := d.resolve
		if quiet {
			resolve = registry.ResolveOrWarnQuiet
		}
		pinnedTag = resolve(baseImage, pullTag, digest, service)
	}

	pinned := fmt.Sprintf("%s:%s@%s", baseImage, pinnedTag, digest)
	outcome.NewRaw = pinned
	outcome.Tag = pinnedTag
	outcome.Digest = digest
	outcome.Changed = true
	return outcome, nil
}

func digestOf(image string) string {
	if i := strings.Index(image, "@"); i != -1 {
		return image[i+1:]
	}
	return ""
}

// tagOf extracts the tag from a "base:tag" or "base:tag@sha256:..." image
// reference, for display purposes (e.g. summarizing what a moving tag like
// "latest" actually resolved to).
func tagOf(image string) string {
	if i := strings.Index(image, "@"); i != -1 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i != -1 {
		return image[i+1:]
	}
	return ""
}
