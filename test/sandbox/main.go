// sandbox stands a scenario up and leaves it running, to look at what duva
// makes of it.
//
//	go run ./test/sandbox duva-policy/four-verdicts
//	go run ./test/sandbox down
//
// It calls the same harness the integration tests do, so what a person stands
// up by hand is exactly what the tests exercise. The shell script this
// replaces was a second copy of that setup, and the two drifted.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/Miista/homebrew-docker-pin/test/integration"
)

// console is what the harness needs from a test, for a program that is not
// one.
//
// Cleanup does nothing, which is the whole point: a sandbox is a scenario
// that outlives the process that made it.
type console struct{}

func (console) Helper()                           {}
func (console) Logf(format string, args ...any)   { log.Printf(format, args...) }
func (console) Fatal(args ...any)                 { log.Fatal(args...) }
func (console) Fatalf(format string, args ...any) { log.Fatalf(format, args...) }
func (console) Cleanup(func())                    {}

func main() {
	log.SetFlags(0)
	log.SetPrefix("sandbox: ")

	if len(os.Args) != 2 {
		usage()
	}

	root, err := integration.Setup()
	if err != nil {
		log.Fatal(err)
	}

	if os.Args[1] == "down" {
		integration.Down(console{}, root)
		integration.Teardown(root)
		log.Print("testbed emptied")
		return
	}

	s := integration.Up(console{}, os.Args[1])

	// Pin whatever the fixture watches, so duva has something to act on: a
	// fixture holds a plain tag, and an unpinned service is skipped. A test
	// does this itself, since which services it pins is part of what it is
	// testing; a sandbox just wants the scenario as it is meant to be seen.
	if watched := s.Watched(); len(watched) > 0 {
		s.Pin(watched...)
	}
	// The newer tags the scenario means duva to find, if it names any.
	s.PushAdditional()
	s.Start()

	fmt.Println()
	fmt.Printf("scenario: %s\n", os.Args[1])
	fmt.Printf("project:  %s\n", s.Dir)
	fmt.Println()
	fmt.Println("tear it down with: go run ./test/sandbox down")
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: go run ./test/sandbox <suite>/<scenario>")
	fmt.Fprintln(os.Stderr, "       go run ./test/sandbox down")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "scenarios:")
	for _, name := range integration.Scenarios() {
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
	os.Exit(2)
}
