// duva: one binary for watching pinned services and applying the updates
// their policy allows.
//
// `duva serve` runs watch, queue, updater and, with DUVA_SETTING_UI, the page, in
// one process (docs/duva-v5-one-binary.md).
package main

import (
	"fmt"
	"os"
	// The timezone database, embedded in the binary: the image has no
	// /usr/share/zoneinfo, and without this TZ is silently ignored.
	_ "time/tzdata"

	"github.com/Miista/homebrew-docker-pin/duva-v5/gate"
	"github.com/Miista/homebrew-docker-pin/duva-v5/updater"
	"github.com/Miista/homebrew-docker-pin/duva-v5/watcher"
	"github.com/Miista/homebrew-docker-pin/duva-v5/webui"
)

var version = "dev"

const usage = "Usage: duva <serve|queue|discard|block|unblock|check|health|version>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	watcher.Version, gate.Version, updater.Version, webui.Version = version, version, version, version

	switch os.Args[1] {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	case "self-update":
		// Not in the usage: duva starts this itself, in a copy of its own
		// container, to replace that container. See updater.SelfUpdate.
		if err := updater.SelfUpdate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	case "queue":
		if err := listQueue(dataFile); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	case "rescan":
		// Gone, as a separate command: it was a check that looked back a week.
		fmt.Fprintln(os.Stderr, "Error: rescan is now duva check <service> --since 7d")
		os.Exit(1)
	case "discard", "block", "unblock", "check":
		if err := act(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	case "health":
		os.Exit(health())
	case "version", "--version", "-v":
		fmt.Println("duva", version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}
