package main

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/homebrew-docker-pin/internal/detect"
)

// checkAll checks every service, reporting each finding as it is discovered.
//
// Split out from run so a test can drive the whole cycle -- read a cutoff,
// check services against a registry that behaves however the test says,
// decide whether to advance -- without a network or a compose file. The rule
// worth testing is not what this function returns; it is what the cutoff does
// afterwards, and that only means anything end to end.
func checkAll(
	services []detect.Service,
	cutoff time.Time,
	reg detect.Registry,
	report func(detect.Finding),
	log zerolog.Logger,
) (found, failed int) {
	for _, svc := range services {
		// Said before the work, not after. A service with many tags takes
		// tens of seconds -- one request per candidate where the registry
		// does not date its listing -- and a run that printed nothing until
		// it found something was indistinguishable from one that had hung.
		log.Debug().Msgf("checking %s (%s)", svc.Name, svc.Image)
		began := time.Now()

		before := found
		err := detect.Since(svc, cutoff, reg, func(f detect.Finding) {
			found++
			// Printed as it is discovered, not after the service finishes.
			// Info, not warn: a published tag is the ordinary output of this
			// tool, not something wrong -- whether it matters is the gate's
			// call, and warning about every one would make the level mean
			// "the detector worked".
			//
			// And nothing about what the service is on. Saying "currently on
			// dev" alongside 2.24.0 implies the one could replace the other,
			// which is a version judgement this tool has no basis for.
			log.Info().Msgf("%s: %s was published %s",
				f.Service, f.Tag, f.Published.Local().Format("2006-01-02 15:04:05 MST"))
			report(f)
		})

		if d := time.Since(began); d > slowCheck {
			log.Warn().Msgf("%s took %s to check — it has many tags, and each one costs a request",
				svc.Name, d.Round(time.Second))
		}
		if err != nil {
			// Counted, not swallowed. This number is the whole input to
			// whether the cutoff may move, so a failure that did not
			// increment it would be a service silently skipped forever.
			failed++
			log.Error().Msgf("%s: could not check it — %v", svc.Name, err)
			continue
		}
		if found == before {
			log.Debug().Msgf("%s: nothing published since the last check", svc.Name)
		}
	}
	return found, failed
}
