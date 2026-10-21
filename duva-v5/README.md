# duva v5

Watches pinned compose services and applies the updates each one's policy
allows. One process per host: the same watcher, queue, updater and UI as v4,
connected by calls instead of HTTP. See `../docs/duva-v5-one-binary.md` for
why.

    watch  ->  queue  ->  update
                 ^
                 |
                 ui   (one host only, over every host's queue)

| | what it does |
|---|---|
| **watch** | what tags exist, and when they were published |
| **queue** | is this an update, how big, may it be applied, and when |
| **update** | pull, write the pin, recreate, commit |
| **ui** | the approval page, over every host's queue; on one host, with `DUVA_SETTING_UI` |

Every host runs `duva serve`, and they are peers. One of them also serves the
page, and reaches the others' queues over their API.

## Deploying

```yaml
duva:
  image: ghcr.io/miista/duva:<tag>@sha256:<digest>
  restart: unless-stopped
  command: ["serve"]
  # The user that owns the repository below, and the docker group's GID on
  # *this* host -- see "The user it runs as".
  user: "1000:1000"
  group_add: ["989"]
  environment:
    DUVA_SETTING_HOST: optiplex
    DUVA_SETTING_REPO: /home/guldmund/docker
    DUVA_SETTING_API_TOKEN: ${DUVA_API_TOKEN}
    # The host prefix is literal text: this repository's commit-msg hook
    # requires <box>/<service>.
    DUVA_SETTING_COMMIT_TEMPLATE: "optiplex/{{.Container}}: update to {{if eq .OldVersion .NewVersion}}{{.NewDigest}}{{else}}{{.NewVersion}}{{end}}"
    DUVA_SETTING_NTFY_ENDPOINT: https://ntfy.example.com
    DUVA_SETTING_NTFY_TOPIC: duva
  volumes:
    # The repository at the path it has on the host, read-write: the pin is
    # rewritten here, and a relative bind in a compose file must resolve to a
    # path the daemon can also find.
    - /home/guldmund/docker:/home/guldmund/docker
    # Not :ro. That flag applies to the socket file, not to the API served
    # over it: a container with a :ro socket can still create and delete
    # containers. Anything holding this socket has root on the host.
    - /var/run/docker.sock:/var/run/docker.sock
    # Every component's state. A named volume, so Docker seeds it with the
    # image's ownership (uid 1000).
    - duva-data:/data
  ports:
    # The API, for the page on a peer. Bind it to the LAN address, not
    # 0.0.0.0, and firewall it to the page's host. Not needed at all on the
    # host that serves the page.
    - "192.168.1.10:8080:8080"
```

On the host that serves the page, add:

```yaml
  environment:
    DUVA_SETTING_UI: "true"
    DUVA_SETTING_UI_OIDC_ISSUER: https://auth.example.com
    DUVA_SETTING_UI_OIDC_CLIENT_ID: duva
    DUVA_SETTING_UI_OIDC_CLIENT_SECRET: ${DUVA_UI_OIDC_CLIENT_SECRET}
    DUVA_SETTING_UI_OIDC_BASE_URL: https://duva.example.com
    # The other hosts. Tokens are not in this list: each is its own setting,
    # and is that host's DUVA_SETTING_API_TOKEN.
    DUVA_SETTING_PEERS: pi=http://192.168.1.11:8080
    DUVA_SETTING_PEER_PI_TOKEN: ${DUVA_PI_API_TOKEN}
  # The page is on 8081, apart from the API on 8080: the page is published
  # (through the reverse proxy), and the API must not be.
```

The page holds the authority to replace containers on every host, and runs in
a process that holds this host's docker socket. Sign-in is OIDC, always: duva
refuses to start without all four `DUVA_SETTING_UI_OIDC_*` settings, and
refuses v4's `DUVA_UI_AUTH_DISABLED`, `DUVA_UI_LOCAL_ENABLED` and
`DUVA_UI_AUTH_PUBLIC_PATHS` by name, so a v4 configuration that set them fails
to start rather than running with less protection than intended. Register
`<base url>/auth/callback` as the redirect URI. Only `/healthz` is public.

## The user it runs as

**duva must run as the user that owns the repository, in the `docker` group.**
It checks this at startup and refuses to run otherwise, reporting every
problem at once. The checks are: not root, the repository writable, the
docker socket openable, git present, and the repository having a commit
identity. The last matters because the commit is the *final* step of an
apply: a missing `user.email` fails it after the container has been replaced.

- **As the repository's owner.** Running as root leaves root-owned files in
  the repository, makes git refuse it (`detected dubious ownership`), and
  loses the commit identity, which comes from the repository's own
  `.git/config`.
- **In the `docker` group.** The socket is `root:docker` mode `0660`. Add the
  group by GID, and **the GID is host-specific**:
  `stat -c '%g' /var/run/docker.sock`.

## Policy

A service is watched when it is pinned (`image: name:tag@sha256:...`) and has
a `container_name`, which is how duva identifies it. Policy is labels on the
service:

```yaml
labels:
  duva.auto: patch        # none (default), digest, patch, minor, major
  duva.delay: 3d          # how old a release must be before duva.auto applies it
  diun.include_tags: '^1\.\d+\.\d+$'
  diun.exclude_tags: '-rc'
```

**Policy is read from the running container, not the compose file.** A label
edited and committed takes effect when the container is recreated -- by
`docker compose up`, or by duva's next apply of it -- and until then the
service runs under the policy it was started with. A service with no
container has no policy and is not acted on. The pin and the tag to follow do
come from the file, since that is what duva rewrites.

## Operating

### A dirty repository, or a rate-limited registry

Applying commits, so duva will not start an apply on a repository with
uncommitted changes, or while git holds its index lock. That is not a failure:
nothing was touched, the entry stays queued, and nobody is notified. A pull the
registry refused for rate limiting is treated the same way.

- **An update duva applied on its own** (`duva.auto`) is offered again with
  exponential backoff: after 1 minute, then 2, 4, 8, 16, and every 30 minutes
  from there. Commit or stash and it goes through on the next attempt.
- **A click on the page** is tried once. The reason is shown, the entry stays
  queued, and the page greys Update while the repository is dirty.

### Delaying a release

`duva.auto` alone applies a release on the run that finds it, before anyone
could have heard it was broken. `duva.delay` holds it back until it is old
enough, counted from when the release was published, not when it was found.

It delays only what `duva.auto` would apply unattended. A change bigger than
`duva.auto` allows goes to the queue for a person straight away, delay or not,
and a service with no `duva.auto` has nothing to delay.

| `duva.delay` | means |
|---|---|
| `3d` | whole days |
| `72h`, `90m`, `1h30m` | any Go duration |
| `0`, or no label | no delay |

Anything else -- `3 days`, `1w`, `-1d` -- is refused rather than read as no
delay. There is no host-wide default. An early-v5 `duva.soak` label is
refused by name: it is `duva.delay` now.

A delayed release is listed on the page under **Scheduled**, with when it
applies, and is not announced. It is released on the queue's reconcile, at
most `DUVA_SETTING_RECONCILE_INTERVAL` late (ten minutes by default), or as
soon as a container event prompts one. From then on it is an ordinary
auto-apply. **Update now** applies it at once.

Each release is delayed on its own clock. A project that releases faster than
its delay does not keep restarting the wait: an older release that comes due
while a newer one is still delayed is applied, and the newer one is then
offered from there. A release waiting for a person is different -- only the
newest matters, so a newer one supersedes it.

### Seeing what is waiting, and acting on it

On the page, the menu beside each row's Update button has **Block**: that
release is never applied, on any service running the image, until you unblock
it from **Blocked releases** in the menu at the top, which puts it back in the
queue. On a phone, tapping a row opens a sheet with Update and Block.

**Check…**, also in the menu at the top, looks for new releases: opened and
confirmed, every service on every host, since the last check. Its folded
**Options** narrow it to the services picked -- any a host watches, waiting or
not -- and choose how far back: since the last check, or the past day, week
or month. Since the last check finds what is new. Further back
also finds what an earlier check passed over -- a tag a since-changed
`diun.include_tags` filtered out, a release dropped from the queue -- and
offers it again; blocked releases stay blocked, and anything older than what
runs is not an update. Each service looks back to whichever is earlier, its
own last check or the chosen time, so a longer look never sees less.

The history records who did each -- the signed-in person, or what asked over
the API. `docker exec <duva> duva queue` prints what is waiting, by service,
with each entry's id.

**A block is by digest, never by tag**, so a release re-pushed under the same
tag with a fix is not blocked with the broken one. A version change's digest
is not known until it is pulled, so blocking one looks up what its tag points
at now, and the updater checks the blocklist again after every pull: a
release blocked after it was queued, or approved, is still refused, with
nothing written. A refused release is settled as blocked, and nobody is
notified of what they decided. A check is refused within a minute of the
last, per service and for every service, however far back it looks, so
repeated clicks cannot spend a registry's rate limit.

### Updating by hand

The queue rechecks what is waiting against the compose file when it starts,
every `DUVA_SETTING_RECONCILE_INTERVAL`, and whenever a container is created, started,
stopped or removed. An entry goes once the file has what it offered, or has
moved somewhere it no longer leads from. The log says why each entry went.

### Updating duva itself

duva cannot replace its own container: recreating stops the old container
first, and the stop is the last thing it would do. So an update naming duva is
handed to a helper -- the same image, run as `duva self-update`, in a copy of
duva's own container without its compose labels or network aliases.

- A self-update is deferred while duva is applying anything else.
- On success the helper removes its own container, and the new duva's first
  reconcile clears the entry the helper's commit satisfied.
- On failure the helper is left, named `<container>-self-update-<time>` and
  labelled `duva.self-update`, so `docker logs` can say why. Remove it with
  `docker rm` once read.

## Configuration

Every setting is read through [configy](https://github.com/miista/configy), as
in every app here: `DUVA_SETTING_<KEY>`, the key upper-cased with `.` as `_`.
From the environment only: nothing edits settings from the page, so none is
stored, and a secret never lands in the database. An empty variable counts as
unset. A value that does not parse refuses to start, never falls back.

| setting | variable | |
|---|---|---|
| `repo` | `DUVA_SETTING_REPO` | the repository, mounted at the path it has on the host. Required |
| `compose_dir` | `DUVA_SETTING_COMPOSE_DIR` | the compose project, relative to `repo`; default its root |
| `host` | `DUVA_SETTING_HOST` | what this host is called on the page; default the hostname |
| `api_token` | `DUVA_SETTING_API_TOKEN` | the token a peer's page, or the CLI, must present to this host's API. Required |
| `ui` | `DUVA_SETTING_UI` | serve the page on 8081 |
| `ui.oidc.issuer`, `.client_id`, `.client_secret`, `.base_url` | `DUVA_SETTING_UI_OIDC_ISSUER`, ... | sign-in; all four required with `ui` |
| `peers` | `DUVA_SETTING_PEERS` | the other hosts, `host=url,host=url` |
| `peer.<host>.token` | `DUVA_SETTING_PEER_<HOST>_TOKEN` | each peer's `api_token`. A `-` or `.` in the host name is `_` |
| `commit_template` | `DUVA_SETTING_COMMIT_TEMPLATE` | the commit message; fields `.Container`, `.Image`, `.OldVersion`, `.NewVersion`, `.OldDigest`, `.NewDigest`. Unset gets a default; a template that does not render refuses to start |
| `git.push` | `DUVA_SETTING_GIT_PUSH` | push each commit; default false |
| `schedule` | `DUVA_SETTING_SCHEDULE` | when to look for new tags, 5-field cron; default `0 3 * * *`. Also once at startup |
| `since` | `DUVA_SETTING_SINCE` | override how far back the next check looks (RFC 3339, or a duration such as `168h`) |
| `reconcile_interval` | `DUVA_SETTING_RECONCILE_INTERVAL` | how often the queue rechecks what is waiting, and releases delayed ones; default `10m` |
| `update_timeout` | `DUVA_SETTING_UPDATE_TIMEOUT` | how long an apply may run before it is called failed; default `30m` |
| `ntfy.endpoint`, `ntfy.topic` | `DUVA_SETTING_NTFY_ENDPOINT`, `_TOPIC` | where to announce what waits for a person, and failed auto-applies |
| `ntfy.click` | `DUVA_SETTING_NTFY_CLICK` | the link a notification opens, usually the page |
| `ntfy.token` | `DUVA_SETTING_NTFY_TOKEN` | ntfy access token, when the topic needs one |
| `log_level` | `DUVA_SETTING_LOG_LEVEL` | trace, debug, info (default), warn, error |
| `read_only` | `DUVA_SETTING_READ_ONLY` | the page shows the queue and approves nothing |

A setting set under its old name (`DUVA_REPO`, `DUVA_QUEUES`,
`DUVA_QUEUE_TOKEN_PI`, `NTFY_TOKEN`, ...) refuses to start, naming the new
one: read under the old name it would be silently ignored, and duva would run
on a default nobody chose.

`/data` holds `duva.db`, a SQLite database -- what is waiting, the history of
every decision and who made it, the blocklist, the services duva knows, and
how far back each was checked -- and `auth/`, the page's session key. Losing
`auth/` signs people out. Backing it up is the host's job, as for any other volume. Copy `duva.db`
together with `duva.db-wal` (and `-shm`), or stop duva first; `sqlite3
duva.db "VACUUM INTO 'backup.db'"` on the host turns the copy into one
consistent file. Do not copy `duva.db` alone while duva runs: recent changes
may still be in the `-wal` file.

## Moving from v4

v5 replaces the four v4 containers on each host with one.

1. **Carry the state over**, or accept one noisy first check and an empty
   queue. Copy `queue.json` out of the v4 queue container's `/data` and
   `detector.json` out of the watcher's, with `docker cp`, into v5's `/data`
   before its first start. v5 imports them into its database on that start
   and renames them `.migrated`; going back to v4 can rename them back.
2. **Rename and drop variables.** Every variable is `DUVA_SETTING_*` now
   (see Configuration); duva names any it finds under the old name.
   `DUVA_WEBHOOK_URL`, `DUVA_UPDATE_URL` and
   `DUVA_UPDATE_TOKEN` are gone: nothing talks over a network inside a host
   any more. The page's local account and `DUVA_UI_AUTH_DISABLED` are gone.
3. **Set `DUVA_SETTING_API_TOKEN` on every host.** v4 minted a queue token
   when it was unset; v5 refuses to start.
4. **Move the page** from v4's UI container to `DUVA_SETTING_UI` on one
   host's duva, and point the reverse proxy at port 8081.
5. **Remove the v4 containers** and their volumes once v5 has checked once.

v4's updater cannot hand itself to v5 (they are different images), so the
switch is by hand: `docker compose up -d duva`, then commit the pin.

## Build

```sh
make build
make test
```

Releases are GoReleaser's, from `duva-v5/vX.Y` tags
(`../.goreleaser.duva-v5.yaml`). The integration suite builds the same image
from the same Dockerfile: `go test -tags integration ./test/...` from the
repository root.
