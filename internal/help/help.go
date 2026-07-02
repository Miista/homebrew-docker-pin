// Package help is the single source of truth for CLI help text: the binaries'
// -h/--help print from it, and tools/genman compiles the same strings into
// the man pages — edit here and both stay in sync.
package help

// Topic is one command's help text; the first line is its one-line summary.
type Topic struct {
	Cmd  string
	Text string
}

const PinUsage = `docker-pin — pin compose service images to tag + SHA digest

Usage: docker pin <service>
       docker pin --all
       docker pin upgrade <service> [version]
       docker pin upgrade --all
       docker pin list [--missing] [-q]
       docker pin version
       docker pin help [<command>]

Run 'docker pin help <command>' or 'docker pin <command> --help' for details.
Works on the compose file found in the current directory or any parent.`

var PinTopics = []Topic{
	{"pin", `docker pin — pin a service image to its current tag and SHA digest

Usage: docker pin <service>
       docker pin --all

Flags:
  -a, --all   Pin every service in the compose file.

Rewrites the service's image to <image>:<tag>@sha256:<digest>, pulling the
image first if it is not available locally. A moving tag like 'latest' is
resolved to the concrete version tag when the registry allows it.`},

	{"upgrade", `docker pin upgrade — move a pinned service to a newer version and re-pin

Usage: docker pin upgrade <service> [version]
       docker pin upgrade --all

Flags:
  -a, --all   Upgrade every service (cannot be combined with a version).

With a version, pulls exactly that tag and pins to it. Without one, follows
the image's moving tag (e.g. latest) and re-pins to what it resolves to now.
Already-up-to-date services are left untouched.`},

	{"list", `docker pin list — show every service's image, tag, and pin status

Usage: docker pin list [--missing] [-q]

Flags:
  -m, --missing   Only show unpinned services, and exit non-zero if any
                  exist — usable directly as a CI gate.
  -q, --quiet     Print service names only (for scripting).`},

	{"version", `docker pin version — print the version

Usage: docker pin version   (aliases: --version, -v)`},
}

const UnpinUsage = `docker-unpin — remove the SHA digest pin from service images

Usage: docker unpin <service>
       docker unpin --all
       docker unpin version

Flags:
  -a, --all   Unpin every service in the compose file.

Restores the service's image to <image>:<tag>, keeping the tag. The reverse
of 'docker pin'.`

var UnpinTopics = []Topic{
	{"unpin", UnpinUsage},
	{"version", `docker unpin version — print the version

Usage: docker unpin version   (aliases: --version, -v)`},
}

// For returns the help text for cmd from topics.
func For(topics []Topic, cmd string) (string, bool) {
	for _, t := range topics {
		if t.Cmd == cmd {
			return t.Text, true
		}
	}
	return "", false
}
