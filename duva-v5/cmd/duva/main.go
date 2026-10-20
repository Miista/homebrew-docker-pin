// duva: one binary for watching pinned services and applying the updates
// their policy allows.
//
// `duva serve` runs watch, queue and updater in one process
// (docs/duva-v5-one-binary.md). Each can still run alone, chosen by the first
// argument, exactly as the four v4 binaries did: `duva watch serve` is v4's
// `watch serve`. The self-update helper is `duva update self-update`.
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

const usage = "Usage: duva <serve|watch|queue|update|ui|version> [arguments]"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	watcher.Version, gate.Version, updater.Version, webui.Version = version, version, version, version

	args := os.Args[2:]
	switch os.Args[1] {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	case "watch":
		watcher.Main(args)
	case "queue":
		gate.Main(args)
	case "update":
		updater.Main(args)
	case "ui":
		webui.Main(args)
	case "version", "--version", "-v":
		fmt.Println("duva", version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}
