package integration

import (
	"strings"
	"time"
)

// Waiting for the world to settle.
//
// Almost every flaky integration test is a test that looked before the thing
// it was watching had happened. In shell that was a sleep, or a poll with a
// wall-clock window guessing which log lines belonged to which run -- both
// wrong often enough to matter, and silent when they were.
//
// These wait for a condition and say what they last saw when it never came
// true, so a timeout is a diagnosis rather than a mystery.

// waitFor blocks until cond returns true, or fails the test after timeout
// with what describe last reported.
func (s *Scenario) waitFor(what string, timeout time.Duration, cond func() bool, describe func() string) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("timed out after %s waiting for %s\nlast saw:\n%s",
		timeout, what, indent(describe()))
}

func indent(s string) string {
	if s == "" {
		return "    <nothing>"
	}
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

// CheckNow makes duva check, and returns when that check has finished.
//
// duva runs serve, which is what ships: up permanently, checking on a
// schedule and once at startup. Restarting it is how a test asks for a check
// now rather than waiting for a cron tick.
//
// Which check finished is counted, not timed. duva has already checked once,
// when the scenario came up and before the test changed anything, and a
// wall-clock window is a guess about which run a line belongs to -- a guess
// that was wrong often enough to make this suite flaky.
func (s *Scenario) CheckNow() {
	s.t.Helper()
	s.CheckNowIn("duva")
}

// CheckNowIn is CheckNow for a scenario running more than one duva.
func (s *Scenario) CheckNowIn(service string) {
	s.t.Helper()
	before := s.checksDone(service)

	if out, err := s.compose("restart", service); err != nil {
		s.t.Fatalf("restarting %s: %v\n%s", service, err, out)
	}

	s.waitFor("a check by "+service, 60*time.Second,
		func() bool { return s.checksDone(service) > before },
		func() string { return s.Logs(service) })
}

// checksDone is how many checks a duva has announced. It prints when it goes
// back to sleep, so the count rises once per completed check.
func (s *Scenario) checksDone(service string) int {
	return strings.Count(s.Logs(service), "next check at")
}

// WaitForLine returns the first log line from a service containing want,
// waiting for it to appear.
func (s *Scenario) WaitForLine(service, want string) string {
	s.t.Helper()
	var found string
	s.waitFor(want+" in "+service+"'s output", 30*time.Second,
		func() bool {
			for _, line := range strings.Split(s.Logs(service), "\n") {
				if strings.Contains(line, want) {
					found = strings.TrimSpace(line)
					return true
				}
			}
			return false
		},
		func() string { return s.Logs(service) })
	return found
}
