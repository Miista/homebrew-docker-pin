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
       docker pin upgrade <service> [version] [--dry-run]
       docker pin upgrade --all [--dry-run]
       docker pin list [--missing] [-q]
       docker pin version
       docker pin help [<command>]

Run 'docker pin help <command>' or 'docker pin <command> --help' for details.
Works on the compose file found in the current directory or any parent.`

var PinTopics = []Topic{
	{"pin", `docker pin — pin a service image to its current tag and SHA digest

Usage: docker pin <service> [--dry-run]
       docker pin --all [--dry-run]

Flags:
  -a, --all       Pin every service in the compose file.
  -n, --dry-run   Do everything (including the pull, since digest discovery
                  needs it) except rewrite the compose file: prints
                  "Would pin ..." instead.

Rewrites the service's image to <image>:<tag>@sha256:<digest>.

Pins what is actually running: the digest comes from the service's running
container if there is one, else the local image for the tag, else a pull.
The tag is kept exactly as written -- it is the tag to follow, and the digest
records what runs -- so a service on 'latest' stays on 'latest'.

If the tag has moved since the container started, pin says so and records the
running digest; use 'docker pin upgrade' to move to what the tag points at.`},

	{"upgrade", `docker pin upgrade — move a pinned service to a newer version and re-pin

Usage: docker pin upgrade <service> [version] [--dry-run]
       docker pin upgrade --all [--dry-run]

Flags:
  -a, --all       Upgrade every service (cannot be combined with a version).
  -n, --dry-run   Do everything (including the pull) except rewrite the
                  compose file: prints "Would upgrade ..." instead.

With a version, pulls exactly that tag and pins to it.

Without one, the current pinned tag decides which moving tag is followed:
a plain version (2.11.4) follows 'latest'; a version-variant (2.11.4-alpine)
follows the variant's moving tag discovered in the registry (alpine,
latest-alpine or alpine-latest — errors if none or several exist); any other
tag (alpine, stable, v1.2.3-ls45) is treated as moving already and re-pulled
as-is, so it only picks up digest changes. The tag written back is always the
tag that was pulled — never a different one guessed from the digest.
Already-up-to-date services are left untouched.

This is also how you follow a tag that moved upstream: run upgrade to
re-pull and re-pin to the new digest. No need to 'docker pull' by hand
first — upgrade pulls for you, and a pinned service never picks up a moved
tag on its own (compose up fetches strictly by the pinned digest).`},

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

Usage: docker unpin <service> [--dry-run]
       docker unpin --all [--dry-run]
       docker unpin version

Flags:
  -a, --all       Unpin every service in the compose file.
  -n, --dry-run   Don't rewrite the compose file: prints "Would unpin ..."
                  instead.

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
