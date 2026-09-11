package main

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// The UI's log is meant to be read by a person looking at `docker logs`
// because the page did not show what they expected, not parsed by a machine.
// So: one line per event, a complete sentence, and the consequence stated
// rather than implied.
//
// The format is zerolog's ConsoleWriter, "TIME | LEVEL | message", matching
// duva, diun and the other tools this runs alongside.

// newLogger builds the process-wide logger. Level comes from
// DUVA_UI_LOG_LEVEL, so verbosity can be turned up on a running host without
// a redeploy -- which is how an unreachable decider gets diagnosed.
func newLogger(levelStr string) zerolog.Logger {
	// ParseLevel("") returns NoLevel with no error, which as a threshold
	// filters everything -- so an unset level would silence the detector
	// completely. The empty case has to be caught before parsing, not after.
	level := zerolog.InfoLevel
	if levelStr != "" {
		parsed, err := zerolog.ParseLevel(strings.ToLower(levelStr))
		if err == nil {
			level = parsed
		}
	}

	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
		// NoColor left false deliberately: stdout is a pipe in a container,
		// but `docker logs` and the viewers used against this stack render
		// ANSI colour fine.
	}
	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}
