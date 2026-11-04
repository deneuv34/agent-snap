# Retention design

**Status:** Proposed. Phase 6, [#7](https://github.com/deneuv34/agent-snap/issues/7). Nothing in this release deletes a snapshot ref.

This record decides how Blink lets a person shorten a worktree's snapshot timeline. It answers the five Phase 6 work items in [`BLINK_DEVELOPMENT_PLAN.md`](BLINK_DEVELOPMENT_PLAN.md). The wire contract is FR-17 and FR-18 in [`BLINK_SPEC.md`](BLINK_SPEC.md), with the objects in sections 8.7 and 8.8.

A developer whose repository has collected thousands of checkpoints gets a read-only report that names every snapshot a policy would delete and every rule that keeps the rest. They then get one terminal command that deletes exactly those snapshots after they type a code from the prompt. An agent can read the report but cannot delete anything. The next maintainer gets one place that decides protection, one Git transaction that deletes, and no JSONL method that can remove a ref.

Deleting a hidden ref is the first operation that removes Blink data. The design follows from one measured fact about the timeline.

## A cleanup removes timeline entries, not disk space

Each snapshot commit names the previous snapshot as its parent (FR-2, FR-6). `latest` names the newest commit, so `latest` reaches every snapshot of the worktree through the parent chain. Deleting an item ref removes the snapshot from list, lookup, diff, and restore. It does not make the commit unreachable, so `git gc` keeps its objects.

The storage probe measured this on Git 2.54 and 2.56, SHA-1 and SHA-256, files and reftable. It recorded five snapshots, each with its own 256 KiB random file, then deleted the three oldest item refs and ran `git gc --prune=now`. The oldest snapshot's commit and its random file were still in the object store in all 8 configurations. Only after the probe deleted every ref of the namespace, `latest` included, did `git gc --prune=now` remove them. The pack shrank from 1283 KiB to 1 KiB. A plain `git gc` kept them, because Git keeps unreachable objects for `gc.pruneExpire`, two weeks by default.

Three decisions follow.

1. A policy has no size input. Every cleanup that keeps `latest` frees 0 bytes, whatever it deletes, so a size target could never be met. This holds while every item is on `latest`'s chain, which is true unless someone deleted or moved `latest` by hand.
2. A cleanup is recoverable. Every deleted snapshot's commit stays reachable from `latest`, so the cleanup result lists each deleted ID with its commit, and the person can recreate any of them. [Runbooks](#runbooks) gives the recipe.
3. Freeing disk space needs a different operation. It must drop a whole chain, or stop new snapshots from chaining. Both are outside Phase 6, and [Follow-ups](#follow-ups) records them.

What a cleanup does buy is a shorter timeline and cheaper ref scans. The scale probe timed `agent-snap list` and an unchanged-tree `create`, which looks up the reused snapshot among all item refs. With 10,001 loose item refs in the files backend, `list` took 1046 and 1428 ms, and the reused `create` 982 and 1053 ms, on Git 2.54 and 2.56. After `git pack-refs --all`, they took 192 to 321 ms. With 101 refs, each took 68 to 283 ms. Reftable at 10,001 refs took 194 to 539 ms. These numbers come from the first scale run, which ran alone.

## Blink never deletes on its own

No create, list, host event, timer, or other automatic step runs retention. A snapshot ref is deleted only by the CLI `cleanup` command, after a person confirms it on the terminal.

This beats an automatic age or count limit. An automatic limit would delete without a person seeing the list, and it frees no disk (above), so it would trade checkpoints for nothing measurable. It is also the gate default `g-retention-default`, which the operator can change.

## Retention is three operations with one safety class each

| Operation | Writes | Who can run it |
| --- | --- | --- |
| Retention report (FR-17) | Nothing. | The protocol, `SnapClient`, the CLI, and both host tools. |
| Pin and unpin (FR-18) | One pin ref under the worktree's namespace. | The CLI only. |
| Cleanup (FR-18) | Deletes item refs that the report listed. | The CLI only, after the person types the code on the controlling terminal. |

No JSONL method deletes or writes a ref for retention. `serve` therefore cannot delete a snapshot, so lifecycle hooks and agent tools, which reach the core only through `SnapClient` and `serve`, cannot delete one either. This beats a `snapshot.cleanup` method behind a one-shot entry like FR-16's `serve --restore`. That entry exists because Pi applies a restore from its terminal UI. Retention needs no host to delete, and with no method there is no transport, option, or request shape to guard.

The read-only report lands before any delete capability, in [Phase 6 lands in this order](#phase-6-lands-in-this-order).

## A policy names a count, an age, or both

A policy has two inputs. At least one is required.

- `keep` is an integer from 0 through 2147483647. The newest `keep` snapshots, by identifier, are kept. Protected snapshots count toward `keep`.
- `before` is a UTC timestamp, `YYYY-MM-DDTHH:MM:SSZ`. Only a snapshot whose identifier time is earlier than `before` can be deleted.

A snapshot is deleted when it is outside the newest `keep`, its time is earlier than `before`, and no protection rule applies. A missing input does not restrict. With both inputs, a snapshot must meet both, so adding an input can only keep more.

A policy with neither input is refused with `-32602`, and never read as "delete everything". This is the FR-15 rule that an invalid scope is never the whole tree.

Age is the identifier's timestamp. That is the time Blink minted the snapshot, it sorts the timeline (FR-7), and the report reads it from the ref name without opening an object. The committer date beats it on nothing, because Blink writes both from the same clock.

The Store reads no clock for retention. `before` is an absolute time, so a report is a function of the refs and the request. The CLI turns `--older-than <days>` into `before` once, from its own clock.

A size input is rejected, as [the first section](#a-cleanup-removes-timeline-entries-not-disk-space) measured.

A policy covers only the worktree that `cwd` resolves to. The Store builds every ref path from its own namespace, so no input can name another worktree's refs.

## Five rules protect a snapshot

A protected snapshot is never deleted, whatever the policy says. The report names the first rule that applies, in this order:

| Protection | Applies when | Why |
| --- | --- | --- |
| `invalid` | FR-12 lookup refuses the item ref. It is symbolic, does not point at a commit, has more than one parent, does not match its identifier, or lacks the product message prefix or identity. | Blink cannot trust any field it would judge. The corrupt-ref runbook covers these refs. |
| `latest` | `latest` names the item's commit. | Deleting it breaks the next unchanged create. The latest probe deleted that item and left `latest`, and the next `create` with an unchanged tree failed with `snapshot ref not found` in all 8 configurations. |
| `pinned` | A pin ref exists for the identifier (FR-18). | A person asked to keep it. |
| `backup` | The reason is exactly `restore backup`. | FR-15 records a restore's safety backup with that reason, and recovery from a restore needs it. |
| `manual` | The stored `source` is absent or is not `session-start`, `session-idle`, `agent-settled`, or `tool`. | A person typed it (`cli`, `command`), or Blink cannot tell how it was made. |

An item ref whose name is not a snapshot identifier is skipped, as FR-7 skips it. It is neither listed nor deleted.

Protection by `source` and `reason` reads caller-reported values, which FR-13 says are not evidence. The rule reads them only to keep more. A caller that sends a false automatic `source` makes its own snapshot deletable, and the person still sees it in the prompt before it goes. The guarantees that do not depend on reported values are `invalid`, `latest`, `pinned`, and the confirmation.

`tool` counts as automatic. An agent can call `agent_snap_create` in a loop, and if `tool` were protected, nothing could ever shorten those snapshots. This is a product call, and [Questions for review](#questions-for-review) lists it.

A snapshot without origin is `manual`. That keeps every snapshot that a 0.1.0 writer made, and every snapshot from a caller that sends no origin. A person can still shorten those by hand, as the corrupt-ref runbook describes. That is a limit the design accepts.

A restore's backup can be a reused snapshot (FR-15), which keeps its own reason and `source`. Once a later create moves `latest`, such a backup has no protection unless a person pins it. [Follow-ups](#follow-ups) proposes that a restore pin its backup.

## A person pins a snapshot without Git

A pin is a ref at `refs/agent-snapshots/<namespace>/pins/<id>` that names the snapshot's commit. `Store` builds the path from a checked identifier. The pin probe showed that `agent-snap list` does not list a pin ref, because list reads only `items/`.

- `agent-snap pin <id>` looks the snapshot up as FR-12 does, then writes the pin in one transaction with `create <pin> <commit>` and `verify <item> <commit>`. A pinned snapshot is reported as pinned again, with nothing written.
- `agent-snap unpin <id>` deletes the pin with its old value. A snapshot with no pin is reported unpinned, with nothing written. Unpin needs no confirmation, because it deletes no snapshot, and a later cleanup still needs one.
- A pin whose item ref is gone protects nothing. `unpin` still removes it.

Pins are CLI-only in Phase 6. A host tool that pins would be harmless, and one that unpins would let an agent lower a protection. Neither is needed for the exit criteria, so neither is built.

## The report lists every deletion and binds them with a digest

`snapshot.retention_report` reads the item refs and the pin refs of the namespace with one `for-each-ref`, with replace objects disabled as FR-12 lookup does. It then reads `latest` with a second command, and never before the items. [Concurrent creates](#concurrent-creates-other-worktrees-and-moved-repositories) shows why the order matters. The report returns the deletions, oldest first, 200 per page, and totals for every reason a snapshot is kept.

Two values bind the pages, as `current_tree` binds restore plan pages.

- `through` is the newest snapshot identifier when the first page was read. Every page and the cleanup consider only snapshots at or before it. A snapshot created after the report can never be deleted by it, and it does not move the `keep` window. Without `through`, every agent idle would shift the window and make the next page stale.
- `delete_digest` is the SHA-256 of one line per deletion, `<id> <commit>` and a line feed, oldest first. A later page that carries it recomputes the report and fails with `-32002` when the digest differs. A pin added, a ref deleted, or a ref changed between pages all change it.

The report never lists `latest`'s item, a pin, or a ref outside the namespace, because it builds its list only from `items/` of its own namespace and the protection rules run before selection.

A report entry carries the identifier, commit, creation time, `source`, `host`, and the reason cut to its first 256 bytes. Section 8.7 defines the fields and the cut. A page holds at most 200 entries, so it holds at most about 100 KiB of text.

## The cleanup deletes in one transaction with an old value per ref

The cleanup runs FR-18's sequence. Its write is one `git update-ref --no-deref --stdin` transaction. For each deletion, it sends `delete <item> <commit>` and `verify <pin> <zero-oid>`, then `prepare` and `commit`.

The ref probes showed what this gives, on Git 2.54 and 2.56, SHA-1 and SHA-256, files and reftable.

- One wrong old value fails the whole transaction, and no ref is deleted. Git printed `cannot lock ref '<ref>': is at <oid> but expected <oid>`.
- A ref that is already gone fails the transaction. Git printed `unable to resolve reference '<ref>'`.
- A pin that exists fails the transaction, and the item stays. Git printed `reference already exists`. A packed pin blocks the same way.
- A packed item ref is deleted, and the files backend's `packed-refs` no longer names it.
- A symbolic ref at an item path is deleted with `--no-deref` without touching its target. `refs/heads/main` was unchanged. The report never lists such a ref, because `invalid` protects it, so this is a second check.

The transaction holds every rule that matters at the instant of the delete. The old value proves that the ref still names the reported commit. The pin check closes the window between the recompute and the delete, so a pin added there blocks the cleanup. `latest` is not verified. An item that is not `latest` can never become `latest` again, because a create moves `latest` only to a commit it just minted (FR-6). Verifying `latest` would make every cleanup fail while an agent is active, and protect nothing.

This beats deleting refs one at a time. A loop would leave a half-done cleanup on any failure, and each step would need its own staleness check. It also beats `update-ref --batch-updates` on Git 2.51 and later, which deletes the refs that pass and reports the rest. Blink's floor is Git 2.30, and the person confirmed one set, not a subset.

A cleanup deletes at most 10,000 snapshots. A larger report refuses before the prompt, and the person picks a narrower policy. In the second scale run, one transaction that deleted 1000 item refs took 259 to 385 ms in the files backend and 33 to 91 ms in reftable. That run shared the machine with the race probe, so these are upper bounds. A 10,000-ref transaction was not measured. Ten times the files-backend number is about 4 seconds, which is a guess.

From just before `update-ref` starts until it exits, the CLI ignores `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT`, as FR-16 does for its write. Git inherits them as ignored. The RESTORE signal probe showed that a Go parent passes an ignored signal to its children.

After `update-ref` exits, the cleanup reads the namespace's refs again and decides the outcome from what it reads, not from Git's exit status. A reftable commit is one table write, so it is atomic. The files backend rewrites `packed-refs` and removes loose ref files one by one, so a `SIGKILL` during its commit can leave some listed refs deleted. That is inferred from Git's files backend and was not probed. The after-read reports exactly which listed refs are gone.

| Outcome | When | CLI exit |
| --- | --- | --- |
| `deleted` | Every listed ref is gone. | `0` |
| `refused` | Every listed ref is still present. | `4` |
| `partial` | Some listed refs are gone and some are present. | `3` |

## Only the CLI deletes, after a person types six hex digits on the terminal

The command is `agent-snap cleanup [--cwd <dir>] [--keep <n>] [--older-than <days>]`. It requires `--keep` or `--older-than`. It has no `--yes`, and it never reads the confirmation from standard input.

It reuses the FR-16 CLI rules from PR #33. It checks that standard input, standard output, and standard error are each the controlling terminal, then opens the terminal, and refuses with exit `4` when any check fails. An agent's shell tool gives the CLI pipes, so the agent cannot raise the prompt in front of the person. It never exits `2`, because Go exits `2` on a panic, and that status must not mean that nothing was deleted.

The code is the last six hex digits of `delete_digest`. It differs from FR-16, which uses the backup ID, because a cleanup makes no backup. The digest ties the typed code to the exact set of deletions, so a code typed for one report cannot confirm another. The code is not a secret. It makes the person read the prompt.

The prompt names the worktree, the policy, `through`, the totals for every protection, the number of deletions, and the oldest and newest deletion. It lists at most the first 20 deletions, with identifier, time, `source`, and reason under the quoting rule of host tool text. It says that the cleanup frees no disk space while `latest` stays, and that the result lists every deleted commit for recovery. It names the `retention-report` command that lists every deletion.

The cleanup refuses on Windows until a Windows CI job runs the terminal cases, as the restore does. The console checks there are inferred.

Hosts get the report tool and no cleanup. Both report tools end with the CLI command that runs the same policy. The OpenCode reason of FR-16 holds here too, because a host permission prompt can be set to allow without asking.

## Concurrent creates, other worktrees, and moved repositories

The race probe ran 40 creates against cleanup transactions that each deleted the oldest item that `latest` did not name, with its old value, until the creates ended. It ran on Git 2.54 and 2.56, files and reftable, with and without `git pack-refs --all --prune` in a loop, as `git gc` runs it.

The first version of the probe read `latest` before it listed the items. In one of its 8 runs, on Git 2.56 with the files backend, a create moved `latest` between those two reads. The probe then deleted the item that the new `latest` named, and left the namespace with no item. Version 2 lists the items first and reads `latest` after them, as FR-17 requires. In 24 runs, 3 for each configuration, it deleted 42 items each time and never the item that `latest` named. No create failed, no cleanup transaction failed, the chain held no merge, and `latest` reached all 43 commits each time.

A create and a cleanup touch disjoint refs. A create writes one new item and moves `latest`. A cleanup deletes old items and verifies pins. The one lock that both can wait on is the files backend's `packed-refs.lock`, which a deletion and `git pack-refs` take. Git's documentation says it waits up to `core.packedRefsTimeout` for it, one second by default. That is inferred, and the race probe saw no lock failure.

Refs under `refs/agent-snapshots/` live in the common ref store, so every linked worktree sees every namespace. The worktree probe created a snapshot in a linked worktree, saw its namespace from the main worktree, and saw the namespace stay after `git worktree remove`. The namespace is the first 16 hex digits of the SHA-256 of the absolute Git directory, which the probe checked in all 8 configurations. Moving the repository changes that directory, and the move probe showed that a list after `mv` no longer shows the snapshots from before it.

A cleanup never touches another namespace, including one whose worktree is gone. Blink cannot prove that a namespace is orphaned. A worktree on a removable disk, or a repository that a person will move back, looks the same. [Follow-ups](#follow-ups) records a separate design for orphaned namespaces.

## Git gc and an in-flight restore

A cleanup that keeps `latest` leaves every deleted commit reachable, so neither `git gc` nor `git maintenance` removes its objects.

A restore that names a deleted snapshot or backup fails FR-16 step 2 lookup with `-32001` and writes nothing. That is inferred from FR-16, because the restore is not built. A cleanup between a restore's step 2 and its write does not stop the write, because the restore holds tree IDs and the objects stay. The restore result then names a backup ID that no longer resolves, so recovery by restoring the backup needs the ID to be recreated first. The `backup` protection, and the follow-up that pins a reused backup, guard against that.

The cleanup does not take the FR-16 restore lock. A held lock would only delay the cleanup, the deletion it guards against is already refused by the protection rules, and taking it adds a file to the Git directory for an operation that otherwise writes only refs.

## Observability records operation metadata only

Blink keeps no metrics store and writes no log file. The core prints results and errors, the CLI prints the cleanup result as JSON, and hosts log through their own logger.

Phase 6 adds one structured event, `SnapOperationEvent`, that `SnapClient` passes to an optional `observe` callback once per settled request. A host routes it to its logger. It holds only these fields:

| Field | Values |
| --- | --- |
| `method` | The JSONL method. |
| `outcome` | `ok`, `error`, `timeout`, or `aborted`. |
| `code` | The JSON-RPC error code, when `outcome` is `error`. |
| `duration_ms` | Wall time from send to settle. |
| `reused` | For `snapshot.create`, the `reused` flag. |
| `counts` | For a report or a restore result, its totals. For a diff, its file total. |

It never holds `cwd`, a path, a reason, an error message, a snapshot ID, Git output, file content, a hunk, or an environment value. Error messages can name paths, so they stay in the host's local log under the existing [`DIAGNOSTICS.md`](DIAGNOSTICS.md) rules and never reach the event. A test asserts the exact key set of every event, so a new field needs a change to this table.

The cleanup result is the audit record of a deletion. It names every deleted ID and commit, which [`DIAGNOSTICS.md`](DIAGNOSTICS.md) already lists as safe to record.

## Runbooks

[`DIAGNOSTICS.md`](DIAGNOSTICS.md) already covers these failures.

- Stuck child processes. "Teardown failures", for POSIX and Windows, and "Scratch directories left after a hard kill".
- Corrupt refs. The snapshot failure rows for symbolic, foreign, and tampered item refs, and the ref lock row.

The landing steps add these rows and sections.

| Row or section | Says |
| --- | --- |
| `-32002` `snapshots changed since the report` | A pin, a deletion, or a changed ref moved the report. Request the first page again without `through` and `delete_digest`. |
| `cleanup needs a terminal` | Run `agent-snap cleanup` from a terminal. An agent's shell tool cannot confirm. Nothing was deleted. |
| `cleanup declined` | The typed code did not match. Nothing was deleted. |
| `snapshot <id> was pinned after the report` | Git's `reference already exists` on a pin. Run the cleanup again. Nothing was deleted. |
| `snapshot <id> changed after the report` | Git's `but expected` or `unable to resolve reference` on an item. Run the cleanup again. Nothing was deleted. |
| `cleanup deletes at most 10000 snapshots` | Raise `--keep` or move `--older-than` further back. |
| A `packed-refs.lock` held past one second | A `git gc` or `pack-refs` is running. Wait for it and run the cleanup again. Never remove the lock while a Git process runs. |
| `partial` outcome | The result lists the deleted and the kept refs. Run the report again to see what is left. |
| `snapshot ref not found` from `create` | `latest` names a commit whose item ref is gone, as the latest probe showed. A cleanup never does this. A hand-deleted item does. See the recovery section. |
| Recovery after a cleanup | Recreate one deleted snapshot with `git update-ref refs/agent-snapshots/<namespace>/items/<id> <commit> <zero-oid>`, from the IDs and commits in the cleanup result. The `<namespace>` is the prefix that `git for-each-ref refs/agent-snapshots/` shows for the worktree. |
| Large repositories | A long timeline slows `list` and reused creates (scale probe). Run `git pack-refs --all`, then `retention-report`, then `cleanup`. A cleanup frees no disk. Namespaces of removed or moved worktrees stay until a person removes them. |

## Phase 6 lands in this order

Each step is one pull request that ends in a check. Each step that adds a method, an error code, a CLI command, or a host tool updates FR-8 through FR-11, `README.md`, `docs/COMPATIBILITY.md`, and `docs/DIAGNOSTICS.md` in the same pull request. Store tests run on real repositories, SHA-1 and SHA-256, files and reftable.

1. This record, FR-17, FR-18, and sections 8.7 and 8.8. The check is review and a link check.
2. Store. `Store.RetentionReport`, read-only. `TestRetentionReportProtectsEachRule` is a table with a symbolic item, an item that names a tag, a foreign-author commit, `latest`'s item, a pinned item planted with `git update-ref`, a `restore backup` reason, and each `source` value with none. Each is kept with the right protection. `TestRetentionReportSelectsByKeepAndBefore` covers each input alone, both together, `keep` 0, and a snapshot whose time equals `before`, which is kept. `TestRetentionReportIgnoresSnapshotsAfterThrough` creates a snapshot between pages and checks the same entries and digest. `TestRetentionReportFailsWhenTheDigestChanged` plants a pin between pages and expects `ErrReportChanged`. `TestRetentionReportReadsLatestAfterTheItems` runs a create at a seam between the two reads, and checks that the item the new `latest` names is not listed. `TestRetentionReportReadsOnlyItsNamespace` fills a linked worktree's namespace and checks that the main worktree's report counts none of it. `TestRetentionReportCutsLongReasons` uses a 1 MiB reason and a multibyte character at byte 256. `TestRetentionReportWritesNothing` fingerprints every ref, the index, `HEAD`, the configuration, and the object store before and after. The check is `go test ./...`.
3. Protocol. `snapshot.retention_report` with its validation table, the shared `-32002` for a changed digest, and `-32601` from an older binary. `TestServeRetentionReportRejectsAPolicyWithNoInput` expects `missing required param: keep or before`. The check is `go test ./...`.
4. TypeScript. The section 8.7 types and every cross-field rule, `SnapClient.retentionReport`, `SnapLike.retentionReport`, and `SnapRetentionReportUnsupportedError` for `-32601`. Tests cover a digest that is not 64 hex digits, entries out of order, a `next_offset` that breaks the paging rule, and an entry whose identifier fails the FR-12 syntax.
5. CLI `retention-report`, with `--keep`, `--older-than`, `--offset`, `--through`, and `--delete-digest`. `retention-report converts older-than once` checks that a two-page report uses the first page's `before`.
6. Hosts. `agent_snap_retention_report` in both hosts. Its text lists every entry with the quoting rule, the totals, the next page's values, and the CLI cleanup command. `retention report tool text ends with the cleanup command` checks both hosts. The OpenCode and Pi tests check that neither host registers a cleanup or pin tool.
7. Store. `Store.Pin` and `Store.Unpin`. `TestPinProtectsFromTheReport`, `TestPinIsIdempotent`, `TestUnpinWithoutAPinIsANoOp`, `TestPinRefusesAnInvalidItem`, and `TestPinWritesOnlyItsRef`, which fingerprints every other ref. `TestPinsAreNotListed` checks list and get.
8. CLI `pin` and `unpin`.
9. Store. `Store.Cleanup`, with its tests below.
10. CLI `cleanup`, with its tests below. It needs `internal/ptytest` from Phase 5 step 9 of [`RESTORE.md`](RESTORE.md). If Phase 5 has not landed it, this step adds it.
11. Observability. `SnapOperationEvent` and the `observe` option in `packages/core`, and both hosts' wiring. `observe events hold only the allowed keys` runs every method, including failures, timeouts, and aborts, and checks each event's exact key set. `observe never sees cwd or reasons` sends a `cwd` and a reason that hold a marker string, and checks that no event's JSON holds it.
12. Runbooks. The [Runbooks](#runbooks) rows in `docs/DIAGNOSTICS.md` that the earlier steps did not add.

The Store cleanup step (9) adds these tests. Tests reach points in the sequence through `afterCleanupStep`, an unexported package variable like the restore's seam.

- `TestCleanupDeletesOnlyTheReportedRefs` fingerprints every ref in the repository and checks that only the listed item refs changed.
- `TestCleanupNeverDeletesLatestOrAnotherNamespace` gives the policy `keep` 0 in a repository with a linked worktree, and checks that `latest`, its item, every pin, and the other namespace are byte-identical.
- `TestCleanupRefusesWhenTheDigestChanged` deletes one listed ref by hand before the recompute and expects `refused` with nothing deleted.
- `TestCleanupRefusesAPinAddedAfterTheRecompute` plants a pin at the seam between the recompute and the transaction, and expects `refused` with every listed ref present.
- `TestCleanupRefusesAStaleOldValue` repoints one listed ref at the same seam and expects `refused` with nothing deleted.
- `TestCleanupUnderPackedRefs` packs every ref first. `TestCleanupOnReftable` runs the suite on a reftable repository.
- `TestCleanupRacesCreates` and `TestCleanupRacesPackRefs` repeat the race probe and check that no create fails, the chain has no merge, every created snapshot is listed or was in a cleanup result, and `latest`'s item exists.
- `TestCleanupKeepsObjects` runs `git gc --prune=now` after a cleanup and checks that every deleted commit is still present.
- `TestCleanupRefusesAHeldRefLock` holds an item's `.lock` file and expects `refused` with Git's message and nothing deleted.
- `TestCleanupDecidesTheOutcomeFromTheRefs` replaces the transaction runner, an unexported package variable, with one that deletes half the listed refs and exits non-zero. It expects `partial` with exact lists.
- `TestCleanupRefusesMoreThanTenThousand` plants 10,001 deletable items with `git fast-import`. `TestCleanupDeletesTenThousand` plants 10,000 and checks that they are all deleted.

The CLI cleanup step (10) adds these tests, driven through `internal/ptytest`.

- `cleanup refuses without a terminal`, and once for each of the three streams as a pipe while `/dev/tty` opens. Each exits `4` and deletes nothing.
- `cleanup declines a wrong code`, `cleanup declines an empty line`, and `cleanup declines the end of input`. Each exits `4` and deletes nothing.
- `cleanup deletes after the code` checks the result JSON, exit `0`, and the refs.
- `cleanup refuses when the report changed during the prompt` creates a pin while the prompt waits. It exits `4` and deletes nothing.
- `cleanup with nothing to delete exits 0 without a prompt`.
- `cleanup prompt quotes reasons` uses a reason with a line feed and an escape byte.
- `cleanup ignores stop signals during the transaction` re-executes the test binary as the CLI. The child's `afterCleanupStep` seam, at the point after the signals are ignored and before `update-ref` starts, sends `SIGINT` and `SIGTERM` to its own process group. The cleanup ends in `deleted` and exits `0`.
- `cleanup refuses on Windows` runs on the Windows CI job.

## Proposed safety invariants for AGENTS.md

AGENTS.md is held by PR #33, so these invariants are proposed here. The coordinator adds them when that PR lands.

- Blink never deletes a snapshot ref on its own. No create, list, host event, or timer runs retention.
- No JSONL method deletes or writes a ref for retention. Only the CLI `cleanup` command deletes snapshot refs, and only after a person types the code on the controlling terminal.
- A cleanup deletes only item refs of its own worktree's namespace that its report listed. It never deletes `latest`, the item `latest` names, a pin, or any ref outside that namespace.
- A cleanup deletes in one `git update-ref --no-deref --stdin` transaction that checks each ref's old value and verifies that no pin exists for it.
- A cleanup decides its outcome by reading the refs after Git exits, and never tells the person that nothing was deleted when a ref may be gone.

## How the design meets the Phase 6 exit criteria

| Exit criterion | Met by |
| --- | --- |
| Retention cannot delete a protected checkpoint or a ref outside Blink's namespace. | The five protection rules, the namespace-built ref paths, the old value and pin check in the transaction, and the Store step 9 tests. |
| Operators can diagnose failures without accessing developer source code. | The runbook rows, the cleanup result, and `SnapOperationEvent`, which holds no path, reason, or content. |
| Storage behavior and cleanup safety are documented and tested. | [A cleanup removes timeline entries, not disk space](#a-cleanup-removes-timeline-entries-not-disk-space), its probe, and `TestCleanupKeepsObjects`. |

## Decisions

| Decision | Beats | Because |
| --- | --- | --- |
| Only a confirmed cleanup deletes. | An automatic age or count limit. | A person sees every deletion first, and an automatic limit would free no disk. |
| No JSONL method deletes. | A `snapshot.cleanup` method behind a one-shot entry. | No host needs to delete, and a method that does not exist needs no guard. |
| A policy is `keep` and `before`. | A size input. | A cleanup that keeps `latest` frees 0 bytes (storage probe). |
| Age is the identifier time. | The committer date. | It sorts the timeline, needs no object read, and comes from the same clock. |
| The report binds with `through` and `delete_digest`. | Recomputing the policy at cleanup time with no anchor. | Without `through`, every create shifts the `keep` window, and every report goes stale while an agent works. |
| The report reads `latest` after the items. | One read of the whole namespace, or `latest` first. | A `latest` read first let a concurrent create make the newest item look unprotected (race probe version 1). |
| `latest`'s item is always protected. | Letting a cleanup delete it and move `latest`. | Deleting it breaks the next unchanged create (latest probe). Moving `latest` would also change the next snapshot's parent. |
| Pins are refs under the namespace. | A pin file in the Git directory, or a pin flag in the commit message. | A ref is covered by the same transaction, and a commit cannot change without changing its ID. |
| One transaction with an old value and a pin check per ref. | A loop of single deletes, or `--batch-updates`. | All or nothing, with every rule checked at the instant of the delete (ref probes), on Git 2.30 and later. |
| The outcome comes from reading the refs after Git exits. | Git's exit status. | The files backend can be interrupted part way through its commit. |
| The code is the last six hex digits of `delete_digest`. | The count of deletions, or a `y`. | It ties the confirmation to one exact set. |
| A cleanup never touches another namespace. | Detecting and removing orphaned namespaces. | Blink cannot prove that a worktree is gone for good. |
| `tool` snapshots can be deleted. | Protecting them as manual. | An agent can make them without limit, and only a person-typed snapshot is manual. |

## Questions for review

- Is `tool` automatic? Protecting it keeps agent checkpoints such as "before refactor", and leaves no way to shorten them.
- Is a cleanup worth building when it frees no disk? The scale probe puts its value at about 1 second per list and per reused create at 10,000 loose refs, and about 0.3 seconds once refs are packed.
- Should Phase 6 also unchain new snapshots, or add an operation that drops a whole timeline, so that retention can free disk? Both are in [Follow-ups](#follow-ups).
- Is the 10,000 cap right?
- Should the cleanup take the FR-16 restore lock?
- Is a recovery recipe that names `git update-ref` acceptable in DIAGNOSTICS, or should Blink restore refs from a cleanup result itself?

## Follow-ups

- A restore pins its backup. This is a Phase 5 change to `confirmRestore` and the CLI `restore`, which PR #33 owns, and it needs the pin step first.
- Unchain new snapshots. A create would name `HEAD` as the commit parent and record the previous snapshot in the message, so that deleting a snapshot frees its own objects. It changes FR-2, FR-6, and the meaning of `parent` in section 8.2, so it needs its own design and compatibility review.
- Drop a whole timeline. One confirmed operation would delete `latest` and every item of the namespace, so that `git gc --prune=now` frees the chain (storage probe). It must define what happens to protected snapshots, so it needs its own review.
- Orphaned namespaces. A report could list the namespaces that no live worktree derives, with counts only. Removing one deletes another namespace's refs, so it needs its own design.
- A create whose `latest` names a commit with no item ref fails until the tree changes (latest probe). The create could treat that state as no latest. This is a Store fix outside Phase 6 scope.

## Evidence

The probes ran on macOS on Git 2.54.0 (Apple Git-157) and Homebrew Git 2.56.0, with an `agent-snap` built from main at `ff57d46`. Each ran in fresh `mktemp -d` repositories with a private `HOME`, `GIT_CONFIG_NOSYSTEM=1`, and no inherited `GIT_*` variables. The scripts and logs are kept with the Phase 6 program's evidence, outside this repository.

- The ref probe ran SHA-1 and SHA-256, files and reftable, on each Git, 8 configurations with 33 checks each. All 260 checks passed. It covers the storage probe, the latest probe, the transaction cases, the packed and symbolic refs, the worktree probe, and the move probe above.
- The pin probe ran the same 8 configurations with 8 checks each. All 64 passed.
- The race probe ran 8 configurations, Git 2.54 and 2.56, files and reftable, each with and without a `pack-refs` loop. Its first run deleted at most one item per create, so only 7 or 8 deletions overlapped the creates. Version 1 then ran its cleanups until the creates ended, and version 2 read `latest` after the items. Both are described in [Concurrent creates](#concurrent-creates-other-worktrees-and-moved-repositories).
- The scale probe ran files and reftable on each Git, at 101, 1001, and 10,001 item refs, with fast-import commits that use the product identity.

Not observed. Windows, Git older than 2.54, a `SIGKILL` during a files-backend commit, a restore that names a deleted snapshot, and `git maintenance` run by its scheduler.

## Assumptions

Each assumption the design relies on is listed with its label. An observed fact has a probe. An inferred fact follows from code or documentation that this record read but did not run. A requirement is a rule that a later pull request builds and the named test proves.

| Assumption | Label | Proven by |
| --- | --- | --- |
| Deleting item refs while `latest` stays frees no object, even after `git gc --prune=now`. | Observed (storage probe, 8 configurations) | `TestCleanupKeepsObjects`. |
| Deleting every ref of a namespace lets `git gc --prune=now` remove the chain, and a plain `git gc` keeps it. | Observed (storage probe) | None in Phase 6. The drop follow-up needs it. |
| Hidden refs have no reflog by default, so the cleanup result is the only record of a deleted ref. | Observed (`git reflog exists` in the ref probe). Inferred for `core.logAllRefUpdates=always`, which would log them. | None. A deleted ref's reflog goes with it. |
| One wrong old value fails a `start`/`prepare`/`commit` transaction and deletes nothing. | Observed (ref probe, 8 configurations) | `TestCleanupRefusesAStaleOldValue`. |
| `verify <ref> <zero-oid>` fails a transaction when the ref exists, packed or loose. | Observed (pin probe) | `TestCleanupRefusesAPinAddedAfterTheRecompute`. |
| `delete` of a packed ref removes it from `packed-refs`. | Observed (ref probe, files) | `TestCleanupUnderPackedRefs`. |
| `--no-deref` deletes a symbolic ref and leaves its target. | Observed (ref probe) | `TestCleanupDeletesOnlyTheReportedRefs` with a planted symbolic item. |
| Deleting `latest`'s item makes an unchanged create fail with `snapshot ref not found`. | Observed (latest probe) | `TestRetentionReportProtectsEachRule`, `latest` row. |
| Concurrent creates and cleanups touch disjoint refs and do not fail each other. | Observed (race probe). The overlap was 40 creates per run. | `TestCleanupRacesCreates` and `TestCleanupRacesPackRefs`. |
| An item that is not `latest` never becomes `latest` again. | Inferred from FR-6 and `recordTree`, which moves `latest` only to a commit it just wrote. | `TestCleanupRacesCreates`. |
| A `SIGKILL` during a files-backend commit can leave some listed refs deleted. | Inferred from Git's files backend. Not probed. | `TestCleanupDecidesTheOutcomeFromTheRefs`. |
| A reftable commit is atomic. | Inferred from the reftable format, which writes one table. | `TestCleanupOnReftable`. |
| `update-ref` started from a CLI that ignores the stop signals inherits them as ignored. | Inferred from the RESTORE signal probe, which observed it for `sh`. | `cleanup ignores stop signals during the transaction`. |
| Refs under `refs/agent-snapshots/` are shared by every worktree, and a removed worktree's namespace stays. | Observed (worktree probe) | `TestCleanupNeverDeletesLatestOrAnotherNamespace`. |
| Moving a repository changes its namespace. | Observed (move probe) | None. A runbook row documents it. |
| A list at 10,001 loose refs takes about 1 second, and about 0.3 seconds packed. | Observed (scale probe) | None. It sets the value of the work, not a rule. |
| A 1000-ref transaction finishes in under 0.4 seconds, and a 10,000-ref one in a few seconds. | Observed for 1000 refs under load (second scale run). A guess for 10,000. | `TestCleanupDeletesTenThousand`. |
| A report that reads `latest` after the items never lists the item that the newest `latest` names. | Observed (race probe version 2). Inferred from FR-6. | `TestRetentionReportReadsLatestAfterTheItems`. |
| A restore that names a deleted snapshot refuses with `-32001` before any write. | Inferred from FR-16 step 2 | The Phase 5 restore suite. |
| An agent's shell tool starts the CLI with pipes on all three streams. | Inferred, as in [`RESTORE.md`](RESTORE.md) | `cleanup refuses without a terminal`. |
| Origin values are caller-reported. A false automatic `source` makes a snapshot deletable after the person confirms it. | Accepted limit (FR-13) | None. |
| Snapshots without origin are kept, including every 0.1.0 snapshot. | Accepted limit | `TestRetentionReportProtectsEachRule`, no-`source` row. |
| A reused backup has no protection once `latest` moves, unless pinned. | Accepted limit until the backup-pin follow-up | None. |
| Windows terminal checks are not proven, so the cleanup refuses on Windows. | Accepted limit | `cleanup refuses on Windows`. |
