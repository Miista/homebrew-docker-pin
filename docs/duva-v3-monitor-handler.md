# duva v3: monitor and handler as separate processes

Idea, not a decision. Nothing here is implemented, and the case for doing it at
all is argued below rather than assumed.

`duva-v2.md` is titled "detector + actor" and describes both halves living in
one process. That is what shipped: duva detects, decides, and applies — pull,
pin, recreate, commit. This note asks what happens if the actor moves out of
duva entirely.

## The idea

duva becomes only the **monitor**. It watches pinned services, applies the
policy ladder, holds the soak, keeps the approval queue, and decides *when an
update should be applied*. It does not apply it.

Applying becomes a **handler**: a separate process that receives a webhook and
does whatever "apply" means in that deployment. A reference handler ships in
this repo — the one that does what duva does today.

The unix argument: duva's two halves answer different questions. "Has this
moved, and may it move?" is a policy question about registries and time.
"Rewrite this file, replace this container, commit this" is a mechanical
question about one host. They are coupled today only because they started in
the same binary.

## What was settled

From the conversation that produced this note:

- **Transport is a webhook POST.** Not a queue, not a socket.
- **duva notifies when the update *should be applied*, not when it becomes
  available.** The distinction is the whole point: soak, policy and approval
  all resolve on duva's side, and the handler is told about work that is
  already cleared to happen.
- **Two channels, two cadences.** ntfy fires **once**, to ask a human for
  approval. The handler is notified **on every run** for as long as it has not
  handled the update.
- **The payload carries what is needed to do the job.** Not a pointer to state
  the handler has to fetch.
- **The approval queue stays in duva.** Approval is policy.
- **No new baseline state.** "The baseline is literally recorded in the compose
  file." The file is the record; neither side needs a separate ledger of what
  is currently pinned.

## Why re-notify every run

This is the load-bearing choice and worth stating plainly.

A fire-and-forget webhook makes the handler's uptime part of duva's
correctness: one refused connection and the update is silently dropped. Instead
duva keeps telling the handler about work it has not observed completing, every
run, until the compose file shows the new digest.

The consequence is that **the handler needs no delivery guarantees, no retry
logic and no queue**. A handler that was down comes back and is told again. A
handler that crashed mid-apply is told again. Retry becomes a property of the
protocol rather than a feature either side implements.

It also means the handler must be **idempotent**: being told twice about the
same update must not produce two commits. Since the compose file is the record,
the natural implementation is to check whether the file already says the target
digest and do nothing if it does.

"Has not handled the update" is therefore not a flag duva keeps. It is
`compose file digest != target digest`, which duva already computes every run.

## Open questions

Genuinely open — these are the reasons this is a note and not a plan.

**What does the handler do on failure?** Today duva reverts the file write when
`compose up` returns non-zero, because it *knows* the file describes something
that never ran (see duva-v2.md). Split across a process boundary, who owns that
revert? If the handler does, it needs the same write access to the compose file
and git that duva has, and "separate responsibility" starts to look thin.

**Does the handler need state at all?** The re-notification design says no. But
a handler that is mid-apply when duva's next run fires would be told to start
again — so either applies must be fast relative to the schedule, or the handler
needs an in-flight marker, which is state.

**Does the handler ever see an unapproved update?** Presumably not, since duva
notifies only what is cleared. Worth confirming: it decides whether the
approval UI can offer "apply now" as anything other than "notify sooner".

**Is the reference handler `docker pin`'s existing apply path, or new?** Most of
what a handler does — pull, pin, recreate, commit — already exists in
`internal/pin` and `cmd/duva/apply.go`. If the handler is a thin wrapper around
those, the split is real but small. If it is not, there is duplication.

**What is actually gained?** The honest version: today's single process works,
is tested end-to-end, and the coupling has not caused a problem yet. The split
buys the ability to apply updates some other way — a handler that opens a pull
request instead of committing, or one that defers to Ansible — without duva
knowing. That is a real benefit, but it is speculative until someone wants one
of those.

## Relationship to what exists

This does not supersede `duva-v2.md`. v2 is shipped and works; this would carve
a seam through it. If it happens, the monitor half is almost entirely the
current `internal/watch` plus the state and UI in `cmd/duva/main.go`, and the
handler half is `cmd/duva/apply.go` and `transaction.go` behind an HTTP
endpoint.

The invariant does not change: every compose image stays digest-pinned, every
change is a git commit, and the tag is the tag to follow.
