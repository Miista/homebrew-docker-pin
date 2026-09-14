# The actor contract

What a decider requires of an actor, so a different one can be written.

`duva-v4/actor` is the reference implementation — it pulls, rewrites the pin,
recreates the container and commits. It is not the only possible one. An actor
that opens a pull request, writes to a config service, or files a ticket for a
human is equally valid, and this document exists so that writing one does not
mean reading the reference actor's source to guess at what is required.

The whole of what the decider knows is below. If something is not here, the
decider does not rely on it, and an actor may do anything it likes about it.

## What an actor is

Something that takes "this service should move from A to B" and makes it so,
by whatever means it considers applying.

Three endpoints, one payload type, one line format. Everything else — a
registry, a container, a working tree — belongs to the actor and the decider
must never learn about it.

**Completed means the actor did its job**, not that the host is running a new
image. A pull-request actor completes when the pull request exists. This is
the single most important thing to get right when writing one: reporting
`completed` for work that has not happened is how an entry leaves a queue
having changed nothing.

## Endpoints

The actor is reached by the decider on its own host network. It is not
published, so the token below says who is asking rather than hiding what is
said.

### `POST /v1/apply`

Take one piece of work. Authenticated with `Authorization: Bearer <token>`
when the actor was given one.

Request body:

```json
{
  "service": "gluetun",
  "file":    "/compose/optiplex/media/docker-compose.yml",
  "image":   "qmcgaw/gluetun",
  "from":    "sha256:8e92dcb…",
  "to":      "sha256:a2d9649…",
  "tag":     "latest",
  "digest":  "sha256:a2d9649…"
}
```

| field | meaning |
|---|---|
| `service` | the compose service name |
| `file` | the compose file it is declared in — not necessarily the project root, since `include:` is resolved. Carried so the actor need not re-derive what the decider already knows. |
| `image` | the base reference, no tag, no digest |
| `from`, `to` | the change. **Tags** for a version change; **digests** for a moving tag whose digest moved. |
| `tag` | the tag to follow afterwards. Carried separately from `to` because the tag is an *instruction*, not a description of what is pinned: a digest move leaves it alone, a version change sets it. |
| `digest` | what to end up on, when the decider knows it. Empty for a version change, where the actor resolves it by pulling the tag. |

Answer, `200`:

```json
{ "stream": "/v1/stream/gluetun" }
```

`stream` may be absolute or relative to the actor's base. **The decider
follows it and never constructs it**, so an actor is free to key its streams
however it likes — by service, by a run id, by anything.

Returning `200` means *accepted*, not *done*. An apply takes minutes; a caller
holding the request open for it would time out long before the work finished.

Refuse with any non-2xx and a plain-text or JSON body saying why. A refusal
here means the work never began, and the decider leaves the entry queued.

### `GET /v1/stream/…`

Progress, as newline-delimited plain text, flushed as it happens.

```
pulling qmcgaw/gluetun@sha256:a2d9649… for gluetun
pinned gluetun to qmcgaw/gluetun:latest@sha256:a2d9649…
recreating gluetun
committing "gluetun: latest digest moved"
status: completed
```

(That is the reference actor's wording, shown to make the shape concrete. None
of it is required — only the last line is.)

Unauthenticated in the reference actor, deliberately: it returns nothing when
no work is running, so there is no session to guess at, and guarding it would
mean a token for reading a log that is about to be shown to whoever asked for
the update anyway. An actor may guard it if it prefers.

The stream has two readers with different needs — the decider, which must
recognise the end, and a person watching a UI, who should see something
readable. Everything before the terminal line is relayed verbatim and the
decider does not parse it. Write whatever a person would want to read.

**The terminal line** is the last thing written, and the stream ends after it:

```
status: completed
status: failed: the container refused the new image
```

Only `completed` and `failed` are terminal. **A status nobody defined is not
terminal** — a typo would otherwise read as success, where leaving it as
progress means the decider's timeout catches it.

`failed` may carry a reason after a second colon-space. It is shown to a
person, so write it for one.

### `GET /v1/ready`

Whether work offered *now* would be attempted.

```json
{ "ready": false, "reason": "the repository has uncommitted changes, and applying commits — commit or stash them and this clears on its own" }
```

Unauthenticated: it names no service and carries no secret, and the UI must be
able to show why a button is grey without holding a token.

This exists because a UI that offers a button for work the actor would refuse
is worse than one that greys it — the person has already decided by the time
they find out. The reference actor answers `false` when its repository has
uncommitted changes, because applying commits and committing on top of
somebody's half-finished edit is never wanted.

Answer `{"ready": true}` if there is nothing that would stop you. An actor
that cannot tell should say ready rather than claim to be broken.

`reason` is shown to a person verbatim and is **not** parsed. Say what is
wrong and, if you can, what clears it.

Note what this is *not*: it is not "is the repository clean". That is the
reference actor's reason, and the contract must not learn it — an actor that
opens a pull request has no working tree to be dirty. What every actor can
answer is whether it would take work.

Answer it **per call**, not from a cache. What blocks an actor usually stops
blocking it without anything restarting, and a cached `false` leaves a button
grey after the cause is fixed.

### `GET /healthz`

`200` and anything in the body. For a container runtime, so unauthenticated.

## What the decider does about all this

It calls `/v1/apply`, follows the `stream` it is given, tees the lines to
whoever is watching, and waits for a terminal line.

It runs a **timeout** — `DECIDER_APPLY_TIMEOUT`, 30 minutes by default. That
is the only thing it has to believe about work it did not do: an actor that
died mid-apply would otherwise hold its entry out of the queue forever,
invisible to the person who could have noticed.

An approved entry **leaves the queue when it is accepted**, not when it
finishes. The queue answers "what needs a decision", and this one has had its
decision. If the apply fails, the detector reports the tag again and it comes
back — which is the protocol working, not state being lost.

The decider never retries an apply itself. Retry is a property of the detector
running again.

## Versioning

The `/v1` in the paths is load-bearing. The contract is the only thing the two
sides share, so it is the only thing with a compatibility obligation, and that
is what lets the decider and the actor be released independently rather than
in lockstep.

- **Adding a field is backwards compatible.** An older actor ignores what it
  does not recognise; an older decider ignores what it is sent.
- **Renaming or removing one is not**, and means a `/v2` served alongside
  `/v1` until both sides have moved.

So: add freely, never rename in place.

## Writing one: the short version

1. Accept `POST /v1/apply`, return `{"stream": "…"}`, start working.
2. Write progress lines a person would want to read.
3. Finish with `status: completed` or `status: failed: why`.
4. Answer `GET /v1/ready` honestly.
5. Answer `GET /healthz`.

And the one rule that is not an endpoint: **only say `completed` when you have
actually done your job**, whatever that means for you. The decider believes
you, and the entry is gone from the queue either way.

## Deployment, for an actor that touches a repository

Not part of the contract — an actor that touches no repository can ignore all
of it — but the reference actor carries requirements worth knowing about
before writing a similar one. See `duva-v4/README.md`: it must run as the user
that owns the repository and be in the `docker` group, and it checks both at
startup rather than discovering them halfway through an apply.
