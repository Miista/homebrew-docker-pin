package main

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// duva's log is meant to be read by a person looking at `docker logs duva`
// after something happened, not parsed by a machine. So: one line per event,
// a complete sentence, and the consequence stated rather than implied.
//
// "app: not pinned, skipping" was none of those. It assumed the reader knew
// that pinned means a digest in the image line, that duva watches only pinned
// services, and that skipping meant this one was left alone -- three pieces
// of context, none of them in the sentence.
//
// The format is zerolog's ConsoleWriter, "TIME | LEVEL | message", matching
// diun and the other tools this tends to run alongside.
//
// The timestamp carries the date. These processes are long-lived and check on
// a daily schedule, so a bare clock time cannot tell yesterday's run from
// today's -- and `docker logs` on a container up for a week is exactly where
// that question gets asked.

// newLogger builds the process-wide logger. Level comes from DUVA_LOG_LEVEL,
// so verbosity can be turned up on a running host without a redeploy -- which
// matters for a tool that rewrites files and replaces containers.
func newLogger(levelStr string) zerolog.Logger {
	// ParseLevel("") returns NoLevel with no error, which as a threshold
	// filters everything -- so an unset DUVA_LOG_LEVEL would silence duva
	// completely. The empty case has to be caught before parsing, not after.
	level := zerolog.InfoLevel
	if levelStr != "" {
		parsed, err := zerolog.ParseLevel(strings.ToLower(levelStr))
		if err != nil {
			// A typo should not quietly change how much duva tells you.
			os.Stderr.WriteString("duva: DUVA_LOG_LEVEL=" + levelStr +
				" is not a level (debug, info, warn, error); using info\n")
		} else {
			level = parsed
		}
	}

	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "2006-01-02 15:04:05",
		// Colours are left on even though stdout in a container is a pipe
		// rather than a terminal: `docker logs` and the viewers actually used
		// against these stacks -- Dozzle, Portainer -- render ANSI fine.
	}

	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}

// Call sites build the whole sentence and pass it as the message; fields are
// not how information is conveyed here. The level carries the rest: Info for
// what duva did, Warn for what it declined to do, Error for what failed.
