// detector finds tags that appeared since it last looked.
//
// It reads the compose project, asks each service's registry which tags
// exist, keeps the ones published since the last check, and publishes them to
// a webhook. It
// knows nothing about versions: not semver, not calver, not whether 4.39.25
// beats 4.39.20. Deciding what a tag means belongs to the gate, and a
// detector that also understood versions would be a second thing that had to
// agree with the gate about ordering.
//
// It does not know what is subscribed to its webhook, either. A gate, a
// notifier, a script that appends to a file -- it publishes what it observed
// in its own shape, and what that means is the subscriber's business.
//
// It holds no docker socket, writes nothing, and its only state is one
// timestamp. Losing that costs a noisy run, not a rebuild -- which is the
// whole reason it is one timestamp rather than a per-service baseline.
//
//	/compose   the compose project directory, read-only
//	/data      one timestamp, so a restart does not re-report everything
//
// Configuration:
//
//	DETECTOR_HOST            what this detector calls itself
//	DETECTOR_COMPOSE_SUBDIR  where the project lives within /compose
//	DETECTOR_WEBHOOK_URL     where to publish findings; empty means log only
//	DETECTOR_SINCE           override the cutoff for one run (RFC 3339, or a
//	                         duration like 168h meaning "the last week")
//	DETECTOR_LOG_LEVEL       trace/debug/info/warn/error
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	// The timezone database, embedded in the binary. Go reads TZ on its own,
	// but resolves a name like Europe/Copenhagen against the host's
	// /usr/share/zoneinfo -- which a distroless image does not carry, so TZ
	// would be silently ignored and every timestamp would stay UTC. ~450KB
	// to make the setting mean what it says, and this tool's whole output is
	// timestamps.
	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/compose"
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/detect"
	"github.com/Miista/homebrew-docker-pin/oci/registry"
)

var version = "dev"

var (
	composeDir = "/compose"
	stateFile  = "/data/detector.json"
)

// slowCheck is how long one service may take before it is worth saying so.
//
// A service is checked by listing its tags and then asking when each
// candidate was published -- one request per tag. A repository with hundreds
// of them takes a minute on its own, and silence for a minute reads as a
// hang. Better to say which service is costing the time than to leave the
// reader guessing whether anything is happening.
const slowCheck = 10 * time.Second

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("detector", version)
			return
		case "run":
			// The only mode. Named anyway, so the command reads as a verb
			// and so adding a second one later does not change how this is
			// invoked.
		default:
			fmt.Fprintln(os.Stderr, "Usage: detector [run|version]")
			os.Exit(1)
		}
	}

	log := newLogger(os.Getenv("DETECTOR_LOG_LEVEL"))
	if err := run(log); err != nil {
		log.Error().Msgf("%v", err)
		os.Exit(1)
	}
}

func run(log zerolog.Logger) error {
	host := os.Getenv("DETECTOR_HOST")
	if host == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			host = h
		} else {
			host = "unknown-host"
		}
	}

	root, err := projectFile(os.Getenv("DETECTOR_COMPOSE_SUBDIR"))
	if err != nil {
		return err
	}

	cutoff, source, err := loadCutoff(log)
	if err != nil {
		return err
	}
	log.Info().Msgf("looking for tags published since %s (%s)",
		cutoff.Local().Format("2006-01-02 15:04:05 MST"), source)

	services, err := readServices(root, log)
	if err != nil {
		return err
	}
	log.Info().Msgf("checking %d service(s) in %s", len(services), root)

	reg := detect.Registry{
		// The dated listing, so Docker Hub images cost one request per page
		// rather than one per tag. Everywhere else the dates come back zero
		// and TagCreated is asked per surviving tag.
		ListTags: func(image string) ([]detect.DatedTag, error) {
			dated, err := registry.ListTagsWithDates(image)
			if err != nil {
				return nil, err
			}
			out := make([]detect.DatedTag, 0, len(dated))
			for _, d := range dated {
				out = append(out, detect.DatedTag{Name: d.Name, Published: d.Published})
			}
			return out, nil
		},
		TagCreated: registry.TagCreated,
	}

	// startedAt, not time.Now() at the end: anything published while the
	// check was running would otherwise fall in the gap between the cutoff
	// it was compared against and the cutoff recorded afterwards, and never
	// be reported at all.
	startedAt := time.Now()

	reporter := newReporter(os.Getenv("DETECTOR_WEBHOOK_URL"), host, log)
	found, failed := checkAll(services, cutoff, reg, reporter.report, log)

	log.Info().Msgf("check complete: %d new tag(s) across %d service(s), %d could not be checked",
		found, len(services), failed)

	if !mayAdvance(failed) {
		log.Warn().Msgf("not advancing the cutoff: %d service(s) could not be checked, "+
			"so moving it would silently skip anything they published", failed)
		return nil
	}
	if err := saveCutoff(startedAt); err != nil {
		return fmt.Errorf("recording when this check ran: %w", err)
	}
	log.Info().Msgf("cutoff advanced to %s", startedAt.Local().Format("2006-01-02 15:04:05 MST"))
	return nil
}

// mayAdvance says whether a run earned the right to move the cutoff forward.
//
// Only a complete one does. A run with holes in it must not move the line,
// or whatever was published by the services it could not reach falls between
// the old cutoff and the new one and is never reported -- silently, and
// forever. That is the one thing a detector must never do, which is why the
// rule is a named function rather than an `if` buried in a loop: it is the
// most important sentence in this program and deserves somewhere to be
// tested.
//
// The cost of being wrong the other way is one noisy run. The asymmetry is
// the whole argument.
func mayAdvance(failed int) bool { return failed == 0 }

// readServices turns the compose project into what the detector looks at.
//
// Pin status is the opt-in, as everywhere else in this codebase: an unpinned
// service has made no versioning decision, so there is nothing to watch over.
func readServices(root string, log zerolog.Logger) ([]detect.Service, error) {
	index, err := compose.ContainerIndex(root)
	if err != nil {
		return nil, fmt.Errorf("reading the compose project at %s: %w", root, err)
	}

	var out []detect.Service
	for container, ref := range index {
		raw, err := compose.RawImage(ref.File, ref.Service)
		if err != nil {
			log.Warn().Msgf("%s: could not read its image — %v", ref.Service, err)
			continue
		}
		if !strings.Contains(raw, "@sha256:") {
			log.Debug().Msgf("%s: not pinned, so it made no versioning decision to watch over", ref.Service)
			continue
		}
		if compose.HasUnexpandedVariable(raw) {
			log.Warn().Msgf("%s: its image has an unexpanded variable, so it cannot be reasoned about: %s",
				ref.Service, raw)
			continue
		}
		base, tag, err := compose.ParseImage(ref.File, ref.Service)
		if err != nil {
			log.Warn().Msgf("%s: could not parse its image — %v", ref.Service, err)
			continue
		}

		include, exclude, err := tagRules(ref.File, ref.Service)
		if err != nil {
			log.Warn().Msgf("%s: %v", ref.Service, err)
			continue
		}

		out = append(out, detect.Service{
			Name:      ref.Service,
			Container: container,
			Image:     base,
			Tag:       tag,
			Include:   include,
			Exclude:   exclude,
		})
	}
	return out, nil
}

// projectFile is the compose file to read, honouring the subdirectory.
func projectFile(sub string) (string, error) {
	dir := composeDir
	if sub != "" {
		if filepath.IsAbs(sub) {
			return "", fmt.Errorf("DETECTOR_COMPOSE_SUBDIR must be relative to %s, got %q", composeDir, sub)
		}
		joined := filepath.Join(composeDir, sub)
		// Join cleans ".." away rather than erroring, so a value trying to
		// escape would otherwise resolve quietly to somewhere else.
		rel, err := filepath.Rel(composeDir, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("DETECTOR_COMPOSE_SUBDIR %q escapes %s", sub, composeDir)
		}
		dir = joined
	}
	return compose.FindFile(dir)
}
