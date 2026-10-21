// watcher finds tags that appeared since it last looked.
//
// It reads the compose project, asks each service's registry which tags
// exist, keeps the ones published since the last check, and hands them to the
// queue. It knows nothing about versions: not semver, not calver, not whether
// 4.39.25 beats 4.39.20. Deciding what a tag means belongs to the gate, and a
// watcher that also understood versions would be a second thing that had to
// agree with the gate about ordering.
//
// Its state is two things per service: how far back to look, and what it
// found and could not hand on yet. Losing that costs a noisy run, not a
// rebuild -- which is why it is a cutoff and an outbox rather than a baseline
// of what each image pointed at.
//
//	/data      per-service cutoffs, and findings not yet handed on
//
// Configuration, through internal/settings (DUVA_SETTING_<KEY>):
//
//	schedule   a 5-field cron expression
//	since      override the cutoff for one run (RFC 3339, or a duration
//	           like 168h meaning "the last week")
//	log_level  trace/debug/info/warn/error
package watcher

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
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
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/settings"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/store"
	"github.com/Miista/homebrew-docker-pin/duva-v5/internal/watch"
	"github.com/Miista/homebrew-docker-pin/internal/croncal"
	"github.com/Miista/homebrew-docker-pin/oci/registry"
	ociversion "github.com/Miista/homebrew-docker-pin/oci/version"
)

var Version = "dev"

var (
	composeDir = "/compose"
	// memStore is where the watcher keeps what it checked: duva's database,
	// set from Options.Store.
	memStore *store.Store
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
func scheduleFromEnv() (string, error) {
	return settings.String(settings.Schedule, defaultSchedule)
}

const defaultSchedule = "0 3 * * *"

// Options is what a caller in the same process supplies instead of the
// environment.
type Options struct {
	// Root is the compose file. Empty means the one mounted at /compose.
	Root string
	// Report hands one finding to the queue, reporting whether it landed. A
	// finding that did not land is held and offered again next run.
	Report func(watch.Finding) bool
	// Labels reads a service's tag rules from its container, by
	// container_name. Nil reads them from the compose file.
	Labels func(container string) (map[string]string, bool, error)
	// Store is where the cutoffs and held findings are kept.
	Store *store.Store
}

// Serve checks once now and then on the schedule setting, until ctx is done.
func Serve(ctx context.Context, log zerolog.Logger, opts Options) error {
	expr, err := scheduleFromEnv()
	if err != nil {
		return err
	}
	// Validated before the first check rather than at the first tick: a
	// schedule that does not parse should stop this now, not in a day, with
	// nothing in the log to say why nothing happened.
	if _, err := croncal.Next(expr, time.Now()); err != nil {
		return fmt.Errorf("%s %q: %w", settings.EnvVar(settings.Schedule), expr, err)
	}
	log.Info().Msgf("checking on schedule %q", expr)

	// Once at startup, rather than waiting for the first tick. A daily
	// schedule would otherwise leave a freshly deployed watcher silent for
	// up to a day, and it is what makes restarting the container a way to
	// ask for a check now.
	if err := run(log, "", 0, opts); err != nil {
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
			if err := run(log, "", 0, opts); err != nil {
				// A failed check is not a reason to stop checking: the next
				// one may find the registry back.
				log.Error().Msgf("%v", err)
			}
		}
	}
}

// runMu serialises checks: the scheduled one, and any a person asks for.
// Each loads the memory, checks, and saves it whole, so two at once would
// each overwrite what the other learned.
var runMu sync.Mutex

// Check checks now, what a person's "Check…" asks for: one service by its
// compose or container name, or every one for an empty service.
//
// back is how far back to look. Zero is since each service's last check, and
// finds only what is new. More looks back at least that far, which is how a
// release an earlier check passed over is found again -- what forgetting a
// cutoff used to be, as a question rather than as deleting state. What is
// found again is offered again, except what is blocked.
func Check(log zerolog.Logger, service string, back time.Duration, opts Options) error {
	return run(log, service, back, opts)
}

// watching is the services the last full check looked at, by compose name,
// sorted: exactly what a check of every service covers now.
var (
	watchingMu sync.Mutex
	watching   []string
)

// Watched is every service the watcher checks, by compose name, sorted: what
// a person may pick to check again. The last full check's list, not the
// cutoffs: a service unpinned, or retired from the compose file, keeps its
// cutoff, and picking it would ask for a check of something not watched.
// Until the first full check after a start, the cutoffs are all there is.
func Watched(st *store.Store) []string {
	watchingMu.Lock()
	known := watching
	watchingMu.Unlock()
	if known != nil {
		return known
	}
	return checked(st)
}

// checked is every service with a recorded cutoff.
func checked(st *store.Store) []string {
	rows, err := st.DB().Query(`SELECT service FROM watch_cutoffs ORDER BY service`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			out = append(out, name)
		}
	}
	return out
}

// LastChecked is when this host last looked for new tags: the latest cutoff
// recorded, which each service's check stamps as it starts. Zero when nothing
// has been checked yet.
func LastChecked(st *store.Store) time.Time {
	var raw sql.NullString
	if err := st.DB().QueryRow(`SELECT MAX(cutoff) FROM watch_cutoffs`).Scan(&raw); err != nil || !raw.Valid {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return time.Time{}
	}
	return at
}

func run(log zerolog.Logger, only string, back time.Duration, opts Options) error {
	runMu.Lock()
	defer runMu.Unlock()

	root := opts.Root
	if root == "" {
		var err error
		if root, err = projectFile(); err != nil {
			return err
		}
	}

	if opts.Store != nil {
		memStore = opts.Store
	}
	mem, err := loadMemory(log, back)
	if err != nil {
		return err
	}
	log.Info().Msgf("looking for tags published since each service's last check (%s)", mem.why)

	services, err := readServices(root, log, opts.Labels)
	if err != nil {
		return err
	}
	if only == "" {
		names := make([]string, 0, len(services))
		for _, svc := range services {
			names = append(names, svc.Name)
		}
		sort.Strings(names)
		watchingMu.Lock()
		watching = names
		watchingMu.Unlock()
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

	report := opts.Report
	if report == nil {
		return fmt.Errorf("nothing to hand findings to")
	}

	// The backlog first: the queue should hear what earlier runs could not
	// hand on before it hears anything new.
	handHeld(mem, report, log)

	found, failed := checkAll(services, mem, reg, report, log)

	log.Info().Msgf("check complete: %d new tag(s) across %d service(s), %d could not be checked",
		found, len(services), failed)

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
func readServices(root string, log zerolog.Logger, labels func(string) (map[string]string, bool, error)) ([]watch.Service, error) {
	index, err := compose.ContainerIndex(root)
	if err != nil {
		return nil, fmt.Errorf("reading the compose project at %s: %w", root, err)
	}

	// Said once, with the count: a service with no container_name is not
	// indexed at all, so it never reaches the loop below and none of its
	// reasons are logged. On a host that is a service silently unwatched for
	// as long as nobody looks -- which is the failure this tool exists to
	// prevent, arriving as silence.
	if all, err := compose.ListServices(root); err == nil && len(all) > len(index) {
		log.Warn().Msgf("%d of %d service(s) declare no container_name, so they are not watched: "+
			"duva replaces a container by name and cannot act on one it cannot name",
			len(all)-len(index), len(all))
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

		var include, exclude *regexp.Regexp
		if labels == nil {
			include, exclude, err = tagRules(ref.File, ref.Service)
		} else {
			include, exclude, err = containerRules(labels, container)
		}
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

// projectFile is the compose file to read, which must be at the root of what
// is mounted.
//
// No subdirectory, and no walking. This stage reads compose files and touches
// no git, so it has no reason to see the repository around them -- the project
// is what is mounted, and if it is not there that is a misconfiguration to say
// out loud. FindFile would climb out of the mount looking for one and answer
// with whatever it found, which is a wrong answer wearing the shape of a right
// one.
func projectFile() (string, error) {
	return compose.FileIn(composeDir)
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

// containerRules reads a service's tag rules from its container.
//
// A service with no container is not watched: there is nothing running to
// update, and the rules in the file describe a container that does not exist.
func containerRules(labels func(string) (map[string]string, bool, error), container string) (include, exclude *regexp.Regexp, err error) {
	set, found, err := labels(container)
	if err != nil {
		return nil, nil, fmt.Errorf("reading its container's labels: %w", err)
	}
	if !found {
		return nil, nil, fmt.Errorf("no container named %s, so nothing to update", container)
	}
	return rulesFrom(set)
}
