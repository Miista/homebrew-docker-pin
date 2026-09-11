# duva v4: diun, decider, actor

Idea, not a decision. Nothing here is implemented. `duva-v3-monitor-handler.md`
proposed carving duva into a monitor and a handler; this goes further and asks
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

The UI is central, over every host's decider — the agent/hub split shipped in
`feature/duva-v2` is exactly this shape, and it would be a shame to end up with
one UI per host again.

```
host A:  diun ──▶ decider ──▶ actor
host B:  diun ──▶ decider ──▶ actor
                     ▲
                  central UI
```

## What is new and what is extraction

Mostly extraction, which is the argument for the shape being right:

| piece | source |
|---|---|
| detector | diun, unchanged |
| current tag lookup | `internal/compose` (`RawImage`, `ParseImage`) |
| classification | `internal/registry` (`Classify`, `CompareVersions`) |
| threshold | `internal/watch` (`Decide`, `ParseAuto`) |
| queue + supersession | `internal/watch` (`State.Reconcile`) |
| actor | `cmd/duva/apply.go`, `transaction.go`, `recreate.go` |
| UI | `internal/ui` |
| transport | `internal/agent`, `internal/hub` |

Genuinely new: a webhook receiver, the service-mapping rule, and the lock.

## Open questions

**Mapping a webhook to a service.** diun sends an image reference, not a
compose service name, and the same image can back more than one service —
`caddy`, `ofelia`, `cloudflared` and `restic` all run on both hosts. The
payload's `metadata` carries container id and names, which is probably enough,
but the rule needs writing down: acting on the wrong container is the worst
failure this design can have.

**Does the decider still need the soak?** `duva.delay` holds a release back
until it is old enough. diun reports on discovery, so the decider would have to
hold the candidate itself and re-evaluate on a timer — which is state with a
clock, and the only part of the decider that is not purely reactive.

**What replaces `duva.auto`?** The threshold has to live somewhere. Another
label on the service is the obvious answer and keeps config next to what it
governs, but it means a service carries both `diun.*` (detection) and
something-else (policy), which is two namespaces for one concern.

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
