# duva v5: applying in a repository that has uncommitted edits

Status: **done** (2026-10-22). Points 1-3 as proposed; for 4, readiness no
longer asks about uncommitted changes at all, and the apply refuses with the
file named.

## The problem

duva refuses every apply on a host while its repository has *any*
uncommitted change. `updater/transaction.go`:

```go
IsClean: func(dir string) (bool, error) {
	cmd := gitCmd(dir, "status", "--porcelain")
	...
	return strings.TrimSpace(string(out)) == "", nil
},
```

and the transaction checks it before anything else ("A clean repository is a
precondition, not a setting"). `updater/main.go`'s readiness asks the same
thing, so the page greys Update with "commit or stash them and this clears on
its own".

That is stricter than the reason for it. The reason is *never commit on top of
someone's half-finished edit*. But an edit to one compose file blocks applies
to services in every other file too.

It came up on optiplex: while iterating on the page, `optiplex/duva/compose.yml`
was left pointing at `registry.guldmund.dk/duva:latest` and uncommitted. That
one edit stopped duva applying anything on the host -- services in
`optiplex/docker-compose.yml` and `optiplex/media/media.yml` included, which the
edit has nothing to do with.

## What hemma already does

hemma solved the same thing for deploys. `syncWithOrigin` in
`~/docker/homebrew-hemma/internal/cli/kernel/deploy.go` (commit `8213757`,
"bring every remote host level with origin too, before the fleet pulls"):

- On a remote host (`allowDirty`), uncommitted files are allowed: someone may
  be mid-edit there.
- Before `git pull --rebase`, it compares the dirty paths
  (`git status --porcelain`) with what origin's new commits change
  (`git diff --name-only HEAD...@{u}`).
- If none overlap, it pulls with `--autostash`. Re-applying the edits then
  cannot conflict.
- If one does, it touches nothing and says which files:
  "has uncommitted edits to X, which origin's new commits also change --
  nothing was touched."
- A rebase that stops on a conflict anyway is aborted, leaving the repo as it
  was.

## Proposal for duva

1. **Refuse only when the file duva would rewrite is dirty.** The transaction
   knows its file, the compose file declaring the service (`file` in
   `transaction.go`, `dirOf(file)` for the repo). Ask
   `git status --porcelain -- <file>` instead of the whole tree.
   - An apply to a service in `media/media.yml` goes ahead while
     `duva/compose.yml` has an edit.
   - An apply to duva itself, whose pin is in that edited file, is still
     refused, with the file named in the reason.
   - The repo-busy (index.lock) handling stays as it is.

2. **Commit only that file.** Today it is `git add <file>` then
   `git commit -m <message>`, which also commits anything a person had
   staged. Make it `git commit -m <message> -- <file>`, which commits that
   path whatever else is in the index.

3. **With `git.push` on: pull with `--autostash`, behind hemma's overlap
   check.** `PullRebase` is `git pull --rebase` today, and it fails outright
   on a dirty tree. Do as hemma does: dirty paths ∩ incoming paths; empty
   pulls with `--autostash`, non-empty refuses with the files named. Keep the
   existing `RebaseAbort` on failure. (Off on optiplex today, so this path
   matters less there, but it is the same rule.)

4. **Readiness on the page.** Whether a host can apply is no longer one yes or
   no for the whole repo: it depends on the row. Either:
   - make `CanApply` per entry (dirty file ∩ that entry's file), or
   - keep the host-level answer, and have the apply refuse with a precise
     reason when the row's own file is dirty.

   The second is smaller. The first is what a person would expect from a
   greyed button.

## What still holds

- **"`IsClean` then blocks the next apply" (CLAUDE.md, duva v4/v5)** after a
  failed recreate. A failed apply leaves *its own* compose file rewritten and
  uncommitted, so the same service stays blocked until a person looks. That
  is the case the rule exists for, and it still holds. Other services carry
  on.
- **No rollback.** Unchanged.
- **The updater's own container.** Self-update rewrites duva's own compose
  file. If that file is dirty, it is refused like any other.

## Tests to add (`updater/transaction_test.go`, plus the integration suite)

- A dirty file other than the target does not block the apply. The commit
  contains only the target file.
- The target file dirty: refused, reason names the file, nothing pulled or
  written.
- Something else *staged* (not just modified) is not swept into duva's
  commit.
- Push on, origin ahead, dirty unrelated file: pulled with autostash,
  committed, pushed, the edit still there afterwards.
- Push on, origin ahead, dirty file that origin also changes: refused, nothing
  touched, files named.
- A failed recreate still blocks the next apply *of that service*.
- Integration (`test/integration`, v5 fixtures): an uncommitted edit to an
  unrelated file in the fixture project, and the within-policy apply still
  goes through. Mutation-check it: without the change, the suite must fail.

## Docs to update when it lands

- `CLAUDE.md`, the duva v4/v5 updater bullets: the precondition is "the file
  being rewritten is clean", not "the repository is clean".
- `duva-v5/README.md`, "A dirty repository, or a rate-limited registry".

## Current state on optiplex (2026-10-21)

- `optiplex/duva/compose.yml` points at `registry.guldmund.dk/duva:latest`,
  uncommitted, while the page is iterated on. Pin it by digest and commit it
  once it settles; until then duva applies nothing on optiplex.
- The checkout is ahead of origin by three unpushed commits: duva's own
  `9548fb4 optiplex/qui`, and two pin commits (`2267668`, `a836d88`) that
  should not have been made there.
