# duva v4

A three-stage pipeline that watches pinned compose services and applies the
updates each one's policy allows, with a UI over the middle stage.

    watch  ->  queue  ->  update
                 ^
                 |
                 ui

| | what it does | docker socket | /compose | git |
|---|---|---|---|---|
| **watch** | what tags exist, and when | — | read | — |
| **queue** | is this an update, how big, may it be applied | — | read | — |
| **update** | pull, write the pin, recreate, commit | read-write | **read-write** | yes |
| **ui** | the approval queue, over one or more hosts | — | — | — |

**All three stages run on the host they touch.** Each reads that host's compose
project and, in the case of update, its docker socket and its repository — none
of which is reachable from anywhere else. Only the UI is host-agnostic: it holds
no socket, reads no compose file, and serves as many hosts' queues as it is
given.

Only update holds any privilege. That is the point of the split: the
network-facing half needs none of what the work needs.

## Running the updater

**The update stage must run as the user that owns the repository, and that user must
be in the `docker` group.**

It checks this at startup and refuses to run otherwise, reporting every
problem at once rather than one per restart:

```
this stage cannot do the work it exists for:

  - this is running as root (uid 0), which would leave root-owned files in
    the repository mounted at /compose and lose the commit identity its
    .git/config carries
    Set `user: "<uid>:<gid>"` ...
```

Refusing to start is right here where it would be wrong elsewhere: a watcher
whose webhook is down still watches and logs, but an updater that cannot write,
cannot reach the daemon, or has no git can do nothing at all. Starting anyway
means discovering it halfway through an apply — after a container has been
replaced and before the pin recording it was written.

The checks are: not root, `/compose` writable, the docker socket openable,
git present and runnable, and the repository having a commit identity. The
last matters because the commit is the *final* step of an apply: a missing
`user.email` fails it after the container has already been replaced.

This is not a hardening preference. Each half of it is load-bearing:

### As the repository's owner

Update rewrites the `image:` line in the compose file and commits it, in a
repository bind-mounted from the host. Running as root instead:

- **Creates root-owned files in your repository.** Every file the updater
  touches becomes root's, and editing them afterwards needs `sudo`. This is
  not theoretical — the watcher hit exactly this with its `/data` volume.
- **Makes git refuse.** Git rejects a repository whose owner is not the
  running user (`detected dubious ownership`). The updater passes
  `-c safe.directory` so it still works, but that is a workaround for a
  problem that running as the owner does not have.
- **Loses the commit identity.** Identity comes from the repository's own
  `.git/config`, which travels with the bind mount. The updater passes no
  `-c user.email`: whatever the repository already uses is the right answer.

### In the `docker` group

The updater pulls images and replaces containers through the daemon's API over
the mounted socket. The socket is `root:docker` mode `0660`, so a non-root
user reaches it only through group membership.

The group must be added by GID, and **the GID is host-specific** — find it per
host rather than copying a number between them:

```sh
stat -c '%g' /var/run/docker.sock    # e.g. 989
```

### The compose entry

```yaml
duva-update:
  image: registry.example.com/duva-update:<tag>@sha256:<digest>
  restart: unless-stopped
  # The user that owns the repository below. Not root: see README.
  user: "1000:1000"
  # The docker group's GID *on this host*. Find it with:
  #   stat -c '%g' /var/run/docker.sock
  group_add: ["989"]
  environment:
    DUVA_UPDATE_TOKEN: ${DUVA_UPDATE_TOKEN}
    # What a change is committed under. The host prefix is literal text --
    # this repository's commit-msg hook requires <box>/<service>.
    DUVA_UPDATE_COMMIT_TEMPLATE: "optiplex/{{.Container}}: update to {{if eq .OldVersion .NewVersion}}{{.NewDigest}}{{else}}{{.NewVersion}}{{end}}"
    # Commits stay local. Turning this on pushes unattended.
    DUVA_UPDATE_GIT_PUSH: "false"
  volumes:
    # Read-write, unlike the queue's: rewriting the pin is the whole point.
    # The updater refuses to start against a :ro mount.
    - ..:/compose
    # Not :ro. That flag applies to the socket *file*, not to the API served
    # over it -- the daemon never sees it, and a container with a :ro socket
    # can still create and delete containers. Measured, not assumed:
    # POST /containers/create through a :ro mount answers 404 (no such image),
    # not 403. Anything holding this socket has root on the host, which is
    # why only the updater holds it.
    - /var/run/docker.sock:/var/run/docker.sock
```

A different updater can take its place: the contract is specified in
`../docs/update-contract.md`.

The queue reaches it by name, with `DUVA_QUEUE_UPDATE_URL: http://duva-update:8080`
and `DUVA_QUEUE_UPDATE_TOKEN` matching `DUVA_UPDATE_TOKEN`.

Without `DUVA_QUEUE_UPDATE_URL` the queue holds every decision and applies
nothing. That is a legitimate way to run it, and the right way to run it
first: clicking Update then answers "no actor is configured, so nothing can be
applied from here" and leaves the entry queued.

### A dirty repository, or a rate-limited registry

Applying commits, so the updater will not start on a repository with
uncommitted changes, or while git holds its index lock. That is not a failure:
nothing was touched, so it answers `deferred`, the queue keeps the entry, and
no failure notification is sent.

A pull the registry refused for rate limiting is answered the same way. The
daemon reports it as a 500, like every other registry error, so it is
recognised by the registry's text (`toomanyrequests`, `retry-after`); any
other pull failure, `denied` included, is still `failed`.

What happens next depends on who asked:

- **An update the queue applied on its own** (`duva.auto`) is offered again
  with exponential backoff: after 1 minute, then 2, 4, 8, 16, and every 30
  minutes from there until the repository is clean or the entry leaves the
  queue. Commit or stash and it goes through on the next attempt.
- **A click in the UI** is tried once and not retried. The reason is shown, the
  entry stays queued, and clicking again once the repository is clean applies
  it. The page also greys Update while the repository is dirty.

The backoff is held in memory, so restarting the queue starts it again at one
minute. The intervals are not configurable. A queue older than this change does
not recognise `deferred`, so deploy the queue before the updater.

### Soaking a release

`duva.auto` applies a release on the run that finds it, which is before anyone
could have heard it was broken. `duva.soak` holds it back until it is old
enough:

```yaml
labels:
  duva.auto: patch
  duva.soak: 3d   # Go durations too: 72h, 90m. 0 turns it off.
```

There is no host-wide default: a soak is a judgement about one image's
upstream, like `duva.auto`, so it sits beside it. No label means no soak. An
unreadable value is refused, not treated as no soak.

The soak counts from when the release was published, as the watcher reports
it, not from when it was found, so a daily check does not add a day. A notice
without a publish time counts from when it arrived.

A soaking release sits in the queue with "soaking until …" and is not
announced. The queue releases it on its recheck tick (`DUVA_RECONCILE_INTERVAL`,
ten minutes by default), so it applies up to one interval late. From then on it
is an ordinary auto-apply: a deferral is retried, a failure is announced and
waits for a person. Clicking Update applies it at once, soak or not.

A soak only delays what policy would apply on its own. An update that waits
for a person was never going to apply unattended, so it has no soak.

### Updating by hand

When it starts, and every ten minutes by default, the queue rechecks what is waiting
against the compose file. An entry goes once the file has what it offered —
and also once the file has moved somewhere it no longer leads from: a service
moved to `latest`, or past the offered version, or a moving tag pulled to a
different digest. Kept, those would offer a step backwards. A version change
the file has moved only partway towards stays, rewritten to start from where
the service is now. The log says why each entry went.

### Updating the updater

The updater cannot replace its own container: recreating stops the old
container before creating the new one, and when the old one is the updater,
the stop is the last thing it does. So an update naming the updater is handed
to a helper — the same image, run as `update self-update`, in a container
copied from the updater's own (same user, group, socket, repository and
networks, but none of its compose labels or network aliases). To the helper
the updater is an ordinary container.

- The queue is answered with the helper's stream, so it watches the helper do
  the work and reads its outcome. Nothing in the queue or the contract changes.
- The helper does that one job. It serves no `/v1/apply`, and it refuses a job
  naming anything but the updater that started it.
- While the helper lives, the old updater reports not ready and defers any
  work it is offered, since the helper is about to stop it. If the helper exits
  without replacing it — a failed pull, say — the old updater takes work
  again.
- A self-update is deferred while the updater is applying anything else:
  stopping it would interrupt that.
- On success the helper removes its own container. On failure it is left,
  named `duva-update-self-update-<time>` and labelled `duva.self-update`, so
  `docker logs` can say why. Remove it with `docker rm` once read.

An updater older than this change still replaces itself in place and leaves
the host without one, so the first update *to* this version must be done by
hand: `docker compose up -d duva-update`, then commit the pin.

## Running the others

The watcher, queue and UI hold nothing and need no special user (the UI does
need a writable `/app/data` for its sign-in state, see **ui** below). All three
are distroless and answer `health` as a subcommand, which is what their
`HEALTHCHECK` runs — there is no shell or curl in the image to poll a port
with.

The watcher needs `/data` as a **named volume**, not a bind mount: Docker
chowns a fresh named volume to match what the image carries, and the image
carries an empty `/data` owned by the user it runs as. A bind mount arrives
root-owned and the watcher fails on its final write, having done the whole
run.

## Configuration

**detector** — `DUVA_WATCH_HOST`, `DUVA_WATCH_COMPOSE_SUBDIR`,
`DUVA_WATCH_WEBHOOK_URL`, `DUVA_WATCH_SCHEDULE` (5-field cron, default `0 3 * * *`),
`DUVA_WATCH_SINCE`, `DUVA_WATCH_LOG_LEVEL`. Run it as `serve` to check on the
schedule, or `run` for one pass.

**queue** — `DUVA_QUEUE_HOST`, `DUVA_QUEUE_COMPOSE_SUBDIR`, `DUVA_QUEUE_TOKEN`,
`DUVA_QUEUE_UPDATE_URL`, `DUVA_QUEUE_UPDATE_TOKEN`, `DUVA_QUEUE_UPDATE_TIMEOUT`,
`DUVA_RECONCILE_INTERVAL` (also how often a soak is checked for release; see
[Soaking a release](#soaking-a-release)).

Set `DUVA_QUEUE_TOKEN` rather than letting it mint one: a minted token changes on
every restart, and anything holding it goes stale.

**actor** — `DUVA_UPDATE_TOKEN`, `DUVA_UPDATE_GIT_PUSH`, `DUVA_UPDATE_COMMIT_TEMPLATE`.

The template is what a change is committed under, rendered with `.Container`,
`.Image`, `.OldVersion`, `.NewVersion`, `.OldDigest`, `.NewDigest`. Unset gets
a sensible default; set-but-empty is an error.

There is no `.Host` field and no `DUVA_UPDATE_HOST`: the updater needs the host for
nothing, and the template is per host anyway, so a host prefix goes in as
literal text. This repository's `commit-msg` hook requires one:

```yaml
DUVA_UPDATE_COMMIT_TEMPLATE: "optiplex/{{.Container}}: update to {{if eq .OldVersion .NewVersion}}{{.NewDigest}}{{else}}{{.NewVersion}}{{end}}"
```

A template that does not parse, or names a field that does not exist, refuses
to start — rather than being discovered by an apply, after a container has
already been replaced.

**ui** — `DUVA_UI_QUEUES` (`host=url,host=url`), `DUVA_UI_TOKEN`,
`DUVA_UI_TOKEN_<HOST>` where they differ, `DUVA_UI_READ_ONLY`.

Tokens are deliberately not in `DUVA_UI_QUEUES`: a token is the authority to
replace a container on that host, so a list carrying one could not be shown in
a log or committed to git, which is where the rest of it belongs.

The UI signs people in through [authy](https://github.com/miista/authy), because
its Update button is that same authority. With `DUVA_UI_OIDC_ISSUER`,
`DUVA_UI_OIDC_CLIENT_ID`, `DUVA_UI_OIDC_CLIENT_SECRET` and `DUVA_UI_OIDC_BASE_URL`
(all four together) sign-in is the provider's, with redirect URI
`<BASE_URL>/auth/callback`; with none of them the first visitor creates a local
account. `DUVA_UI_LOCAL_ENABLED=true` keeps that account next to OIDC as a
break-glass login, and `DUVA_UI_AUTH_DISABLED=true` turns the gate off for a UI
that sits behind something else. Only `/healthz` is public.

The gate keeps a signing key in `/app/data/auth`, so mount a writable volume at
`/app/data` for the UI's user (the image runs as `nonroot`, so set `user:` to
whoever owns a bind mount, as the queue does). Losing it only signs people out.

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
