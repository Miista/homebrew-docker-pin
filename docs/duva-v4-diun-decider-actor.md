# duva v4: diun, decider, actor

**Status: built and working end to end**, on `feature/duva-v4-decider` —
`internal/decide`, `internal/actor`, `internal/diun`, `internal/dockerapi`,
`cmd/decider`, `cmd/actor`. Not deployed. Existing duva is untouched and
still running.

Verified in real containers as a real compose stack: a diun-shaped webhook
classified minor, queued, approved, pulled, repinned, container replaced,
committed as `app: v1.11 -> v1.12`, queue empty afterwards. The failure path
was verified first by accident — the actor could not find its compose
project, and the transaction put the file back exactly as it promises to.

What follows was the design note, and is now partly a description.

`duva-v3-monitor-handler.md` proposed carving duva into a monitor and a handler; this goes further and asks
what happens when the **detection** half is not duva's either.

The existing duva is not touched by any of this. It keeps running as it is.

## The idea

Three single-purpose tools, each answering one question:

| | question | who |
|---|---|---|
| **detector** | has a new matching version been published? | **diun** (exists, deployed) |
| **decider** | does it fit, how big is it, may it be applied? | new, small |
| **actor** | rewrite the file, replace the container, commit | duva's `apply.go`, extracted |

Plus a **UI**, which is a view over the decider's queue with one write verb.

```
diun ──webhook──▶ decider (the gate)
                     ├── auto ──▶ actor
                     └── pending ──▶ [queue] ◀── reads ── UI
                                        └── approve ──▶ actor
```

## Why diun is the detector

v3 left "what is actually gained?" honestly marked as speculative: the split
would let someone apply updates differently, but nobody wanted that yet. The
detector seam is different — it has a concrete occupant today.

diun already runs on both hosts. `DIUN_DEFAULTS_WATCHREPO=true` and
`DIUN_DEFAULTS_SORTTAGS=semver` mean it watches whole repositories and sorts
tags by version, so it discovers **new version tags**, not only moved digests.
Per-service constraints already exist as `diun.include_tags` /
`diun.exclude_tags` labels on ~45 services across both hosts.

That last point resolves something that looked like technical debt. duva reads
`duva.*` labels; every service carries `diun.*` ones; so duva currently
classifies almost nothing and the obvious conclusion was "migrate the labels".
If diun is the detector, those labels are **correct as they stand** and nothing
needs migrating.

Concretely, diun found authelia `4.39.25` while duva could not: duva has no
`duva.include_tags` on it, so it falls through to the moving-tag path and a
concrete tag's digest never moves.

The decider therefore needs **no registry access at all** — no tag listing, no
per-registry auth, no rate limits. That is most of what makes duva's detection
half complicated.

## Who needs what

| | docker socket | compose (read) | compose (write) | git |
|---|---|---|---|---|
| decider | — | yes | — | — |
| actor | read-write | yes | yes | yes |
| UI | — | — | — | — |

(diun is a dependency, not a component. What it needs is its own business.)

Two things worth drawing out.

**Neither tool talks to a registry.** The actor pulls through the daemon —
`Docker.Pull` and `Docker.GetDigest` in today's `apply.go` go over the mounted
socket, and the daemon does the registry talking with whatever credentials it
already holds. Neither ever constructs a registry request, handles a
`WWW-Authenticate` challenge, or lists tags. There is no column for it because
there is nothing to put in one.

So `internal/registry` belongs entirely to diun's side of the split and does
not come along at all: tag listing, bearer auth discovery, the Docker Hub tag
API for `TagCreated`. That is a whole package deleted from this design rather
than ported.

The exception is `Classify` and `CompareVersions`, which live in
`internal/registry` but are pure functions over version strings and are exactly
what the decider needs. They should move somewhere neutral during the
extraction rather than dragging the decider into importing a package it uses
five percent of.

**Nothing holds more than it must.** duva today gives every part the socket,
because any part might need to act. Here only the actor does.

## What the decider does

In: diun's webhook. The payload carries `image` (full reference, so the tag is
derivable), `digest`, `status`, `hostname`, and container `metadata`.

It does **not** carry the previous digest or a semver classification. So the
decider gets the current tag from what it already knows: **the compose file**.
That is the same source duva uses, and it means the decider is not purely
reactive — it holds a read-only view of what this host declares.

Then:

1. Map the webhook's image to a compose service.
2. Read the currently pinned tag and digest from the compose file.
3. Classify the change — patch / minor / major — from the tag pair.
4. Apply the service's auto threshold.
5. Emit to the actor (automatic) or hold in the queue (request).

Only the actor writes the compose file. The decider reads it. One of the three
can corrupt state, not three.

## The decider is the gate

Everything passes through it. The UI reads; the actor obeys. Two things follow.

**The queue lives in the decider, not the UI.** A pending update is a decision
awaiting confirmation, which is policy, and policy is the decider's. The UI can
die and come back without the queue going with it — which matters because the
display layer is the part most likely to be replaced.

**A newer candidate replaces the pending one** rather than queueing beside it.
Supersession falls out of the queue being keyed by service: you cannot approve
something that has already been overtaken. This is what `State.Reconcile` does
today, for the reason its own comment gives — a stale row invites approving
something that is gone.

## Locking

The actor locks while applying or while the queue is being updated. If a new
version arrives mid-apply, it waits.

The consequence worth stating: a waiting candidate is evaluated against the
compose file **after** the apply finishes, not against the state it arrived in.
A queued `4.39.20 → 4.39.25` may become `4.39.24 → 4.39.25`, or may evaporate
because the apply already landed something newer. That is correct — it
re-decides against reality rather than against a stale premise — but it means
the lock is held across "apply, then re-evaluate", not merely "apply".

## Failure and revert

**The actor owns the revert.** If the container refuses the new image, the
actor puts the file back, because the file would otherwise describe something
that never ran.

This makes "the actor is dumb" true only about *policy*. It is not dumb about
transactions: it holds the compose file, git, and the docker socket, and it
owns the whole five-step sequence — pull, write the pin, recreate, commit,
push — including what a failure at each step means.

## The HTTP surfaces

**diun, decider and actor are all same-host.** They reach each other over that
host's compose network by name. Only the UI crosses hosts, and it only ever
talks to the decider.

So there is **one published port per host** — the decider's. The actor is
entirely internal: nothing outside its own compose network can reach it.

**Decider**

| | caller | auth |
|---|---|---|
| `POST /v1/notify` | diun | none |
| `GET /v1/snapshot` | UI | token |
| `POST /v1/apply/{service}` | UI | token |
| `GET /v1/progress/{service}` | UI | token |
| `GET /healthz` | container runtime | none |

`internal/agent/server.go` is already most of this: snapshot, apply, progress,
healthz, bearer token on everything but health. What changes is that
`/v1/refresh` goes away — the decider checks nothing, diun does — and
`/v1/notify` arrives.

`/v1/notify` is unauthenticated because diun reaches the decider by container
name on the local network. It is still exposed on the published port, which is
worth being honest about rather than implying the network shape hides it.

What an unauthenticated notify can do is bounded: it names a container and a
digest, and the decider validates both against its own compose files. An
unknown container is a loud error. A known one still has to clear
classification and the service's `duva.auto` threshold — `none` everywhere
today — before anything reaches the actor. So the worst case is a forged
candidate appearing in the queue for a human to reject.

That is a judgement about a LAN, not a principle. If it stops being one, the
notify endpoint takes a token too, and diun's webhook notifier can send one.

**Actor**

Internal only. The decider calls it; nothing else can.

The contract is deliberately small, because **a different actor must be able
to take its place**. The decider may know an endpoint, a payload — which
service, in which file, from what, to what — and that the response carries a
stream URL. It may not know that applying involves a registry, a container or
git: an actor that opens a pull request touches none of them.

That rules out something an earlier draft of this note assumed. "Re-notify
until the compose file shows the target digest" is knowledge about *this*
actor's idea of applying, and a pull-request actor never changes the compose
file, so the decider would re-notify forever.

Instead the actor reports its own completion, and the decider runs a timeout.
**Completed means the actor did its job** — not that the host is running a new
image. A pull request actor completes when the pull request exists. The
timeout is what keeps an actor that died mid-apply from wedging the queue, and
it is the only thing the decider needs to believe about work it did not do.

Status comes through the **same stream** the UI watches rather than a second
endpoint: there is one thing happening, so there is one place to read about
it. The decider **tees** — one connection to the actor, relayed verbatim to
the UI while it watches for the end.

The end is a line with a known prefix:

```
pulling example.com/app:1.2.3
recreating app
status: completed
```

A prefix rather than a typed event or a separate result call, because the
stream has two readers with different needs — the decider, which must
recognise the end, and a person, who should see something readable. A status
nobody defined is *not* terminal: treating an unrecognised one as the end
would let a typo in an actor read as success, where leaving it as progress
means the timeout catches it.

### The components version independently

The contract is the only thing the two sides share, so it is the only thing
with a compatibility obligation — which means the decider and the actor are
released on their own schedules rather than in lockstep.

The `/v1` in the paths is load-bearing for that, not decoration. Adding a
field is backwards compatible in both directions, since each side ignores what
it does not recognise; renaming or removing one is not, and means a `/v2`
served alongside `/v1` until both have moved. **Add freely, never rename in
place.**

Nothing yet tells a decider which contract version an actor speaks — today it
would POST and find out. A health endpoint advertising the versions supported
would let it fail at startup instead, on the same reasoning as refusing to
start on an unwritable `/data`. Not worth building with one actor, but worth
leaving room for.

`POST /v1/apply` starts the transaction and returns a URL to stream progress
from. **The decider proxies that stream** — a clean passthrough, copying bytes
without interpreting them.

The passthrough is what preserves the property worth having. The actor still
owns the shape of its own progress: a different actor — one that opens a pull
request, or defers to Ansible, which is what v3 imagined this seam buying —
streams whatever it likes, and the decider relays it unchanged. The decider is
a pipe, not a participant, and learns nothing about what an apply looks like.

Proxying rather than redirecting also means the actor never needs to know its
own externally-reachable address, which inside a container it cannot infer: it
sees a compose-network address and its own container name, neither of which
resolves from wherever the UI runs. Nothing has to be configured with a public
base URL, because nothing outside the host ever addresses the actor.

The stream is empty when no update is in progress, which is the whole of its
error handling.

## Carried over from v3, unchanged

- **Transport is a webhook POST.** Not a queue, not a socket.
- **The decider notifies when an update *should be applied*,** not when it
  becomes available. Soak, policy and approval resolve on the decider's side;
  the actor is told about work already cleared to happen.
- **Re-notify until the compose file shows the target digest.** The actor needs
  no delivery guarantees, no retry logic and no queue of its own: one that was
  down comes back and is told again. Retry is a property of the protocol.
- **The actor must therefore be idempotent** — told twice about the same update
  must not produce two commits. "Already handled" is not a flag anyone keeps;
  it is `compose file digest == target digest`.
- **No new baseline state.** The compose file is the record.

## Topology

The actor must run where the socket and the compose file are, so decider and
actor are per host. diun already is.

The UI is **one instance over all of them**, and the deciders are **registered
with it** — the same arrangement Dockhand uses for its agents, and the same one
the hub shipped in `feature/duva-v2` already implements: a static list of
`host=url`, dialled outbound, rows labelled with the configured host rather
than whatever the far end calls itself.

```
  host A:   diun ──▶ decider-A ──▶ actor-A
  host B:   diun ──▶ decider-B ──▶ actor-B

                 UI ──reads──▶ decider-A
                    ──reads──▶ decider-B      one page, every queue
```

The UI stays a separate process rather than moving inside the decider. It is
only a thin view on the queue, which is an argument for folding it in — but the
decider is per host, so a UI inside it means one page per host, which is the
thing having a UI at all was meant to fix.

Configured, not discovered, for the reason the hub already documents: at this
scale a registration protocol adds an inbound path by which something could
claim to be a decider, to solve a problem a two-line config solves.

## What is new and what is extraction

Mostly extraction, which is the argument for the shape being right:

| piece | source |
|---|---|
| detector | diun, unchanged |
| current tag lookup | `internal/compose` (`RawImage`, `ParseImage`) |
| classification | `internal/registry` (`Classify`, `CompareVersions`) — to move somewhere neutral |
| threshold | `internal/watch` (`Decide`, `ParseAuto`) |
| queue + supersession | `internal/watch` (`State.Reconcile`) |
| actor | `cmd/duva/apply.go`, `transaction.go`, `recreate.go` |
| UI | `internal/ui` |
| UI transport | `internal/agent`, `internal/hub` — deciders replace agents |

Genuinely new: a webhook receiver, the service-mapping rule, the lock, and
the actor's internal HTTP surface with its progress stream.

## The webhook payload

Captured from diun 4.33 against a real container, not read from docs:

```json
{
  "diun_version": "v4.33.0",
  "hostname": "b110ea828afd",
  "status": "new",
  "provider": "docker",
  "image": "docker.io/fnsys/dockhand:latest",
  "hub_link": "https://dockhand.pro",
  "mime_type": "application/vnd.oci.image.index.v1+json",
  "digest": "sha256:4e0c30e703f1...",
  "created": "2026-09-02T14:38:12Z",
  "platform": "linux/amd64",
  "metadata": {
    "ctn_id": "9dca70d6637a...",
    "ctn_names": "dockhand",
    "ctn_state": "running",
    "ctn_status": "Up 2 hours (healthy)",
    "ctn_command": "...",
    "ctn_createdat": "...",
    "ctn_size": "0B"
  }
}
```

`status` is `new` on first sight and `update` when something moved. Only the
latter is a signal; the former is diun recording a baseline.

There is no previous digest and no semver classification, which is why the
decider reads the compose file for the current tag.

## Mapping a webhook to a service

Settled by the payload above: **`metadata.ctn_names`**.

The decider already parses the compose files to read the current pinned tag,
so it builds `container_name -> (service, file)` from the same parse and looks
the webhook's container name up in it. Every one of the 71 services across
both hosts sets `container_name:` explicitly, so the map is total.

An image reference would not do: `caddy`, `ofelia`, `cloudflared` and `restic`
each run on more than one host, and two services on one host can share an
image. A container name is unique per host, and diun runs per host — so the
host ambiguity disappears without anyone having to encode it.

**The decider therefore needs no docker socket.** Compose files in, decisions
out. Only the actor talks to docker, which is a better privilege story than
duva has today, where everything holds the socket because anything might act.

The one risk is a future service with no `container_name`, which would be
invisible. A webhook naming a container the decider cannot map must be a loud
error, not a silent drop — the same reasoning as refusing to start on an
unwritable `/data`: discovering it later means discovering it after something
was already missed.

(`ctn_id` would also work and is what the container's own compose labels key
off, but reading those needs the socket. The name is enough and costs nothing.)

## The soak is not a separate thing

`duva.delay` holds a release back until it is old enough. That is the same
structure as a pending update awaiting a person: an entry in the queue that is
not yet released. The gate differs — a clock rather than a human — but the
queue does not.

So there is one queue with two release conditions, not a queue and a soak
list. `State.Soaking` and `ReconcileSoaking` go away.

## Policy lives on the container

`duva.auto` stays a label on the service it governs, alongside what kind of
update may be applied automatically. Config that governs a service lives with
that service, which is the argument the existing labels were chosen on.

A service therefore carries `diun.*` for detection and `duva.*` for policy.
Two namespaces, but they answer to two different tools — which is the point of
the split rather than a wart in it.

## Open questions

**Is this better than what runs today?** Today's duva works and is tested end
to end. The honest case for v4 is that diun already detects what duva cannot,
and that the pieces are mostly extraction rather than new code. That is a
better case than v3 had, but it is still a rewrite of something that is not
broken.

## Prior art, checked

Worth recording, because it was not obvious and shapes what is actually novel
here:

- **Renovate** and **Updatecli** already update a compose file's tag *and*
  digest together and commit — Renovate's `docker:pinDigests` writes
  `image:tag@sha256:...` and keeps the tag "for readability", which is the same
  rule as "the tag is the tag to follow". Both self-host; Renovate supports
  Forgejo and Gitea. Neither touches containers: Updatecli's docs say plainly
  that recreating "remains your responsibility".
- **Dockhand** manages containers well but will not apply a version-tag change
  at all (advisory badge only, even for a stack it has adopted), and a digest
  pin suppresses its digest-move detection entirely — verified by experiment,
  not read from docs.

So the gap nothing else fills is **edit the file and replace the container as
one transaction**. That is the actor, and it is the piece worth keeping
whatever else changes.
