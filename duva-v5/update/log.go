package main

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// The updater's log is meant to be read by a person looking at `docker logs`
// after a check ran, not parsed by a machine. So: one line per event, a
// complete sentence, and the consequence stated rather than implied.
//
// The format is zerolog's ConsoleWriter, "TIME | LEVEL | message", matching
// duva, diun and the other tools this runs alongside.
//
// The timestamp carries the date. This process is long-lived and acts rarely,
// so a bare clock time cannot tell which day an update was applied -- and the
// log of what this did to a host is the record that matters most here.

// newLogger builds the process-wide logger. Level comes from
// DUVA_LOG_LEVEL, so verbosity can be turned up on a running host without
// a redeploy -- which matters for a tool that rewrites files and replaces containers.
func newLogger(levelStr string) zerolog.Logger {
	// ParseLevel("") returns NoLevel with no error, which as a threshold
	// filters everything -- so an unset level would silence the watcher
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
		TimeFormat: "2006-01-02 15:04:05",
		// NoColor left false deliberately: stdout is a pipe in a container,
		// but `docker logs` and the viewers used against this stack render
		// ANSI colour fine.
	}
	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}
