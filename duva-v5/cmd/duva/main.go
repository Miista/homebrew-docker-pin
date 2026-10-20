// duva: one binary for watching pinned services and applying the updates
// their policy allows.
//
// For now each component still runs as its own process, chosen by the first
// argument, exactly as the four v4 binaries did: `duva watch serve` is v4's
// `watch serve`. That is a step on the way to `duva serve`, which runs them
// in one process (docs/duva-v5-one-binary.md); until then, the integration
// suite exercises the same behaviour through one binary.
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

const usage = "Usage: duva <watch|queue|update|ui|version> [arguments]"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	watcher.Version, gate.Version, updater.Version, webui.Version = version, version, version, version

	args := os.Args[2:]
	switch os.Args[1] {
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
