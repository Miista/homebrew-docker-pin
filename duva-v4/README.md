# duva v4

Four single-purpose processes that watch pinned compose services and apply the
updates each one's policy allows.

| | what it does | docker socket | /compose | git |
|---|---|---|---|---|
| **detector** | what tags exist, and when | — | read | — |
| **decider** | is this an update, how big, may it be applied | — | read | — |
| **actor** | pull, write the pin, recreate, commit | read-write | **read-write** | yes |
| **ui** | the approval queue, over one or more deciders | — | — | — |

Only the actor holds any privilege. That is the point of the split: the
network-facing half needs none of what the work needs.

## Running the actor

**The actor must run as the user that owns the repository, and that user must
be in the `docker` group.**

It checks this at startup and refuses to run otherwise, reporting every
problem at once rather than one per restart:

```
this actor cannot do the work it exists for:

  - this is running as root (uid 0), which would leave root-owned files in
    the repository mounted at /compose and lose the commit identity its
    .git/config carries
    Set `user: "<uid>:<gid>"` ...
```

Refusing to start is right here where it would be wrong elsewhere: a detector
whose webhook is down still detects and logs, but an actor that cannot write,
cannot reach the daemon, or has no git can do nothing at all. Starting anyway
means discovering it halfway through an apply — after a container has been
replaced and before the pin recording it was written.

The checks are: not root, `/compose` writable, the docker socket openable,
git present and runnable, and the repository having a commit identity. The
last matters because the commit is the *final* step of an apply: a missing
`user.email` fails it after the container has already been replaced.

This is not a hardening preference. Each half of it is load-bearing:

### As the repository's owner

The actor rewrites the `image:` line in the compose file and commits it, in a
repository bind-mounted from the host. Running as root instead:

- **Creates root-owned files in your repository.** Every file the actor
  touches becomes root's, and editing them afterwards needs `sudo`. This is
  not theoretical — the detector hit exactly this with its `/data` volume.
- **Makes git refuse.** Git rejects a repository whose owner is not the
  running user (`detected dubious ownership`). The actor passes
  `-c safe.directory` so it still works, but that is a workaround for a
  problem that running as the owner does not have.
- **Loses the commit identity.** Identity comes from the repository's own
  `.git/config`, which travels with the bind mount. The actor passes no
  `-c user.email`: whatever the repository already uses is the right answer.

### In the `docker` group

The actor pulls images and replaces containers through the daemon's API over
the mounted socket. The socket is `root:docker` mode `0660`, so a non-root
user reaches it only through group membership.

The group must be added by GID, and **the GID is host-specific** — find it per
host rather than copying a number between them:

```sh
stat -c '%g' /var/run/docker.sock    # e.g. 989
```

### The compose entry

```yaml
duva-actor:
  image: registry.example.com/duva-actor:<tag>@sha256:<digest>
  restart: unless-stopped
  # The user that owns the repository below. Not root: see README.
  user: "1000:1000"
  # The docker group's GID *on this host*. Find it with:
  #   stat -c '%g' /var/run/docker.sock
  group_add: ["989"]
  environment:
    ACTOR_TOKEN: ${DUVA_ACTOR_TOKEN}
    # What a change is committed under. The host prefix is literal text --
    # this repository's commit-msg hook requires <box>/<service>.
    ACTOR_COMMIT_TEMPLATE: "optiplex/{{.Container}}: update to {{if eq .OldVersion .NewVersion}}{{.NewDigest}}{{else}}{{.NewVersion}}{{end}}"
    # Commits stay local. Turning this on pushes unattended.
    ACTOR_GIT_PUSH: "false"
  volumes:
    # Read-write, unlike the decider's: rewriting the pin is the whole point.
    # The actor refuses to start against a :ro mount.
    - ..:/compose
    # Not :ro. That flag applies to the socket *file*, not to the API served
    # over it -- the daemon never sees it, and a container with a :ro socket
    # can still create and delete containers. Measured, not assumed:
    # POST /containers/create through a :ro mount answers 404 (no such image),
    # not 403. Anything holding this socket has root on the host, which is
    # why only the actor holds it.
    - /var/run/docker.sock:/var/run/docker.sock
```

A different actor can take its place: the contract is specified in
`../docs/actor-contract.md`.

The decider reaches it by name, with `DECIDER_ACTOR_URL: http://duva-actor:8080`
and `DECIDER_ACTOR_TOKEN` matching `ACTOR_TOKEN`.

Without `DECIDER_ACTOR_URL` the decider queues every decision and applies
nothing. That is a legitimate way to run it, and the right way to run it
first: clicking Update then answers "no actor is configured, so nothing can be
applied from here" and leaves the entry queued.

## Running the others

The detector, decider and UI hold nothing and need no special user. All three
are distroless and answer `health` as a subcommand, which is what their
`HEALTHCHECK` runs — there is no shell or curl in the image to poll a port
with.

The detector needs `/data` as a **named volume**, not a bind mount: Docker
chowns a fresh named volume to match what the image carries, and the image
carries an empty `/data` owned by the user it runs as. A bind mount arrives
root-owned and the detector fails on its final write, having done the whole
run.

## Configuration

**detector** — `DETECTOR_HOST`, `DETECTOR_COMPOSE_SUBDIR`,
`DETECTOR_WEBHOOK_URL`, `DETECTOR_SCHEDULE` (5-field cron, default `0 3 * * *`),
`DETECTOR_SINCE`, `DETECTOR_LOG_LEVEL`. Run it as `serve` to check on the
schedule, or `run` for one pass.

**decider** — `DECIDER_HOST`, `DECIDER_COMPOSE_SUBDIR`, `DECIDER_TOKEN`,
`DECIDER_ACTOR_URL`, `DECIDER_ACTOR_TOKEN`, `DECIDER_APPLY_TIMEOUT`.

Set `DECIDER_TOKEN` rather than letting it mint one: a minted token changes on
every restart, and anything holding it goes stale.

**actor** — `ACTOR_TOKEN`, `ACTOR_GIT_PUSH`, `ACTOR_COMMIT_TEMPLATE`.

The template is what a change is committed under, rendered with `.Container`,
`.Image`, `.OldVersion`, `.NewVersion`, `.OldDigest`, `.NewDigest`. Unset gets
a sensible default; set-but-empty is an error.

There is no `.Host` field and no `ACTOR_HOST`: the actor needs the host for
nothing, and the template is per host anyway, so a host prefix goes in as
literal text. This repository's `commit-msg` hook requires one:

```yaml
ACTOR_COMMIT_TEMPLATE: "optiplex/{{.Container}}: update to {{if eq .OldVersion .NewVersion}}{{.NewDigest}}{{else}}{{.NewVersion}}{{end}}"
```

A template that does not parse, or names a field that does not exist, refuses
to start — rather than being discovered by an apply, after a container has
already been replaced.

**ui** — `DUVA_UI_DECIDERS` (`host=url,host=url`), `DUVA_UI_TOKEN`,
`DUVA_UI_TOKEN_<HOST>` where they differ, `DUVA_UI_READ_ONLY`.

Tokens are deliberately not in `DUVA_UI_DECIDERS`: a token is the authority to
replace a container on that host, so a list carrying one could not be shown in
a log or committed to git, which is where the rest of it belongs.

## Build

```sh
make build      # all four binaries
make test
make images     # one image each; context is the repository root
make push
```

The build stage runs on the *native* platform and Go cross-compiles to the
target (`--platform=$BUILDPLATFORM`, `GOARCH=$TARGETARCH`). Emulating the Go
toolchain under QEMU dies with `fatal error: sync: inconsistent mutex state`
partway through compiling, so building an amd64 image on an arm64 machine has
to cross-compile rather than emulate.
