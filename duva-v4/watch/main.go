// watcher finds tags that appeared since it last looked.
//
// It reads the compose project, asks each service's registry which tags
// exist, keeps the ones published since the last check, and publishes them to
// a webhook. It
// knows nothing about versions: not semver, not calver, not whether 4.39.25
// beats 4.39.20. Deciding what a tag means belongs to the gate, and a
// watcher that also understood versions would be a second thing that had to
// agree with the gate about ordering.
//
// It does not know what is subscribed to its webhook, either. A gate, a
// notifier, a script that appends to a file -- it publishes what it observed
// in its own shape, and what that means is the subscriber's business.
//
// It holds no docker socket. Its state is two things per service: how far
// back to look, and what it found and has not handed on yet. Losing that
// costs a noisy run, not a rebuild -- which is why it is a cutoff and an
// outbox rather than a baseline of what each image pointed at.

//	/compose   the compose project directory, read-only
//	/data      per-service cutoffs, and findings not yet handed on
//
// Subcommands:
//
//	run [service]  check everything, or just the named service (its service
//	               name or its container name). A schedule always checks
//	               everything; narrowing is for asking about one thing.
//	serve          check on DUVA_WATCH_SCHEDULE
//	health         exit 0 if the webhook is reachable
//
// Configuration:
//
//	DUVA_WATCH_HOST            what this watcher calls itself
//	DUVA_WATCH_COMPOSE_SUBDIR  where the project lives within /compose
//	DUVA_WATCH_WEBHOOK_URL     where to publish findings; empty means log only
//	DUVA_WATCH_SCHEDULE        a 5-field cron expression, for `serve`
//	DUVA_WATCH_SINCE           override the cutoff for one run (RFC 3339, or a
//	                         duration like 168h meaning "the last week")
//	DUVA_WATCH_LOG_LEVEL       trace/debug/info/warn/error
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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
	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/watch"
	"github.com/Miista/homebrew-docker-pin/internal/croncal"
	"github.com/Miista/homebrew-docker-pin/oci/registry"
	ociversion "github.com/Miista/homebrew-docker-pin/oci/version"
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

// schedule is a 5-field cron expression, parsed by internal/croncal -- the
// same parser duva uses, and for the same reason: it rejects restricting both
// day-of-month and day-of-week, because cron ORs those and systemd ANDs them,
// and a schedule that means two different things depending on who reads it is
// not a schedule.
//
// Unset means daily at 03:00. A default rather than a refusal because a
// watcher with no schedule is not a configuration anyone wants: the choice
// is between checking daily and checking never, and never is not a sensible
// thing to arrive at by omission.
//
// Daily because that is how often these registries actually publish. Hourly
// would multiply the request count by twenty-four to learn the same thing.
func scheduleFromEnv() string {
	if s := os.Getenv("DUVA_WATCH_SCHEDULE"); s != "" {
		return s
	}
	return defaultSchedule
}

const defaultSchedule = "0 3 * * *"

func main() {
	mode := "run"
	// A service named after run narrows that run to it. Nothing is recorded
	// differently: the cutoff each service carries is its own, so checking one
	// moves only that one, exactly as a full run would have.
	var only string
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("watch", version)
			return
		case "run", "serve":
			mode = os.Args[1]
			if mode == "run" && len(os.Args) > 2 {
				only = os.Args[2]
			}
		case "health":
			// For HEALTHCHECK. The watcher serves no HTTP of its own, so
			// health is a subcommand the image can exec rather than a port
			// something can poll -- which also means it needs no shell and no
			// curl, neither of which a distroless image carries.
			//
			// What it reports is whether the webhook can be reached, because
			// that is the one thing that is both outside this process and
			// fatal to its purpose: a watcher that cannot publish is doing
			// all the work and dropping the result. Between daily runs there
			// is nothing else to be wrong.
			os.Exit(health())
		default:
			fmt.Fprintln(os.Stderr, "Usage: detector [run [service]|serve|health|version]")
			os.Exit(1)
		}
	}

	log := newLogger(os.Getenv("DUVA_WATCH_LOG_LEVEL"))

	var err error
	if mode == "serve" {
		err = serve(log)
	} else {
		err = run(log, only)
	}
	if err != nil {
		log.Error().Msgf("%v", err)
		os.Exit(1)
	}
}

// health is the exit code for the health subcommand: 0 healthy, 1 not.
//
// A watcher with no webhook configured is healthy. That is a real way to run
// it -- findings go to the log and nowhere else -- and reporting a deliberate
// configuration as unhealthy would make the signal useless for everyone who
// runs it that way.
func health() int {
	url := os.Getenv("DUVA_WATCH_WEBHOOK_URL")
	if url == "" {
		return 0
	}
	if err := dialWebhook(url); err != nil {
		fmt.Fprintf(os.Stderr, "the webhook at %s cannot be reached: %v\n", url, err)
		return 1
	}
	return 0
}

// serve checks on a schedule, until told to stop.
//
// `run` stays the whole of the program: serve is a loop around it, so a
// scheduled check and a manual one do exactly the same thing. A separate
// scheduled path would be a second implementation to keep in agreement with
// the first.
func serve(log zerolog.Logger) error {
	expr := scheduleFromEnv()
	// Validated before the first check rather than at the first tick: a
	// schedule that does not parse should stop this now, not in a day, with
	// nothing in the log to say why nothing happened.
	if _, err := croncal.Next(expr, time.Now()); err != nil {
		return fmt.Errorf("DUVA_WATCH_SCHEDULE %q: %w", expr, err)
	}
	log.Info().Msgf("checking on schedule %q", expr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Once at startup, rather than waiting for the first tick. A daily
	// schedule would otherwise leave a freshly deployed watcher silent for
	// up to a day, and it is what makes restarting the container a way to
	// ask for a check now.
	if err := run(log, ""); err != nil {
		log.Error().Msgf("%v", err)
	}

	for {
		next, err := croncal.Next(expr, time.Now())
		if err != nil {
			return err
		}
		wait := time.Until(next)
		// Local, with the zone named: an operator reading this wants to know
		// when it fires in their own time.
		log.Info().Msgf("next check at %s, in %s",
			next.Local().Format("2006-01-02 15:04:05 MST"), wait.Round(time.Second))

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Info().Msg("shutting down")
			return nil
		case <-timer.C:
			if err := run(log, ""); err != nil {
				// A failed check is not a reason to stop checking: the next
				// one may find the registry back.
				log.Error().Msgf("%v", err)
			}
		}
	}
}

func run(log zerolog.Logger, only string) error {
	host := os.Getenv("DUVA_WATCH_HOST")
	if host == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			host = h
		} else {
			host = "unknown-host"
		}
	}

	root, err := projectFile(os.Getenv("DUVA_WATCH_COMPOSE_SUBDIR"))
	if err != nil {
		return err
	}

	mem, err := loadMemory(log)
	if err != nil {
		return err
	}
	log.Info().Msgf("looking for tags published since each service's last check (%s)", mem.why)

	services, err := readServices(root, log)
	if err != nil {
		return err
	}
	if only != "" {
		services = justOne(services, only)
		if len(services) == 0 {
			return fmt.Errorf("%s names no service the detector watches in %s: "+
				"it is absent, unpinned, or its image cannot be read", only, root)
		}
	}
	log.Info().Msgf("checking %d service(s) in %s", len(services), root)

	reg := watch.Registry{
		// The dated listing, so Docker Hub images cost one request per page
		// rather than one per tag. Everywhere else the dates come back zero
		// and TagCreated is asked per surviving tag.
		ListTags: func(image string) ([]watch.DatedTag, error) {
			dated, err := registry.ListTagsWithDates(image)
			if err != nil {
				return nil, err
			}
			out := make([]watch.DatedTag, 0, len(dated))
			for _, d := range dated {
				out = append(out, watch.DatedTag{Name: d.Name, Published: d.Published})
			}
			return out, nil
		},
		TagCreated: registry.TagCreated,
		// For a service following a moving tag: what does it point at now.
		// One request, and the only one such a service needs.
		TagDigest: registry.RemoteDigest,
	}

	reporter := newReporter(os.Getenv("DUVA_WATCH_WEBHOOK_URL"), host, log)

	// The backlog first: a webhook that has come back should hear what earlier
	// runs could not hand on before it hears anything new.
	handHeld(mem, reporter.report, log)

	found, failed := checkAll(services, mem, reg, reporter.report, log)

	log.Info().Msgf("check complete: %d new tag(s) across %d service(s), %d could not be checked",
		found, len(services), failed)
	reporter.summarise()

	// Saved whatever happened. Each service's cutoff moved only if that
	// service was checked, and anything that could not be handed on is held --
	// so there is no run whose result is not worth recording. That is the
	// whole of the change from a single line that a lone failure could pin in
	// the past forever.
	if err := mem.save(); err != nil {
		return fmt.Errorf("recording what this check learned: %w", err)
	}
	if held := len(mem.Held()); held > 0 {
		log.Warn().Msgf("%d finding(s) are held for the next run that can hand them on", held)
	}
	return nil
}

// readServices turns the compose project into what the watcher looks at.
//
// Pin status is the opt-in, as everywhere else in this codebase: an unpinned
// service has made no versioning decision, so there is nothing to watch over.
func readServices(root string, log zerolog.Logger) ([]watch.Service, error) {
	index, err := compose.ContainerIndex(root)
	if err != nil {
		return nil, fmt.Errorf("reading the compose project at %s: %w", root, err)
	}

	var out []watch.Service
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

		// Whether the tag names a release or a stream decides what an update
		// to this service even means, so it is settled here -- where the
		// version knowledge lives -- rather than inside detect, which has
		// none and should keep none.
		moving := !ociversion.IsVersion(tag)
		if moving {
			log.Debug().Msgf("%s: follows the moving tag %q, so it is watched by digest", ref.Service, tag)
		}

		out = append(out, watch.Service{
			Name:      ref.Service,
			Container: container,
			Image:     base,
			Tag:       tag,
			Include:   include,
			Exclude:   exclude,
			Moving:    moving,
			Digest:    digestOf(raw),
		})
	}
	return out, nil
}

// projectFile is the compose file to read, honouring the subdirectory.
func projectFile(sub string) (string, error) {
	dir := composeDir
	if sub != "" {
		if filepath.IsAbs(sub) {
			return "", fmt.Errorf("DUVA_WATCH_COMPOSE_SUBDIR must be relative to %s, got %q", composeDir, sub)
		}
		joined := filepath.Join(composeDir, sub)
		// Join cleans ".." away rather than erroring, so a value trying to
		// escape would otherwise resolve quietly to somewhere else.
		rel, err := filepath.Rel(composeDir, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("DUVA_WATCH_COMPOSE_SUBDIR %q escapes %s", sub, composeDir)
		}
		dir = joined
	}
	return compose.FindFile(dir)
}

// digestOf is the @sha256:... a pinned image reference records.
//
// Read from the raw line rather than resolved from a registry: it is what the
// file says this service is on, which is the thing a moving tag's current
// digest is compared against.
func digestOf(raw string) string {
	if i := strings.Index(raw, "@"); i >= 0 {
		return raw[i+1:]
	}
	return ""
}

// justOne narrows a run to a single service.
//
// It matches the container name as well as the service name because those
// differ in this project more often than not, and the name a person has to
// hand is whatever docker ps showed them.
func justOne(services []watch.Service, name string) []watch.Service {
	for _, s := range services {
		if s.Name == name || s.Container == name {
			return []watch.Service{s}
		}
	}
	return nil
}
