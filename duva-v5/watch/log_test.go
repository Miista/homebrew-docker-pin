package main

import (
	"io"
	"os"
	"regexp"
	"testing"
)

// The date is the point. A watcher that checks once a day writes lines that
// are read days later, and a bare clock time cannot say which run they came
// from -- which is the whole reason this format is not zerolog's default.
func TestLogLinesCarryTheDate(t *testing.T) {
	line := captureLog(t, func() {
		log := newLogger("info")
		log.Info().Msg("checked everything")
	})

	if !regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`).MatchString(line) {
		t.Errorf("a log line carries no date: %q", line)
	}
	if !regexp.MustCompile(`checked everything`).MatchString(line) {
		t.Errorf("the message did not survive: %q", line)
	}
}

// The negative: a level below the threshold writes nothing at all. Without
// this, a logger that silently dropped every line would still pass the test
// above by never being reached.
func TestLogRespectsTheLevel(t *testing.T) {
	line := captureLog(t, func() {
		log := newLogger("error")
		log.Info().Msg("checked everything")
	})
	if line != "" {
		t.Errorf("an info line was written at error level: %q", line)
	}
}

// captureLog runs fn with os.Stdout replaced, returning what was written.
// The writer is built inside fn rather than passed in, because newLogger
// resolves os.Stdout at call time and that is the behaviour under test.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	real := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = real }()

	fn()
	w.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the captured log: %v", err)
	}
	return string(out)
}
