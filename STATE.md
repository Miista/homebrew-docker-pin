# duva v2 — where things stand

Working notes for the `feature/duva-v2` branch. Delete this when the branch
lands; the durable design lives in `docs/duva-v2.md`.

## Status

26 commits on `feature/duva-v2`, **not pushed**. All 12 packages pass, four
integration suites (58 checks) pass, merged coverage ~71%.

| Milestone | |
|---|---|
| M0 | `internal/pin` extracted as the shared write engine |
| M1 | detection returns findings; approval queue UI |
| M2 | semver classification (patch/minor/major/unknown) |
| M3 | `duva.auto` policy: what applies unattended vs waits |
| M4 | applying: pull → pin → compose up → commit → push, plus the Apply button |
| M5 | **not started** — retire `docker pin schedule` |

Also done along the way: coverage tooling (`make cover`, unit+integration
merged), the test-approach rework, integration suites rebuilt to run offline,
and the dead tag resolver deleted (444 lines).

## Decisions that shaped it

- **The tag is a follow-instruction, never rewritten.** The digest is the
  record of what runs. Resolving `latest` to a concrete version froze services
  silently, because a concrete tag never moves.
- **duva owns the whole transaction.** `internal/pin` stays the shared write
  engine; `docker pin` is untouched. Pinning an image line is shared;
  committing it to a repository is duva's business alone.
- **No rollback**, with one exception: if `compose up` refuses the new image,
  the file is put back. duva knows the container never took it, so leaving the
  claim there would be a lie. Rehearsing an upgrade on a cloned stack is a
  better answer than an after-the-fact revert, and belongs elsewhere.
- **Everything after the container is record-keeping** and cannot make the
  update untrue: a failed commit leaves an uncommitted change `git status`
  shows; a failed push is a note.
- `DUVA_APPLY` and `DUVA_GIT_PUSH` default off. duva refuses to act on a
  repository with uncommitted changes.
- **The moving-tag baseline advances only on apply**, never on detection —
  otherwise a move that could not be applied is forgotten and the service
  silently stops being offered it.
- duva always serves the queue on `:8080`. Whether anyone can reach it is
  decided by `ports:` in a compose file.

## Testing rules

- **Unit**: what we own runs for real (compose parser, pin, watch, policy,
  state, filesystem); what crosses the application boundary is mocked
  (registry HTTP, docker, git). Mocking our own code would only assert a
  belief about it, and beliefs do not get updated when the code does.
- **Input is generated**, not written out (`internal/fixture`): a test pins
  down only what it asserts about and adjusts the generated value where it
  needs something specific. Seeds are logged; reproduce with
  `fixture.NewSeeded`.
- **Two complementary halves**: happy paths assert what duva does when
  everything is right; the break-one-thing tests take a working fixture, break
  exactly one thing, and assert the consequence — so a failure names its cause.
- **Integration**: real registry protocol, real daemon, real compose CLI, real
  git — all local. **Nothing upstream, ever.** Own TLS registry, `FROM scratch`
  images, static Go binaries where something must actually run.
- **Every scenario starts from nothing and leaves nothing** (`hack/lib/context.sh`).
  Cleanup is never written by a test: `scenario` clears the previous one, an
  EXIT trap clears the last, and labelled leftovers from a killed run are swept
  on the next start.

## Commands

    make test              # unit + integration
    make test-unit         # fast, no docker
    make test-integration  # four suites, sequential, offline
    make cover             # unit / integration / merged breakdown

## Next

1. **M5** — retire `docker pin schedule`. ~1500 lines and 27 functions in
   `cmd/docker-pin/schedule.go`, `internal/schedule`, `internal/croncal`.
   Before shipping any removal, run `schedule remove` on every host or
   orphaned systemd timers keep firing. The `PIN_*` hook env contract does not
   survive, so any `on_change` hook is a real migration.
2. **Soak semantics** (task #10, deferred). Today `duva.delay` runs *before*
   classification, so a too-fresh tag is not a candidate at all and one delay
   applies to every bump size. Options: per-bump delays, or soaking only what
   duva would auto-apply. Note a fresh major can be masked by an older
   qualifying patch, since only the newest qualifying tag is returned.
3. **Optional coverage.** `serve` is untested — the suites only drive
   `duva run`, so the cron loop, HTTP server and store accessors are not
   exercised even though `serve` is what ships. `runUpgrade` (53%) and
   `maybeHelp` (29%) in docker-pin are pure argument parsing and survive M5.
   `schedule.go`'s coverage is not worth chasing until M5 decides its fate.

## Open questions

- Do `registry.RemoteDigest` and `docker.GetDigest` agree on multi-arch index
  digests? If not, duva and `docker pin` could flap on the same image. Pulling
  before pinning sidesteps it, but it has not been confirmed.
- Is `/compose` a git working tree on every host, or a deployed copy? The
  entire git half assumes a checkout.
