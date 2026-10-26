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
// schedule. v4 also checks once at startup, so restarting it is how a test
// asks for a check now. v5 does not, and is asked the way a person asks, with
// "Check…" through the queue API -- after a restart too, which clears the
// minute that API waits between two checks of everything.
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
	s.askForCheck(service)

	s.waitFor("a check by "+service, 60*time.Second,
		func() bool { return s.checksDone(service) > before },
		func() string { return s.Logs(service) })
	s.settled(service)
}

// checksDone is how many checks a duva has finished: each one says so, with
// what it found, whether it ran on the schedule, at startup or was asked for.
func (s *Scenario) checksDone(service string) int {
	return strings.Count(s.Logs(service), "check complete:")
}

// askForCheck asks a v5 duva to check every service now, as "Check…" does,
// once its API answers. v4 checks at startup and needs nothing.
func (s *Scenario) askForCheck(service string) {
	s.t.Helper()
	if !strings.HasPrefix(s.Name, "duva-v5/") {
		return
	}
	s.waitFor(service+"'s queue API", 60*time.Second,
		func() bool {
			_, err := s.compose("exec", "-T", service, "wget", "-qO-", "http://127.0.0.1:8080/healthz")
			return err == nil
		},
		func() string { return s.Logs(service) })
	s.API("/v1/check-all", nil)
}

// settled waits, for a v5 duva, until no apply is in flight. A check hands
// what policy allows to the updater and says it is complete; the apply runs
// on after it, and a test reading the container before it lands reads the
// old one.
func (s *Scenario) settled(service string) {
	s.t.Helper()
	if !strings.HasPrefix(s.Name, "duva-v5/") {
		return
	}
	s.waitFor(service+" to finish applying", 60*time.Second,
		func() bool {
			out, err := s.compose("exec", "-T", service, "wget", "-qO-",
				"--header", "Authorization: Bearer "+v5Token, "http://127.0.0.1:8080/v1/snapshot")
			// The key, not the word: an asked check's finding says
			// "fate":"applying" long after the apply has ended.
			return err == nil && !strings.Contains(out, `"applying":`)
		},
		func() string { return s.Logs(service) })
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
