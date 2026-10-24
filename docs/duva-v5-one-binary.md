# duva v5: one binary

**Status: phases 1 and 2 built** on `feature/duva-v5` (2026-10-20), in
`duva-v5/`. Decisions locked 2026-10-19. Soak
(`duva.soak`, commit `2c95716`) landed in v4 first and carries over. Where
phase 1 came out differently from this note, the section says so.

v4 split detection, gating, applying and the UI into four containers so that
each held only what it needed. On a real host the first three run on the same
machine, as the same user, with the same lifecycle. They are deployed and
upgraded together and never apart. The split's privilege story is mostly gone,
and what is left is the cost: four images, two internal HTTP hops with their
own tokens, an outbox to survive one of those hops failing, and restarts to
make one component re-read what another knows.

v5 keeps the responsibilities and drops the processes. It is done in two
phases:

1. **Consolidate.** One binary, one image, one container per host.
   Components call each other in-process. State stays in the JSON files
   v4 already has.
2. **SQLite.** Replace the JSON files with one database, and with it the data
   model that several releases per service, a blocklist and an audit trail
   need.

Phase 1 changes how duva runs and not what it decides, so the existing
integration suite can tell whether the merge alone broke anything.

## Decisions

| | decision |
|---|---|
| shape | one `duva` binary and image; watch, queue, updater and UI are components in one process |
| UI | in-process, turned on by `DUVA_UI` on **one** host, on port 8081 apart from the queue API's 8080; one page over both hosts, as today |
| other hosts | reached by the UI through the queue API |
| identity | `container_name`, as today |
| policy | read from the **running container's** labels, not the compose file |
| recreate | the API clone, as today (`dockerapi.Recreate`); the compose file only supplies the pin |
| removed | `/v1/notify`, `internal/watchevent`, the updater's HTTP API, `DUVA_WEBHOOK_URL`, `DUVA_UPDATE_URL`, `DUVA_UPDATE_TOKEN` |
| auth | always on; OIDC required; refuse to start if it is misconfigured; no local account; `DUVA_UI_AUTH_DISABLED` removed |
| queue API token | required; refuse to start without `DUVA_QUEUE_TOKEN`; no minted fallback |
| versioning | own `duva-v5/vX.Y` tags; v4 keeps running until v5 replaces it on both hosts |
| store (phase 2) | `modernc.org/sqlite` |

### Accepted risk: the UI shares a process with the docker socket

The UI is reachable from the internet behind OIDC, and in v5 it runs in the
process that holds the docker socket. An authentication bypass has the same
impact either way: the UI's fixed actions (approve, and later discard, block,
re-scan), which it can already perform today. There is no generic command
path. What sharing the process adds is the case of **code execution** in code
that runs before sign-in: the HTTP stack, the OIDC/JWT libraries, authy. That
case would be root on the UI host instead of the queue token. In Go, and in
that code, this is a low-probability, high-impact case, and it was accepted for
the sake of one deployment. Revisit if the UI grows code that parses
unauthenticated input beyond authy's.

### Policy comes from the running container

A label only changes on a running container when the container is recreated,
so the policy duva acts on is exactly what is deployed, and there is nothing
to drift. A label edited in the compose file takes effect when the container
is recreated, by `compose up` or by duva's next apply of it.

This is safe with the API clone because the clone copies the running
container's configuration and takes only the image from the file. An edit to
the same service that was committed but never deployed is not deployed by an
apply. Do not switch the updater to `compose up -d <service>` without
revisiting this: compose would deploy that edit with the image.

v4 read policy from the compose file only because the queue held no socket.
v5's process holds one.

### Why not merge the four containers' configuration

This was tried on paper first. Run as they are, the binaries do not fit in one
container:

- The queue and the updater both hard-code `:8080` (`queue/main.go`,
  `update/main.go`), so the second to start fails to bind.
- The watcher's `/data` is owned by `nonroot` (65532), the queue's by uid 1000,
  and the updater must run as the repository's owner in the host's `docker`
  group. One container has one `user:`.
- The watcher and queue images are distroless with no shell, so there is
  nothing to start several processes with.
- A container gets one `HEALTHCHECK`.

Fixing all of that is the restructure. A supervisor running four binaries
would pay its cost and get none of its benefit.

## What stays the same

The rules about **responsibilities** in `CLAUDE.md` and the v4 note carry over.
They are now enforced by package boundaries instead of process boundaries:

- **watch knows nothing about versions.** It cannot import the version
  package. Its state is disposable: losing it costs one noisy run.
- **queue knows nothing about docker, registries or git.** The updater is still
  behind `internal/update`'s contract (`docs/update-contract.md`), so an
  updater that opens a pull request still plugs in. `completed` still means
  the updater did its job.
- **No rollback.** Everything answerable is answered in a precheck. A failed
  recreate is left alone, and `IsClean` blocks the next apply.
- **The updater never recreates its own container in place.** See
  [Self-update](#self-update).
- **One shared `http.Client`** for every registry call.
- **The tag is the instruction, the digest is the record.**
- The UI refuses to start with no queues configured, and an unreachable queue
  suppresses "nothing waiting".

## Phase 1: consolidate

### Process

- `duva serve` runs watch, queue and updater as goroutines, and the UI as well
  when `DUVA_UI` is set. Each component has its own `recover`: a panic is
  logged and the component restarted, so a watcher bug does not take the
  updater down mid-apply. A component that keeps failing exits the process,
  and Docker's restart policy handles it.
- `duva health` for the `HEALTHCHECK`, reporting every component that is
  running.
- Runs as the repository's owner in the host's `docker` group, the updater's
  existing requirement, which now covers the whole process.
- One image: Alpine plus git, which is the updater's base today. The
  released-image-must-match-tested-base rule now covers one image.
- One mount for the repository, at its host path, read-write. One `/data`.

### Communication

| v4 | v5 |
|---|---|
| watcher POSTs `/v1/notify`, retries from an outbox | `queue.Accept(findings)` |
| queue POSTs the updater and follows its stream | `updater.Apply(req, progress)` |
| updater `/ready`, `/applicable` over HTTP | method calls |
| re-scan = delete the watcher's state file and restart it | `watch.Check(service)` |
| UI → queue API for every host | in-process for its own host, the queue API for the other |

The outbox in `detector.json` goes: a call within a process cannot be lost the
way a POST can. Until phase 2 makes the handoff transactional, the order is
"enqueue, then advance the cutoff". A crash between the two re-reports a
finding, which the queue already deduplicates.

### Policy from the daemon

The queue's `Lookup` reads labels from the running container through the
socket (by `container_name`), instead of the compose file. The pin, and the
tag to follow, still come from the compose file, because that is what the
updater rewrites.

Docker events (create, start, stop, destroy) trigger the re-checks that run on
a tick today: reconcile, release of a soak held for a stopped container, and
the applicable check. The tick stays as a fallback, since events are missed
while duva itself restarts.

### Self-update

The container being replaced is now all of duva. The self-update helper stays
as it was, run as `duva self-update`.

**As built, no outcome file was needed.** The plan was for the helper to
report its outcome through `/data` for the new process to read. In practice
the existing pieces already settle it: a self-update is deferred while
anything else is applying (the drain), and on success the helper commits the
new pin, so the replacement's first reconcile drops the entry as satisfied.
On failure the old process is still running and follows the helper's stream
as before. `TestV5DuvaUpdatesItself` holds that the entry leaves the queue.

### State

Unchanged in format: `queue.json` and `detector.json` under one `/data`,
owned by the single runtime user, with the page's session key in
`/data/auth`. The watcher keeps its outbox: a finding the queue could not take
(its compose file unreadable for a moment) is held and offered again.

**As built, v4's state carries over by copying, not code**: the formats are
unchanged, so `docker cp` of the two files into v5's `/data` before its first
start is the migration (`duva-v5/README.md`, "Moving from v4").

### Configuration

What is left after the removals: settings, read through configy as
`DUVA_SETTING_<KEY>` (`schedule`, `reconcile_interval`, `update_timeout`,
`commit_template`, `git.push`, `ntfy.*`, `log_level`, `read_only`, `since`),
and the plain variables needed before the store opens or that would lock
people out if edited: `DUVA_HOST`, `DUVA_REPO`, `DUVA_COMPOSE_DIR`,
`DUVA_QUEUE_TOKEN` (required), and with the UI on: `DUVA_UI`, `DUVA_QUEUES`
(the **other** hosts), `DUVA_QUEUE_TOKEN_<HOST>`, `DUVA_UI_OIDC_*` (required).

### Testing

The harness and fixtures stay. `test/integration` builds one image instead of
four, and each fixture's three duva services become one. The existing v4
scenarios must pass unchanged in what they assert. New ones:

- a panic in one component does not stop the others;
- policy follows the running container: a label edited in the file is not
  acted on until the container is recreated;
- self-update: the queue's state survives the handoff;
- the UI refuses to start with OIDC misconfigured, and the queue API refuses to
  start without a token.

## Phase 2: SQLite

`modernc.org/sqlite`: pure Go, so `CGO_ENABLED=0` builds and cross-compiles
keep working. Measured on 2026-10-19 against a 1.5 MB stub: +4.8 MB and 25
modules (v1.60.1), against +6.1 MB and 15 modules for `ncruces/go-sqlite3`
(v0.35.6, pre-1.0). For comparison, go-containerregistry was rejected at
6.9 MB and 48 modules. The "standard library plus yaml.v3" rule guards against
sprawl, and a whole data layer earns this.

- WAL mode, one writer connection.
- Versioned migrations as embedded `.sql` files plus a `schema_version` table.
- Backups with `VACUUM INTO`, never a `cp` of the live file.
- Unit tests against a real database file in `t.TempDir()`, not a fake.

### Data model

```sql
images (
  id    INTEGER PRIMARY KEY,
  repo  TEXT NOT NULL UNIQUE          -- example.com/app, no tag or digest
)

services (                            -- mirrors the containers; identity and lifecycle only
  id          INTEGER PRIMARY KEY,
  container   TEXT NOT NULL UNIQUE,   -- container_name: the identity
  image_id    INTEGER NOT NULL REFERENCES images,
  removed_at  TEXT                    -- disabled, never deleted
)

watch_cutoffs (                       -- watch's; deletable at any time
  service_id       INTEGER PRIMARY KEY REFERENCES services,
  last_checked_at  TEXT NOT NULL
)

queue_entries (
  id           INTEGER PRIMARY KEY,
  service_id   INTEGER NOT NULL REFERENCES services,
  from_ref     TEXT NOT NULL,
  to_ref       TEXT NOT NULL,         -- tag, or digest for a digest move
  tag          TEXT NOT NULL,         -- the tag to follow afterwards
  digest       TEXT,                  -- index digest, once known
  kind         TEXT,                  -- patch/minor/major/unknown; NULL = digest move
  published_at TEXT,
  found_at     TEXT NOT NULL,
  release_at   TEXT,                  -- soak; NULL = waits for a person
  status       TEXT NOT NULL,         -- waiting | soaking | applying | completed
                                      -- | failed | discarded | superseded | blocked
  why          TEXT NOT NULL
)

queue_events (                        -- append-only audit trail
  id          INTEGER PRIMARY KEY,
  entry_id    INTEGER REFERENCES queue_entries,
  service_id  INTEGER REFERENCES services,
  at          TEXT NOT NULL,
  kind        TEXT NOT NULL,          -- found, soaking, released, approved, applied, ...
  actor       TEXT NOT NULL,          -- "duva", or the OIDC identity
  policy      TEXT,                   -- the duva.auto / duva.soak in force, as read
  detail      TEXT
)

blocked_releases (
  id            INTEGER PRIMARY KEY,
  image_id      INTEGER NOT NULL REFERENCES images,
  digest        TEXT NOT NULL,        -- index digest; never a tag
  reason        TEXT NOT NULL,
  blocked_by    TEXT NOT NULL,
  blocked_at    TEXT NOT NULL,
  unblocked_at  TEXT,
  UNIQUE (image_id, digest)
)
```

- **`services` holds no policy.** Policy is read from the running container at
  each decision, and each event records the policy that was in force. That
  gives history without a stored copy that could drift.
- Foreign keys go into `services`, never into `watch_cutoffs`, which is what
  keeps watch's state disposable. A queue entry copies what was found instead
  of referencing a watcher row.
- The foreign key from `queue_entries` to `services`, plus `removed_at IS NULL`
  at enqueue, means nothing can be queued for a service that does not exist.
- No `ON DELETE CASCADE` on history.
- The watch → queue handoff becomes one transaction: enqueue and advance the
  cutoff together.

**As built, not one transaction.** The queue already ignores a release it
holds, and the watcher's held findings already cover a queue that could not
take one, so "enqueue, then advance the cutoff" is safe across a crash at any
point: the worst case is a re-report the queue drops. A shared transaction
would have meant splitting the queue's handler into decide, persist and act
across components, for no correctness gained. The watcher's tables are keyed
by service name and reference nothing.

### The service sync

Driven by docker events, with the tick as a fallback. A new container is
inserted. A container that is gone gets `removed_at`; one that comes back has
it cleared, which keeps the same row and the same history. A changed tag
supersedes that service's open entries, since they are about the old stream.
Every change is an event.

### Several releases per service

v4's queue is keyed by service, so a newer release replaces a soaking one, and
the wait starts again from the newer release's publish time. A project that
ships faster than its soak never applies. In phase 2 every candidate is its own
row with its own `release_at`. Supersession moves from enqueue time to apply
time: applying a release marks every **older** open entry for that service
`superseded`. A release that falls due while a newer one is soaking is applied.

### Actions

| Action | Does | Comes back? |
|---|---|---|
| **Approve** | applies now, skipping any soak | — |
| **Discard** | entry → `discarded` | yes, on the next scan |
| **Re-scan** | delete the service's cutoff, `watch.Check(svc)` | yes; that is the point |
| **Block** | add to `blocked_releases`; matching open entries → `blocked` | no, until unblocked |

Block is by index digest, never by tag, and scoped to the image. Every UI
action is an event carrying the OIDC identity.

**As built:** a version change's digest is not known when it is queued -- it
is learned on pulling -- so "the digest always comes from an entry" did not
hold. Blocking a version change resolves what its tag points at now, through
the registry client, and the updater checks the blocklist after every pull,
returning a new terminal status, `blocked`. That second check is also what
catches a release queued or approved before the block. Until the page has
buttons, the actions are `duva discard|block|unblock|check|rescan`, through
the queue API.

## Deferred

- **TLS from the UI to another host's queue.** Required eventually. The likely
  route is a publicly trusted certificate for an internal name, through each
  host's reverse proxy with DNS-01. No self-signed certificates: without
  pinning they protect nothing against a man-in-the-middle.
- Socket proxy and container hardening (`read_only`, `cap_drop`,
  `no-new-privileges`).
- `/healthz` on loopback only.
- The UI for several releases per service, discard, block, re-scan and
  history.
