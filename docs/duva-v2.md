# duva v2: detector + actor

Design note, not yet implemented. Supersedes `duva-redesign.md` (that round —
pinned-only watching, rules as compose labels — is already shipped and is the
starting point here).

duva today answers "has the registry moved past the digest I pinned?" and
sends an ntfy notification. v2 keeps that and adds the other half: decide
whether the update may be applied unattended, and if so apply it — pull, pin,
recreate, commit. Anything that needs a human shows up in a small web UI where
it can be approved with one click.

This replaces both Watchtower (which never writes to the compose file, so it
destroys pinning) and `docker pin schedule` (which duplicates duva's detection
in `constrainedTarget`).

## The invariant

Every compose image stays digest-pinned, `<name>:<tag>@sha256:<digest>`, and
every change is a git commit. The tag is the tag to **follow**; the digest is
the record of what runs. An updater that leaves the file floating — or rewrites
the tag to describe the pin — breaks the thing pinning exists for. See "The tag
is the tag to follow" in the README.

That is the whole reason this tool exists rather than an off-the-shelf one.

## Flow

On a `DUVA_SCHEDULE` cron, per watched service:

1. **Detect** — is there something newer?
2. **Classify** — how big a change is it?
3. **Decide** — may duva apply this unattended?
4. **Act** or **Ask**.

### 1. Detect

Unchanged from today (`checkService` in `cmd/duva/main.go`), including the two
paths it already distinguishes:

- **Constrained** (`duva.include` regex): list registry tags, filter by
  include/exclude, sort by `registry.CompareVersions`, honour `duva.delay` as a
  release-age soak. The newest qualifying tag is the candidate.
- **Moving** (no `duva.include`, e.g. `latest`): fetch the remote manifest
  digest and compare against the recorded baseline. The candidate is a digest,
  not a tag.

Only **pinned** services are watched — pin status is the opt-in. Services
built locally (`build:`) are skipped everywhere, since a local image's digest
is not pullable on another host.

### 2. Classify

`Classify(oldTag, newTag) → Major | Minor | Patch | Unknown`.

Clean semver only. Anything that does not parse as a comparable `x.y.z` pair is
`Unknown` — no transform/rewrite formula for now. This costs approvals on
linuxserver-style tags (`1.14.7-ls166`) and is the first thing to revisit if it
becomes annoying.

Two known misreads, both accepted:

- **CalVer** (`2026.07.2`): a month bump reads as a minor.
- **Two-segment versions** (`17.10` → `17.11`): reads as a minor, though
  upstream calls it a patch.

Both err toward *asking* rather than *acting*, which is the safe direction.

**A moving-tag service has no old/new tag pair at all**, so classification does
not apply to it — see the policy table below. That is different from `Unknown`,
which means "there is a pair and duva cannot read it".

### 3. Decide

One label per service:

```yaml
labels:
  duva.auto: patch     # patch | minor | major | none
```

The highest bump duva may apply unattended. Default **`none`** — an acting
build deployed over a notify-only install must never silently start writing.

| Candidate | Applied unattended when |
|---|---|
| Patch | `duva.auto` is `patch`, `minor` or `major` |
| Minor | `duva.auto` is `minor` or `major` |
| Major | `duva.auto` is `major` |
| Unknown | treated as Major — needs `duva.auto: major` |
| Digest move (moving tag) | always, unless `duva.auto: none` |

Moving tags auto-applying is deliberate: choosing `latest` *is* the decision to
ride the edge. Adding an approval step on top would be pretending to a control
already declined.

### 4a. Act

```
pull → pin → compose up -d <service> → commit → pull --rebase && push
```

- **Pull first.** The cheapest failure is the one before anything is written,
  and pinning the digest of the image just pulled avoids any index-vs-manifest
  disagreement between `RemoteDigest` and `docker.GetDigest`.
- **Pin** via `internal/pin` as a library call, not by shelling out — same Go
  module, typed results, no output parsing.
- **Recreate** with `docker compose up -d <service>` rather than the Docker
  API, so the container stays compose-native. A container created outside
  compose gets recreated again by the next `compose up`.
- **Commit**, then `git pull --rebase && git push` — the same motion as the
  `gpp` alias. On a rebase conflict: abort, leave the commit local, notify.
  Never resolve a conflicted tree automatically.
- Commit subject follows the fleet convention, `<box>/<service>: <subject>`,
  with the box from `DUVA_HOSTNAME`. No `--no-verify`: the commit-msg hook
  lives in the mounted tree and rejecting a malformed subject is correct.

**Verification is the exit code, nothing more.** If `compose up -d` reports
success, duva is done. A container that starts and then crash-loops, or comes
up unhealthy, is caught by monitoring (gatus, autoheal) — not by duva. Watching
for that here would mean choosing an arbitrary wait and inventing a health
notion for services that define none.

**No rollback.** If the container starts and later misbehaves, the file and git
both say what is intended, and recovery is a human pinning the old digest back
(the digest is in the commit). Rehearsing upgrades on cloned stacks — what
`watchkeep` does — is a better answer to this than an after-the-fact revert.

**Exception — `compose up -d` returns non-zero.** Bad image, port clash,
invalid config: compose rejected the change, so the container never took the
new pin. Here duva *knows* the file describes something that has not run, so it
**reverts the file write** and notifies. Nothing is committed. This is not
rollback; it is refusing to leave a lie in the file.

Failure summary:

| Fails | Result |
|---|---|
| pull | nothing written, notify, retried next run |
| pin | nothing to undo (write is the last step of pinning), notify |
| `compose up` non-zero | file write reverted, no commit, notify |
| commit | pin stands as an uncommitted diff, notify — `git status` shows it |
| push | commit is local, notify; rides along with the next push |

### 4b. Ask

Anything not auto-applied becomes a **pending update**: an ntfy notification
(so you find out without looking) and a row in the UI (where you act).

## Web UI

Strictly an approval queue. No config pages, no watcher/registry browser, no
container inventory, no history. Auto-applied updates never appear — git log is
their record.

- One page, embedded in the binary. No frontend build, no JS framework.
- A row per pending update: service, current pin, available version, kind
  (major/minor/unknown), **Apply**.
- **Apply acts immediately** through the exact same path as an automatic
  update, and the page reports the result. One write path, not two.
- The row disappears once applied.

duva exposes an HTTP port; how it is fronted is the operator's business. On
this fleet that means Caddy + Authelia and no published port, but nothing
Authelia-specific belongs in the tool.

## State

Pending updates go in the existing `/data` state file — the one that already
holds the dedupe baseline. No second store, no database.

The file must survive restarts (the UI has to know what is pending) and must
not accumulate: an entry disappears when applied, when the candidate is
superseded, or when the service stops being watched.

## Container contract

| | |
|---|---|
| `/compose` | the compose project **directory**, now **rw** (was `:ro`). Directory, not a single file: `include:` resolves relative to it, and rename-on-write orphans a single-file bind |
| `/data` | writable — state |
| docker socket | required; honours `DOCKER_HOST`, so a socket proxy works |
| ssh key + `known_hosts` | mounted `:ro`. Must be owned by the container uid with `0600`, so run as that uid with `HOME` pointing at the mount (as hemma-agent does) |
| image | self-contained: docker CLI, compose plugin, git, openssh baked in |

**Blast radius.** This container rewrites compose files, restarts services, and
pushes to the infra repo — the same trust level hemma-agent already has here,
and worth stating plainly rather than discovering later. `git config --global
--add safe.directory` will be needed for the mounted tree (dubious-ownership
refusal).

## Module structure

Three modules, split by what they *read*; pin/unpin and duva share the same
**write** engine but not the same read engine:

- **core** — the compose file as data: locate/resolve includes, read
  (`RawImage`, `ParseImage`, `IsBuilt`, `Labels`), and the single write
  primitive (`PinImage`, a surgical line rewrite that preserves formatting).
  Plus `internal/registry` and `internal/docker`. The pin *transaction* (pin →
  `compose up`) belongs here too, since duva needs exactly it.
- **docker pin/unpin** — reads `image:` and `build:`. Never reads labels.
- **duva** — reads `image:` and labels.

`compose.Labels`/`labelsField` stays in `internal/compose` despite being
duva-only: moving it would duplicate the `composeFile` struct and the fiddly
two-form `labelsField.UnmarshalYAML` to enforce a boundary that a grep already
enforces. A note in `CLAUDE.md` is enough.

No `internal/core/` directory — it would churn every import to defend a
boundary nothing else in the repo has.

## Milestones

Each is a coherent increment that can be stopped at.

**M0 — `internal/pin`.** Lift `computeUpgrade`, `applyUpgrade` and the
transaction out of `cmd/docker-pin/main.go` and `schedule.go` into a package
both binaries can call. Collapse the doubled `maxDelayChecks`. No behaviour
change; the existing suite must pass untouched.
*Stop here:* nothing visible, but every later milestone depends on it.

**M1 — UI shell.** New `cmd/duva` alongside the old one. Reuse today's
detection verbatim; render what it finds as the approval queue. No Apply
button, no acting.
*Stop here:* you can see what duva sees, in a browser.

**M2 — classification.** `Classify` as a pure function, unit-tested against the
fleet's real tag schemes (`1.26.3`, `2026.07.2`, `17.10-alpine`, `1.14.7-ls166`,
`v1.2.3`). The UI shows the kind per row. Still no acting.
*Stop here:* verdicts can be checked against reality before anything acts on them.

**M3 — policy.** Parse and enforce `duva.auto`; ntfy for anything needing
approval. Still no acting.
*Stop here — the honest stopping point.* Wrong classifications arrive as phone
notifications, not broken containers. Run it for a few weeks; tune labels.

**M4 — act.** Pull → pin → recreate → commit → push, with the
revert-on-compose-failure path. Apply button wired to the same code. The
container contract changes here (rw mount, socket, ssh key).
*Stop here:* the thing works.

**M5 — retire.** Old duva off. Then decide about `docker pin schedule`, whose
`constrainedTarget` duplicates `checkService` — run `schedule remove` on every
host *before* shipping any removal, or orphaned timers keep firing. Note the
`PIN_*` hook env contract does not survive, so any `on_change` hook is a real
migration.

## Carried over from today's duva

Worth keeping as-is: `checkService`'s constrained/moving split, the state and
dedupe model, `loadServiceRules`, the fixed-mount container contract, the
"only pinned services" rule, and `hostLabel` for multi-host ntfy topics.

Worth rethinking: the notify-only assumption baked into `runOnce`'s control
flow, and the absence of any label validation (duva currently validates
nothing — a typo'd `duva.includ` is silently ignored).

## Testing

Follow the repo's seams: `dockerFuncs`, `regFuncs` and `sysFuncs` fakes for
anything touching docker, the registry or the system, so unit tests need no
daemon and no network. `hack/e2e-*.sh` covers what fakes structurally cannot —
`e2e-pin.sh` exists because faked digest lookups can never catch a tag rewrite.
The module targets Go 1.22, so no `t.Chdir` (see `17b45fd`).

## Open questions

- Does `registry.RemoteDigest` agree with `docker.GetDigest` on multi-arch
  index digests? If not, duva and `docker pin` could flap on the same image.
  Pulling before pinning (above) sidesteps it, but it should be confirmed.
- Is `/compose` a git working tree on every host, or a deployed copy? The
  entire git half assumes a checkout.
- `PinImage` writes the whole file with `os.WriteFile`; a write-temp-then-rename
  would remove the only partially-written-file failure mode. Worth doing
  regardless of duva.
