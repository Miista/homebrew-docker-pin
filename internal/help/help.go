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
       docker pin schedule <apply|status|remove|run>
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

With a version, pulls exactly that tag and pins to it.

Without one, the current pinned tag decides which moving tag is followed:
a plain version (2.11.4) follows 'latest'; a version-variant (2.11.4-alpine)
follows the variant's moving tag discovered in the registry (alpine,
latest-alpine or alpine-latest — errors if none or several exist); any other
tag (alpine, stable, v1.2.3-ls45) is treated as moving already and re-pulled
as-is, so it only picks up digest changes. The pulled digest is then resolved
back to a concrete version tag where the registry allows it.
Already-up-to-date services are left untouched.`},

	{"list", `docker pin list — show every service's image, tag, and pin status

Usage: docker pin list [--missing] [-q]

Flags:
  -m, --missing   Only show unpinned services, and exit non-zero if any
                  exist — usable directly as a CI gate.
  -q, --quiet     Print service names only (for scripting).`},

	{"schedule", `docker pin schedule — declarative scheduled upgrades via systemd timers

Usage: docker pin schedule apply
       docker pin schedule status
       docker pin schedule remove
       docker pin schedule run

Reads a git-trackable pin.yaml (or pin.yml) next to the compose file:

  schedule: "0 6 * * 1"        # 5-field cron expression, required
  services:                    # optional; omitted = every service
    - caddy
    - name: paperless-db       # constrained: upgrade only to registry
      tags: '^17\.\d+-alpine$' # tags matching this regex (never escapes
                               # to e.g. 18.x; skipped if none is newer)
      exclude: '(alpha|beta|rc)' # optional; drop matching candidates
      delay: 7d                # optional; only adopt a tag published at
                               # least this long ago ("48h", "7d", "2w")
  on_change: ./pin-upgraded.sh # optional; run after upgrades changed
                               # the compose file (sh -c, compose dir)
  hostname: optiplex           # optional; box name shown in notification
                               # titles (defaults to the OS hostname)
  notify:                      # optional; report each run via ntfy
    ntfy:
      url: https://ntfy.example.net
      topic: docker-pin
      token_env: NTFY_TOKEN    # env var holding the token (this default)
      token_file: /etc/ntfy.env # optional KEY=VALUE file to read it from

The cron expression is translated to a systemd OnCalendar expression.
Names (MON-SUN, JAN-DEC), steps (*/6), ranges (1-5) and lists (1,3,5)
are supported. Restricting both day-of-month and day-of-week is
rejected: cron ORs them, systemd ANDs them.`},

	{"schedule apply", `docker pin schedule apply — install or update the systemd timer

Usage: sudo docker pin schedule apply

Validates pin.yaml against the compose file, then writes
docker-pin-<slug>.service/.timer into /etc/systemd/system and enables
the timer. Idempotent: unchanged pin.yaml is a no-op; a changed one
rewrites the units and reloads systemd. Requires root and Linux.`},

	{"schedule status", `docker pin schedule status — show schedule config and unit state

Usage: docker pin schedule status

Prints the pin.yaml settings, whether the units are installed, whether
they match what pin.yaml would generate (drift check), and the next
fire time. Does not require root.`},

	{"schedule remove", `docker pin schedule remove — disable and remove the systemd units

Usage: sudo docker pin schedule remove

Disables the timer and deletes both unit files. pin.yaml itself is
left untouched, so `+"`"+`schedule apply`+"`"+` restores everything. Requires
root and Linux.`},

	{"schedule run", `docker pin schedule run — execute one scheduled run in the foreground

Usage: docker pin schedule run [--dry-run]

  -n, --dry-run   Full discovery (registry tags, delay checks, pulls) but
                  change nothing: prints "Would upgrade ..." per service.
                  No compose rewrite, no compose up, no on_change, no
                  notifications.

What the systemd service calls; also usable by hand to test. Each
listed service (or all of them) is its own transaction: upgrade the
pin like `+"`"+`docker pin upgrade`+"`"+`, run `+"`"+`docker compose up -d <service>`+"`"+`,
run on_change (environment: PIN_SERVICE, PIN_OLD_IMAGE/PIN_NEW_IMAGE,
PIN_OLD_TAG/PIN_NEW_TAG, PIN_OLD_DIGEST/PIN_NEW_DIGEST — so each
upgrade can be its own commit), and send one ntfy notification. A service with a tags regex is upgraded to the
newest matching registry tag only, and left alone when nothing newer
matches. When a service's compose up fails, only that service's pin is
rolled back and re-asserted — the remaining services proceed
untouched, and the failed upgrade retries next run. A failed on_change
is non-fatal (a stranded commit rides along with the next push).
Failures notify at high priority. The exit code is non-zero if any
service failed.`},

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
