package main

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/duva-v4/internal/detect"
)

// checkAll checks every service against its own cutoff, handing findings on
// as they are discovered and holding whatever did not land.
//
// Two things happen per service and they are independent. A service checked
// without error moves its cutoff, whether or not anything was published --
// looking is not publishing. A finding that could not be handed on is held,
// and the next run that can reach a webhook sends it.
//
// The rule that matters, and the one thing a detector must never get wrong:
// **a service's cutoff never moves past something that service published and
// nothing else heard about.** A line moved over a hole means whatever fell in
// it is older than the cutoff forever, silently. That is why a failed check
// leaves the line alone.
//
// It used to be a whole-run rule: any failure anywhere held the single line
// for everything. That was wrong in a way that took a broken service to show
// -- one private image the detector had no credentials for pinned the line for
// all forty-eight, so every run re-scanned a week and re-reported the same
// findings, and the state file was never written once. The property is per
// service because the failure is.
//
// A publish failure does not hold any line at all. It holds the *finding*,
// which is a different thing and is what makes running with no webhook
// configured a legitimate way to run rather than a way to lose work.
//
// Split out from run so a test can drive the whole cycle -- read the memory,
// check services against a registry that behaves however the test says, see
// what moved and what was held -- without a network or a compose file.
func checkAll(
	services []detect.Service,
	mem *memory,
	reg detect.Registry,
	hand func(detect.Finding) bool,
	log zerolog.Logger,
) (found, failed int) {
	for _, svc := range services {
		// Stamped before the work: anything published while this service was
		// being checked would otherwise fall between the cutoff it was
		// compared against and the cutoff recorded afterwards.
		startedAt := time.Now()

		// Said before the work, not after. A service with many tags takes
		// tens of seconds -- one request per candidate where the registry
		// does not date its listing -- and a run that printed nothing until
		// it found something was indistinguishable from one that had hung.
		log.Debug().Msgf("checking %s (%s)", svc.Name, svc.Image)
		began := time.Now()

		before := found
		err := detect.Since(svc, mem.Cutoff(svc.Name), reg, func(f detect.Finding) {
			found++
			// Printed as it is discovered, not after the service finishes.
			// Info, not warn: a published tag is the ordinary output of this
			// tool, not something wrong -- whether it matters is the gate's
			// call, and warning about every one would make the level mean
			// "the detector worked".
			//
			// Two kinds of finding read differently. A new tag has a publish
			// date; a moving tag has moved, and has no date the registry
			// gives cheaply -- printing a zero time there says
			// "0001-01-01 00:50:20 LMT", which is worse than saying nothing.
			if f.Digest != "" {
				log.Info().Msgf("%s: %s now points at %s", f.Service, f.Tag, shortDigest(f.Digest))
			} else {
				log.Info().Msgf("%s: %s was published %s",
					f.Service, f.Tag, f.Published.Local().Format("2006-01-02 15:04:05 MST"))
			}
			// Held or dropped, but never merely lost. A finding that did not
			// land waits in the memory until something can take it, which is
			// what makes running with no webhook a legitimate way to run
			// rather than a way to throw findings away.
			if hand(f) {
				mem.Handed(f)
			} else {
				mem.Hold(f)
			}
		})

		if d := time.Since(began); d > slowCheck {
			log.Warn().Msgf("%s took %s to check — it has many tags, and each one costs a request",
				svc.Name, d.Round(time.Second))
		}
		if err != nil {
			// Counted, and its cutoff does not move: a service that could not
			// be checked must be asked the same question again, or whatever
			// it published in the meantime falls in the gap and is never
			// reported. Only this service's line is held.
			failed++
			log.Error().Msgf("%s: could not check it — %v", svc.Name, err)
			continue
		}
		if found == before {
			log.Debug().Msgf("%s: nothing published since the last check", svc.Name)
		}
		mem.Checked(svc.Name, startedAt)
	}
	return found, failed
}

// handHeld tries to hand on what earlier runs could not.
//
// Before the check rather than after: a webhook that has come back should hear
// about the backlog first, in the order it was found, rather than after a run
// that may take minutes.
func handHeld(mem *memory, hand func(detect.Finding) bool, log zerolog.Logger) {
	held := mem.Held()
	if len(held) == 0 {
		return
	}
	log.Info().Msgf("%d finding(s) were held from an earlier run; handing them on first", len(held))

	var landed int
	for _, f := range held {
		if hand(f) {
			mem.Handed(f)
			landed++
		}
	}
	switch {
	case landed == len(held):
		log.Info().Msgf("all %d are now handed on", landed)
	case landed > 0:
		log.Warn().Msgf("%d of %d handed on; the rest are still held", landed, len(held))
	default:
		log.Warn().Msgf("none could be handed on; all %d are still held", len(held))
	}
}

// shortDigest is the first twelve hex characters of a digest.
//
// Seventy-one characters of hex in a log line is nobody's idea of readable,
// and twelve is enough to match against a `docker images` listing or another
// line in the same run.
func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}
