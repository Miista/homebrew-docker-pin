# duva redesign: pinned-only, config lives in compose labels

Design note, not yet implemented. No existing duva users — no migration needed.

## Core change

duva only watches **pinned** services (`image:` has `@sha256:...`). Pin
status *is* the opt-in/opt-out:

- `docker pin <service>` → duva starts watching it.
- `docker unpin <service>` → duva stops.

Unpinned services get a log line ("not pinned, skipping") and nothing else:
no notification, no state tracking.

Why: this is what actually differentiates duva from diun. Watching every
service for "is there anything newer" (today's behavior, and diun's) is
generic drift-watching. Watching only pinned services answers a sharper
question — "has the registry moved past the exact digest I deliberately
locked to" — which only makes sense for services where a real versioning
decision was made.

## Per-service rules move into compose labels

`tags` / `exclude` / `delay` move from `config.yaml`'s `services:` list to
labels on the service in the compose file:

```yaml
services:
  paperless-db:
    image: postgres:17.10-alpine@sha256:...
    labels:
      duva.tags: '^17\.\d+-alpine$'
      duva.exclude: '(alpha|beta|rc)'
      duva.delay: 7d
```

Same reasoning as the pin gate: config that governs a service should live
with that service, so there's one file to check and nothing to drift when
services are added/renamed/removed.

`config.yaml` is dropped entirely. `schedule` / `notify` / `hostname` move
to env vars instead (`on_change` is left out — it's a mutate-and-hook
concept that never applied to duva):

- `DUVA_SCHEDULE` — cron expression
- `DUVA_HOSTNAME` — optional, falls back to OS hostname, same as today
- `DUVA_NTFY_URL`, `DUVA_NTFY_TOPIC`, `DUVA_NTFY_TOKEN` — all-or-nothing.
  Plain env vars in duva's code, no `token_env`/`token_file` indirection
  (that existed only to keep secrets out of config.yaml, which no
  longer exists).

State file stays a fixed path (`/data/duva.json`, unchanged) — duva
currently has *no* env vars at all (a prior `DUVA_CONFIG` var was removed
when mounts became fixed positional paths); the ones above are new.

Container contract becomes `/compose` (ro) + `/data` (rw) + env — no
`/config.yaml` mount.

Recommended compose pattern: `DUVA_NTFY_TOKEN` is the only one of these
that's a secret, so it's the only one that needs hiding — give it its own
`env_file:` (gitignored), while everything else stays as plain inline
`environment:` entries that are fine to commit:

```yaml
services:
  duva:
    image: ghcr.io/miista/duva:latest
    environment:
      DUVA_SCHEDULE: "0 6 * * *"
      DUVA_NTFY_URL: https://ntfy.example.net
      DUVA_NTFY_TOPIC: docker-pin
    env_file: ./duva-secrets.env   # DUVA_NTFY_TOKEN=... ; gitignored
```

No new code needed for this — Compose already loads `env_file:` into the
container's environment, so duva just does a plain `os.Getenv`.

## Why not the Docker socket

Considered reading image/digest/labels live via the socket instead of
parsing compose. Rejected: the socket can show what's *running*, but not
whether a digest was *deliberately pinned* vs. just whatever a floating tag
resolved to — that distinction only exists as text in the compose file.
Since duva's job is about declared intent, not live state, compose is the
correct source, not just the incidental one. (Assumes normal workflow:
edit compose, then `up`.)

## Implementation note

Real overlap with `docker-pin` remains in compose file handling — locating
the file, walking `include:`, resolving a service, reading the image line
(`Locate`/`ListServices`/`ResolveServiceIn`/`ParseImage`/`RawImage`). duva
keeps using all of these; they stay shared in `internal/compose`. Reading
a service's `labels:` is the one new capability, and it's small enough to
add alongside `RawImage`/`ParseImage` in the same package rather than
forking a duva-local parser.

What does diverge: duva drops `schedule.Config`/`schedule.Service`/
`schedule.Load` entirely (no config file, no `Services` list) in favor of
env vars plus compose labels. `schedule.ParseDelay` stays shared — it's a
pure "48h"/"7d"/"2w" parser both `docker pin schedule`'s `delay:` and
duva's `duva.delay` label need, with no config-file coupling.

Confirmed no impact on `docker pin schedule`: `cmd/docker-pin/schedule.go`
never references duva, so removing duva's use of `schedule.Config`/
`Service`/`Load` doesn't change `docker pin schedule`'s behavior at all —
it keeps its own `pin.yaml`-based config exactly as today.

## Open / not yet decided

None — env var names and the `docker pin schedule` impact are settled
above.
