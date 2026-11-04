# Retention design

**Status:** Proposed, revision 2. Phase 6, [#7](https://github.com/deneuv34/agent-snap/issues/7). Nothing in this release deletes a snapshot ref.

This record decides how Blink lets a person see and remove snapshot data. It answers the five Phase 6 work items in [`BLINK_DEVELOPMENT_PLAN.md`](BLINK_DEVELOPMENT_PLAN.md). The wire contract is FR-17 and FR-18 in [`BLINK_SPEC.md`](BLINK_SPEC.md), with the objects in sections 8.7 and 8.8.

A developer gets a read-only report of every snapshot namespace in the repository. The report names the worktree that each namespace belongs to, or says that none does, as for a removed worktree or a moved repository. The developer then gets one terminal command that drops one whole namespace after they type a random code from the prompt. A drop followed by `git gc --prune=now` frees the namespace's objects. The next maintainer gets one read-only method, one CLI command that deletes, and one Git transaction.

Deleting a hidden ref is the first operation that removes Blink data.

## Phase 6 scope is a default the operator can change

The first revision of this record designed a selective cleanup by age and count. Its adversarial review failed it, and the coordinator then applied a default under gate `g-retention-scope`. This revision designs that default.

Phase 6 delivers these by default:

- A read-only retention report (FR-17).
- Documentation of what a snapshot costs on disk and how to reclaim it, including `git pack-refs`.
- Runbooks.
- One confirmed delete. It drops one whole snapshot namespace, which is `latest`, every item, and every other ref under it (FR-18).

Selective retention by age or count waits for an operator decision to stop chaining snapshots. The operator can change the default. The measured choices are in the following table.

| Choice | What it frees | Cost |
| --- | --- | --- |
| Default. A report and a whole-namespace drop. | Every object of the namespace, after `git gc --prune=now` (drop probe G2, 1284 KiB to 1 KiB). | No selection. A person keeps a whole timeline or drops it. |
| A selective cleanup by age and count, the first revision. | Nothing. Deleting items while `latest` stays kept every object after `git gc --prune=now` in all 8 configurations (round-1 storage probe). | Its one measured gain is a faster ref scan, which `git pack-refs --all` already gives. At 10,001 loose refs, `list` took 1046 and 1428 ms, and 192 to 321 ms once packed (round-1 scale probe). Every snapshot that a 0.1.0 binary wrote has no origin, so its protection rules could never select one. |
| An archive move. Move items to another ref prefix. | Nothing. | Its gain is a shorter `list`, which a person never sees, because `list` already returns at most the newest 100. |
| Unchain new snapshots. A create names `HEAD` as the commit parent and records the previous snapshot in its message. | The deleted snapshot's own objects. Round-1 review measured 1539 KiB to 513 KiB after it deleted four items and ran `git gc --prune=now`. | It changes the MUSTs in FR-2 and FR-6 and the meaning of `parent` in section 8.2. A 0.1.0 binary that reads a new snapshot reports `changed_files` against `HEAD` and no `parent_id`. Snapshots written before the change stay chained and free nothing. |
| No delete in Phase 6. A report and documentation only. | Nothing. | Removed worktrees and moved repositories leave namespaces that nothing in Blink can reach. |

The default beats the selective cleanup because the drop is the only delete that frees disk under chaining, and it needs no selection rules. It also covers the namespaces that removed worktrees and moved repositories leave behind. The default beats unchaining because unchaining changes the parent semantics of every snapshot, which is a spec decision for the operator and not a part of this docs change.

## What a snapshot costs on disk

Each snapshot commit names the previous snapshot as its parent (FR-2, FR-6). `latest` names the newest commit, so `latest` reaches every snapshot of its namespace. Deleting an item ref while `latest` stays makes no object unreachable. The round-1 storage probe showed it on Git 2.54 and 2.56, SHA-1 and SHA-256, files and reftable.

A snapshot of text edits is small. Round-1 review recorded 300 snapshots of one-line edits in a clone of this repository, and the packed size grew from 1007 KiB to 1167 KiB after `git gc`, about 0.5 KiB a snapshot. Disk grows fast only when large files that are not ignored keep changing, because every changed blob is stored again.

Dropping a namespace removes every ref that reaches its commits. The drop probe recorded five snapshots with a 256 KiB random file each, then dropped the namespace in all 8 configurations:

- A plain `git gc` kept the oldest dropped commit (G1). Git keeps unreachable objects for `gc.pruneExpire`, two weeks by default.
- `git gc --prune=now` removed it, and the pack went from 1284 KiB to 1 KiB (G2).
- The linked worktree's namespace and its commit stayed (D4, G3).

A dropped namespace's refs have no reflog (D4), so Git keeps no other record of them.

A long timeline also slows ref scans. A `list`, and a `create` whose tree did not change, read every item ref of the namespace. `git pack-refs --all` packs loose refs into one file. Git's automatic `gc --auto` runs it, because `gc.packRefs` defaults to true. That last fact is inferred from Git's documentation, and no probe ran an automatic gc.

## Blink never deletes on its own

No create, list, host event, timer, or other automatic step deletes a snapshot ref. Only the CLI `drop` command deletes, after a person types the prompt's code on the terminal. This is the gate default `g-retention-default`, which the operator can change.

## Two operations with one safety class each

| Operation | Writes | Who can run it |
| --- | --- | --- |
| Retention report (FR-17) | Nothing. | The protocol, `SnapClient`, the CLI, and both host tools. |
| Drop (FR-18) | Deletes every ref of one namespace, and writes one record file outside the repository. | The CLI only, after a person types the code on the controlling terminal. |

No JSONL method deletes a ref. `serve` therefore cannot delete a snapshot. Lifecycle hooks and agent tools reach the core only through `SnapClient` and `serve`, so they cannot delete one either. This beats a `snapshot.drop` method behind a one-shot entry like FR-16's `serve --restore`. That entry exists because Pi applies a restore from its terminal UI. No host needs to delete, and a method that does not exist needs no guard.

The read-only report lands in every layer before the drop, in [Phase 6 lands in this order](#phase-6-lands-in-this-order).

## The report lists every namespace in the repository

`snapshot.retention_report` takes `cwd` and an optional `offset`. It reads every ref under `refs/agent-snapshots/` with one `for-each-ref` that prints the ref name, the object ID, the object type, and the symbolic target. It never reads a commit message, so its cost does not depend on reason length. It groups the refs by the path part after `refs/agent-snapshots/`, which is the namespace.

For each namespace it reports:

- The namespace, and whether it is the namespace of `cwd`.
- The worktree it belongs to, when one does.
- The number of refs, and the number of item refs whose names are snapshot identifiers.
- The oldest and newest of those identifiers and their times.
- The state of `latest`, which is `commit`, `missing`, `symbolic`, or `not_commit`.
- A digest of every ref under the namespace with its value.

### A namespace maps to a worktree through the common Git directory

A worktree's namespace is the first 16 hex digits of the SHA-256 of its absolute Git directory. For the main worktree, that directory is the common Git directory. For a linked worktree, it is `<common>/worktrees/<name>`. The drop probe checked both in all 8 configurations (M1, M2). In a linked worktree, `git rev-parse --git-common-dir` printed the same absolute path that `--absolute-git-dir` printed in the main worktree, with symbolic links resolved on both Gits.

The report computes the namespace of the common directory and of each entry under `<common>/worktrees/`. A namespace that none of them gives belongs to no current worktree. After `git worktree remove`, the worktree's entry was gone and its namespace stayed (W1). The report shows the worktree path that the entry's `gitdir` file names.

A new linked worktree that takes a removed worktree's name derives the same namespace, and the report shows the old snapshots under the new worktree. Round-1 review measured this. It is a Phase 4 bug on main, and [Follow-ups](#follow-ups) records it. The drop prompt shows the snapshot times, so a person can see that a timeline is older than its worktree.

### The digest covers every ref with its value

The digest is the lowercase hex SHA-256 of one line per ref under the namespace, in ref-name order. A line is the full ref name, a space, the object ID the ref resolves to, a space, the symbolic target or `-`, and a line feed. A ref that resolves to nothing has the zero object ID.

`for-each-ref` skips a symbolic ref whose target does not exist, and prints no warning (drop probe X4). The report therefore also reads `latest` of each namespace it knows with `git symbolic-ref -q`, where `for-each-ref` did not list `latest`. A dangling symbolic `latest` gets a digest line with the zero ID and its target. A dangling symbolic item ref stays invisible to every listing command. Only `git fsck` names it (X4). Blink never writes a symbolic ref, so such a ref exists only when something else wrote it. The corrupt-ref runbook covers it.

Any create, deletion, or change under the namespace changes the digest. The drop probe saw a create change it (D2).

### The report tool speaks to the person

Both hosts get `agent_snap_retention_report`. Its text lists each namespace's fields under the quoting rule of host tool text. It ends with a line for the person, not an instruction to the agent: "This report changed nothing. A person can drop one namespace from their own terminal with `agent-snap drop <namespace> --digest <digest>`."

## The drop deletes one namespace in one transaction

`agent-snap drop <namespace> --digest <hex> [--cwd <dir>]` takes the namespace and the digest from a report. It deletes in one `git update-ref --no-deref --stdin` transaction. The transaction holds one line for each ref the report read:

- `delete <ref> <object-id>` for a ref that resolves to an object, whether it is direct or symbolic. With `--no-deref`, Git deletes a symbolic ref and leaves its target. It checks the old value against the resolved object (X3).
- `verify <latest> <zero-id>` when `latest` is missing. A `latest` that a create writes after the report then refuses the drop with `reference already exists` (X1).
- `delete <latest>` with no old value when `latest` is a dangling symbolic ref. Git 2.30 has no command that checks a symbolic target, and Git refused a zero old value for one (X4). No Blink operation writes a symbolic `latest`, because a create refuses to update one, so only a hand edit can change it in this window. That is an accepted limit.

The drop probe showed these on Git 2.54 and 2.56, SHA-1 and SHA-256, files and reftable:

- One stale old value refused the whole transaction, and nothing was deleted (D1).
- With every old value right, the namespace was empty afterwards. The branches and the linked worktree's namespace were unchanged (D4).
- A `latest` that names a blob was deleted (X2). A symbolic `latest` and a symbolic item were deleted, and `refs/heads/main` was unchanged (X3).

The drop has no pins, no protection rules, and no selection. A person keeps the whole namespace or drops it.

### A drop deletes at most 10,001 refs

The drop-time probe ran the real drop transaction, one `delete` with its old value for each of N items and for `latest`. It ran before the other probes of this revision, on a machine that other agents' tests also used, so the times are upper bounds:

| Refs | Files, loose | Files, packed | Reftable |
| --- | --- | --- | --- |
| 1,001 | 0.25 to 0.28 s | 0.19 to 0.25 s | 0.03 to 0.06 s |
| 5,001 | 1.27 to 1.65 s | 0.82 to 1.16 s | 0.04 to 0.06 s |
| 10,001 | 2.67 to 3.12 s | 1.71 to 2.09 s | 0.06 to 0.08 s |

The range covers Git 2.54 and 2.56 with SHA-1 and SHA-256. Round-1 review measured 6 to 16 s for 10,000 deletes, but its transaction also verified 10,000 pin refs, which the drop does not have.

A namespace with more than 10,001 refs is refused before the prompt, because no larger transaction was measured. The prompt says how long the drop may take on the files backend. The round-1 scale probe ran at 10,001 refs, so a namespace past the cap is past every size this record measured. The runbook row says how to drop it by hand.

### A concurrent create meets the drop at one of five seams

A create writes one new item and moves `latest` from the value it read, in one transaction. The drop deletes `latest` with the value the report read. Their transactions therefore conflict on `latest`, and Git lets exactly one of them commit.

| Seam | What happens | Evidence |
| --- | --- | --- |
| A create commits between the report and the drop's first read. | The drop's digest differs from `--digest`. It refuses with exit 4 before the prompt. | D2 |
| A create commits while the prompt waits. | The recompute after the code differs. The drop refuses with exit 4. | D2 |
| A create commits between the recompute and the transaction. | Git refuses the drop with `is at <oid> but expected <oid>` on `latest`. Nothing is deleted. | D3, race probe |
| A create's transaction runs while the drop's transaction holds its locks. | One of the two refuses. In the race probe, either the create took `latest` first and the drop refused with nothing deleted, or the drop committed and the create failed. | Race probe |
| A create read `latest` before the drop committed, and writes after it. | Git refuses the create with `unable to resolve reference` on `latest`, and the create writes nothing. Today's `commitRefs` retries only on `but expected`, so that create fails once with an error. The next create succeeds. | D6, race probe |

A create after a drop starts a new chain on `HEAD` (D5). The landing order adds a Store step before the drop that retries a create when `latest` disappeared after it was read, so that the last seam costs no failed create.

### Git runs in its own process group with files for its streams

From just before Git starts until the after-read ends, the CLI ignores `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT`. Git does not inherit that. Once Git takes its first lock it installs its own handlers. The signal probe sent `SIGINT` and `SIGHUP` to the CLI's process group after Git took its first lock, the way a terminal sends Ctrl-C and a hangup:

- With Git in the CLI's process group, Git 2.54 aborted with exit 128 and `couldn't close '<ref>.lock'` in 1 of 3 runs, and nothing was deleted. The other runs finished.
- With Git in its own process group, every run on both Gits finished with exit 0 and an empty namespace.

So the CLI starts Git in its own process group, as FR-16 starts its write. A terminal's Ctrl-C and hangup then reach only the CLI, which ignores them.

The CLI writes the whole transaction, `commit` included, to the record file before Git starts. Git reads it as standard input and writes standard output and standard error to files beside it. The signal probe killed the CLI with `SIGKILL` after Git took its first lock:

- With Git's standard output on a pipe to the dead CLI, every run left every ref in place.
- With Git's standard output on a file, every run finished the drop.

A pipe makes the result depend on when Git next writes. With files, Git finishes the transaction it was given whatever happens to the CLI, and the after-read of the next report shows the result.

### A killed Git leaves lock files, and a person can remove them safely

A `SIGKILL` sent to Git itself, or a crash, can stop it while it holds locks. The kill probe ran 10,001-ref drops on the files backend, loose and packed, on both Gits, and killed Git at delays from 0.3 to 2.5 seconds:

- A kill during `prepare` deleted nothing and left 3,007 to 10,001 `.lock` files under the namespace.
- A kill during `commit` left either every ref deleted, or a part. One Git 2.56 run left 2,499 of 10,001 refs, `latest` among them. Each of these runs also left lock files and `packed-refs.lock`.
- With a stale `packed-refs.lock`, `git branch -d` failed, as round-1 review measured for `git pack-refs` and `git gc` too.

Git writes a ref's new state into its `.lock` file and renames it into place. A lock left by a dead process therefore holds nothing that Git still needs, and removing it is safe once no Git process runs in the repository. After the probe removed the stale locks, the drop ran again and emptied the namespace, and `git branch -d`, `git pack-refs --all`, and `git fsck` succeeded, in 17 of 18 runs. In one Git 2.54 packed run, the second drop and `git pack-refs` failed again with exit 128, and the probe did not keep the message. A rerun of that configuration at 13 delays with the messages kept is in `drop-kill-r2.log`. The runbook row gives the steps, and the `unknown` outcome covers a partial drop.

### The outcome comes from Git's status and the after-read together

After Git exits, the CLI reads the namespace again with the reads of the report. A listed ref is present when it still has the value the transaction listed.

| Outcome | When | CLI exit |
| --- | --- | --- |
| `dropped` | Git exited 0, and no listed ref is present. | `0` |
| `refused` | Git did not exit 0, and every listed ref is present. | `4` |
| `unknown` | Any other result, or the after-read failed. | `3` |

A listed ref that is absent after a failed transaction is never reported as dropped. Another process may have deleted it. Git then refuses the whole transaction with `unable to resolve reference`, which the round-1 ref probe recorded. The outcome is then `unknown`, and the result names the absent refs.

The exit status of the CLI follows the outcome once Git has started, also when printing the result fails afterwards. Exit `1` means an error before Git started, so nothing was deleted. Exit `4` means nothing was deleted. Exit `0` means the namespace was dropped. Every other status, `3` and a death by signal included, means refs may be gone. The CLI never exits `2` on purpose, because Go exits `2` on a panic.

## The record is written before anything is deleted

An identifier cannot be rebuilt from its commit. `recordTree` takes the identifier's time from the clock after `commit-tree`, and the commit stores whole seconds. So the drop writes the transaction it will run to a record file before Git starts, and gives that file to Git as standard input. The record holds the ref names, which contain the identifiers, and the object IDs. It holds no reason, path, or file content.

The record is `<user cache directory>/agent-snap/drops/<UTC time>-<namespace>.txt`, where the user cache directory is Go's `os.UserCacheDir`. The CLI creates the directory with mode 0700 and the file with mode 0600, refuses a path inside the worktree as the scratch rules do, and syncs the file and its directory before Git starts. When any of that fails, the drop exits `1` and deletes nothing. A kill after Git starts leaves the record in place, because Git reads the file and the CLI never removes it.

The prompt and the result name the record. A person recreates the dropped refs with:

```sh
sed -n 's/^delete \(refs\/agent-snapshots\/[^ ]*\) \([0-9a-f]*\)$/create \1 \2/p' <record> | git update-ref --stdin
```

The recovery probe dropped a namespace from such a file and recreated every ref with its value on both Gits. Recovery works only while the objects exist. A plain `git gc` keeps them for `gc.pruneExpire`, two weeks by default, and `git gc --prune=now` removes them at once (G1, G2). The prompt says so.

## Only the CLI drops, after a person types a random code on the terminal

The command reuses the FR-16 CLI rules. It checks that standard input, standard output, and standard error are each the controlling terminal, then opens the terminal, and refuses with exit `4` when any check fails. It has no `--yes`, and it never reads the code from standard input.

The code is six hex digits from `crypto/rand`, made for each prompt and printed only on the terminal. No report, record, tool result, or earlier run holds it. Round-1 review showed why it must not come from the digest. The report tool returns the digest, so an agent could tell the person what to type without the person reading the prompt.

Blink does not claim to stop an agent that has a shell. Such an agent can run Git itself, start its own `serve`, or give the CLI a terminal. Round-1 review measured two ways. A driver built on Python's `pty.fork()` passed every terminal check and typed the code it read from the prompt. A process that inherited the person's controlling terminal pushed input into it with `TIOCSTI` on macOS. The guarantee covers `serve`, host tools, lifecycle hooks, `SnapClient`, and any caller that gives the CLI pipes. [`RESTORE.md`](RESTORE.md) states the same limit for a restore. The random code stops blind injection, because the injector cannot know the code. It does not stop a process that reads the terminal.

The prompt shows:

- The namespace, whether it is the namespace of `cwd`, and its worktree or that no current worktree maps to it.
- The number of refs and of snapshots, the oldest and newest identifier with their times, and the state of `latest`.
- The record path, and that recovery works only until the next `git gc` that prunes.
- That the drop takes up to a few seconds and the terminal must stay open.
- The code.

Ctrl-C, `SIGTERM`, or `SIGHUP` at the prompt declines. The drop prints that nothing was deleted and exits `4`. A wrong line, an empty line, or the end of input declines the same way.

The drop refuses on Windows until a Windows CI job runs the terminal cases, as the restore does.

## Observability stays with the existing diagnostics

Phase 6 adds no event or metric. Work item 4 asks for observability that records operation metadata and never source contents or secrets. These already meet it:

- The drop result and the record hold ref names, identifiers, object IDs, counts, Git's exit status, and Git's message. [`DIAGNOSTICS.md`](DIAGNOSTICS.md) already lists identifiers and object IDs as safe to record.
- The report holds namespaces, counts, identifiers, times, and worktree paths, which a host already has as `cwd`.
- The core writes no log file, and hosts log through their own logger under the DIAGNOSTICS rules.

The first revision proposed a `SnapOperationEvent` from `SnapClient`. It could never see a drop, because the drop is CLI-only, and nothing in Phase 6 consumes it. A restore outcome event belongs with the Phase 5 restore. Leaving it out removes an abstraction that the phase does not need.

## Runbooks

[`DIAGNOSTICS.md`](DIAGNOSTICS.md) already covers stuck child processes ("Teardown failures", and "Scratch directories left after a hard kill") and corrupt item refs (the rows for symbolic, foreign, and tampered item refs, and the ref lock row).

Each landing step adds the rows for what it adds.

| Row | Says |
| --- | --- |
| `drop needs a terminal` | Run `agent-snap drop` from a terminal. An agent's shell tool cannot confirm. Nothing was deleted. |
| `drop declined` | The line did not match the code, or the drop was interrupted at the prompt. Nothing was deleted. |
| `namespace changed since the report` | A create or another change moved the namespace. Run the report again, and drop with its new digest. Nothing was deleted. |
| `namespace holds more than 10001 refs` | Run the report. Delete the namespace by hand with one `git update-ref --stdin` transaction that holds the record's lines, after you write them to a file you keep. |
| `refused` outcome | Git refused the transaction, and every ref is still in place. The result holds Git's message. A `but expected` on `latest` means that a create ran. Run the report again. |
| `unknown` outcome | Refs may be gone. Run the report to see what is left, and keep the record. The result lists the absent refs. |
| Stale `.lock` files after a killed drop | Git reports `Unable to create '<path>.lock': File exists`. Check that no Git or `agent-snap` process runs in the repository, for example with `ps -A -o pid,command | grep -E 'git|agent-snap'`. Then remove the `.lock` files under `refs/agent-snapshots/<namespace>/` in the Git directory and `packed-refs.lock`, and run the report again. Never remove a lock while a Git process runs. |
| Recovery after a drop | Run the `sed` and `git update-ref` command in [The record is written before anything is deleted](#the-record-is-written-before-anything-is-deleted) on the record. It works until the next `git gc` that prunes. |
| A dangling symbolic item ref | `git fsck` names it, and no listing shows it. Delete it with `git update-ref --no-deref -d <ref>`. |
| Large repositories | Run `git pack-refs --all` for faster `list` and unchanged creates. A drop followed by `git gc --prune=now` frees a namespace's objects. Deleting items never frees anything while `latest` stays. |

## Phase 6 lands in this order

Each step is one pull request that ends in a check. Each step that adds a method, an error code, a CLI command, or a host tool updates FR-8 through FR-11, `README.md`, `docs/COMPATIBILITY.md`, and `docs/DIAGNOSTICS.md` in the same pull request. Store tests run on real repositories, SHA-1 and SHA-256, files and reftable. The read-only report lands in every layer, steps 2 to 6, before step 7 starts the drop.

1. This record, FR-17, FR-18, and sections 8.7 and 8.8. The check is review and a link check.
2. Store. `Store.RetentionReport`, read-only. Its check is `go test ./...` with these tests:
   - `TestRetentionReportListsEveryNamespace` creates snapshots in the main worktree and a linked one, and plants a namespace that no worktree maps. It checks each namespace's counts, oldest and newest, `current`, and `worktree`.
   - `TestRetentionReportMapsAWorktreeByItsCommonDirectory` runs from a linked worktree in a repository under a symbolic link, and checks that both namespaces map.
   - `TestRetentionReportMarksARemovedWorktree` removes a linked worktree and checks that its namespace has no worktree.
   - `TestRetentionReportReadsEachLatestState` plants a missing, a blob, a symbolic, and a dangling symbolic `latest`, and checks each state and digest line.
   - `TestRetentionReportDigestChangesOnEveryRefChange` creates, deletes, and repoints one ref in turn.
   - `TestRetentionReportIgnoresMessageSize` plants an item with a 4 MiB message, and checks that the report succeeds and holds no reason text.
   - `TestRetentionReportWritesNothing` fingerprints every ref, the index, `HEAD`, the configuration, the object store, and the file list of the Git directory, before and after.
3. Protocol. `snapshot.retention_report` with its validation table and `-32601` from an older binary. `TestServeRetentionReportRejectsUnknownKeys` and `TestServeRetentionReportPagesNamespaces` cover the request rules and `next_offset`. The check is `go test ./...`.
4. TypeScript. The section 8.7 types and every cross-field rule, `SnapClient.retentionReport`, `SnapLike.retentionReport`, and `SnapRetentionReportUnsupportedError` for `-32601`. Tests cover a digest that is not 64 hex digits, namespaces out of order, a `next_offset` that breaks the paging rule, an identifier that fails the FR-12 syntax, and `oldest` after `newest`. The check is `bun run test` and `bun run test:node`.
5. CLI `retention-report [--cwd <dir>] [--offset <n>]`. `retention-report prints one page` and `retention-report refuses an unknown flag`. The check is `go test ./...`.
6. Hosts. `agent_snap_retention_report` in both hosts. `retention report tool text ends with the line for the person` checks both hosts, and checks that the text never tells the agent to run the command. The OpenCode and Pi tests check that neither host registers a drop tool. The check is `bun run test`.
7. Store. `commitRefs` retries when `latest` disappeared after the create read it, as it retries on `but expected`. `TestCreateRetriesWhenLatestWasDeleted` deletes `latest` and its item at the seam between the create's read and its transaction, and checks that the create succeeds with `HEAD` as the parent. The check is `go test ./...`.
8. Store. `Store.Drop`, with its tests below. The check is `go test ./...`.
9. CLI `drop`, with its tests below, driven through `internal/ptytest` from Phase 5 step 9 of [`RESTORE.md`](RESTORE.md). If Phase 5 has not landed it, this step adds it. The check is `go test ./...`.

The Store drop step (8) adds these tests. Tests reach points in the sequence through `afterDropStep`, an unexported package variable like the restore's seam.

- `TestDropDeletesEveryRefOfTheNamespaceOnly` fingerprints every ref in the repository, with a linked worktree, and checks that only the namespace's refs changed.
- `TestDropRefusesAStaleDigest` creates a snapshot before the first read and expects `ErrNamespaceChanged` with nothing deleted.
- `TestDropRefusesAConcurrentCreate` runs a create at the seam between the recompute and the transaction, and expects `refused` with every ref present.
- `TestDropRacesCreates` repeats the race probe and checks that every run ends with either nothing deleted, or no listed ref present and every snapshot created after the drop on a chain that starts at `HEAD`.
- `TestDropEachLatestState` drops namespaces with a missing, a blob, a symbolic, and a dangling symbolic `latest`, and a symbolic item, and checks that the namespace is empty and `refs/heads/main` is unchanged. For the missing case, it also writes `latest` at the seam and expects `refused`.
- `TestDropUnderPackedRefs` packs every ref first. `TestDropOnReftable` runs the suite on a reftable repository.
- `TestDropDoesNotClaimARefDeletedByAnotherProcess` deletes one listed ref at the seam before the transaction, and expects `unknown` with that ref absent and every other ref present.
- `TestDropReportsUnknownWhenTheAfterReadFails` replaces the after-read with one that fails, and expects `unknown`.
- `TestDropDecidesTheOutcomeFromStatusAndRefs` replaces the Git runner, an unexported package variable, with runners that exit 0, exit 128, and die by `SIGKILL`, each after it deleted none, half, or all of the listed refs. It checks the outcome table.
- `TestDropRunsNoReferenceTransactionHook` plants a `reference-transaction` hook that aborts, and checks that the drop succeeds.
- `TestDropRefusesAHeldLock` holds a listed ref's `.lock` file and expects `refused` with Git's message.
- `TestDropRefusesMoreThanTheCap` plants 10,002 refs with `git fast-import`. `TestDropDeletesAtTheCap` plants 10,001 and checks that the namespace is empty.
- `TestDropFreesObjectsAfterPrune` runs `git gc --prune=now` after a drop and checks that the oldest dropped commit is gone and the other namespace's commits remain.
- `TestDropWritesTheRecordBeforeGit` makes the Git runner check that the record exists, is synced, and equals its standard input.
- `TestDropRecordRecreatesTheNamespace` runs the runbook command on a record and checks that every listed ref is back with its value.

The CLI drop step (9) adds these tests.

- `drop refuses without a terminal`, and once for each of the three streams as a pipe while `/dev/tty` opens. Each exits `4`, deletes nothing, and writes no record.
- `drop declines a wrong code`, `drop declines an empty line`, and `drop declines the end of input`. Each exits `4` and deletes nothing.
- `drop declines a code derived from the report` types the last six hex digits of the digest, and exits `4` with nothing deleted. The test fixes the random source so that the code differs from them.
- `drop codes differ between runs` runs the prompt twice for one namespace and digest.
- `drop declines an interrupt at the prompt` sends `SIGINT` while the prompt waits, and expects exit `4` with nothing deleted.
- `drop deletes after the code` checks the result JSON, exit `0`, the record, and the refs.
- `drop refuses when the namespace changed during the prompt` creates a snapshot while the prompt waits, and exits `4` with nothing deleted.
- `drop survives signals after Git took its first lock` re-executes the test binary as the CLI with a 5,000-item namespace on the files backend. It polls for a `.lock` file under the namespace, sends `SIGINT` and `SIGHUP` to the CLI's process group, and expects exit `0` with the namespace empty.
- `drop finishes when the CLI is killed` kills the CLI with `SIGKILL` after Git took its first lock, waits for Git to exit, and checks that the namespace is empty and the record is in place.
- `drop exits 3 when the after-read fails` uses the Store seam.
- `drop through a pty driver confirms` drives the CLI with a pty that reads the prompt and types the code. It passes, and it records that a process holding a terminal is outside the guarantee.
- `drop prompt quotes the worktree path` uses a path with a line feed and an escape byte.
- `drop refuses on Windows` runs on the Windows CI job.

## Proposed safety invariants for AGENTS.md

AGENTS.md is held by PR #33, so these invariants are proposed here. The coordinator adds them when that PR lands.

- Blink never deletes a snapshot ref on its own. No create, list, host event, or timer deletes one.
- No JSONL method deletes a ref. Only the CLI `drop` command deletes snapshot refs, and only after a person types a code from `crypto/rand` that the prompt printed on the controlling terminal. The guarantee covers `serve`, host tools, lifecycle hooks, `SnapClient`, and callers that give the CLI pipes. Blink does not claim to stop an agent that has a shell or a terminal.
- A drop deletes only refs under one namespace that its report read, in one `git update-ref --no-deref --stdin` transaction that checks each ref's old value. It never writes a ref, the index, `HEAD`, configuration, objects, or working-tree files.
- A drop writes its record outside the repository before Git starts, and runs Git in its own process group with files for its standard streams.
- A drop decides its outcome from Git's exit status and a read of the refs after Git exits. It never reports a ref as dropped unless Git exited 0, and it never tells the person that nothing was deleted when a listed ref is absent.

## How the design meets the Phase 6 exit criteria

| Exit criterion | Met by |
| --- | --- |
| Retention cannot delete a protected checkpoint or a ref outside Blink's namespace. | A drop deletes only refs under one namespace that the report read and the person confirmed, with an old-value check on each. The person protects a checkpoint by not dropping its namespace. Store step 8 tests it. |
| Operators can diagnose failures without accessing developer source code. | The runbook rows, the drop result, and the record, which hold no file content. |
| Storage behavior and cleanup safety are documented and tested. | [What a snapshot costs on disk](#what-a-snapshot-costs-on-disk), its probes, and `TestDropFreesObjectsAfterPrune`. |

## Decisions

| Decision | Beats | Because |
| --- | --- | --- |
| Drop one whole namespace. | A selective cleanup, an archive move, or unchaining in Phase 6. | It is the only delete that frees disk under chaining (G2). The others free nothing or change FR-2 and FR-6. |
| The report lists every namespace in the repository. | A report of the `cwd` namespace only. | Namespaces of removed worktrees and moved repositories are the ones no worktree can reach. |
| No JSONL method deletes. | A `snapshot.drop` method behind a one-shot entry. | No host needs to delete, and a method that does not exist needs no guard. |
| The digest covers every ref with its value. | A digest of items only, or a count. | Any create or change between the report and the delete refuses the drop. |
| One transaction with an old value per ref, capped at 10,001 refs. | Batches, or no cap. | A failure refuses the whole drop, and the cap is the largest measured size (drop-time probe). |
| Git runs in its own process group with files for its streams. | Git in the CLI's group, or pipes. | Git's own handlers aborted a drop on a group signal, and a pipe to a dead CLI changed the result (signal probe). |
| The outcome needs Git's exit 0 for `dropped`. | The after-read alone. | A ref that another process deleted would otherwise count as dropped. |
| The record is written before Git starts and is Git's input. | Printing the result only. | A kill leaves no result, and identifiers cannot be rebuilt from commits. |
| The code comes from `crypto/rand` for each prompt. | The digest's last six hex digits. | The report tool returns the digest, so an agent could tell the person the code. |
| No `SnapOperationEvent` in Phase 6. | An `observe` callback in `SnapClient`. | It could never see a drop, and nothing in the phase consumes it. |

## Questions for review

- Is 10,001 refs the right cap, and is a by-hand runbook acceptable above it?
- Is the user cache directory the right home for the record, given that a person or the system can clear it?
- Should the report also flag a namespace whose oldest snapshot is older than its worktree's creation, the sign of name reuse?

## Follow-ups

- Namespace reuse. A new linked worktree created under a removed worktree's name inherits its timeline. `list` shows the old snapshots, and a restore can target them. This is a Phase 4 bug on main.
- FR-16 confirmation code. The FR-16 code is the backup ID's last six hex digits, and the plan tool prints the backup ID. The same `TIOCSTI` injection works on it. Carry a random code into the FR-16 landing step that builds the CLI prompt.
- A reused CLI create. `agent-snap create` in an unchanged tree returns the automatic snapshot it reused, with that snapshot's origin. The CLI could print `reused` and the origin.
- Unchaining and selective retention. Both wait for the operator's decision under `g-retention-scope`.
- A create whose `latest` names a commit with no item ref fails until the tree changes (round-1 latest probe). This is a Store fix outside Phase 6.

## Evidence

The probes ran on macOS on Git 2.54.0 (Apple Git-157) and Homebrew Git 2.56.0, with an `agent-snap` built from main at `ff57d46`. Each ran in fresh `mktemp -d` repositories with a private `HOME`, `GIT_CONFIG_NOSYSTEM=1`, and no inherited `GIT_*` variables. The scripts and logs are kept with the Phase 6 program's evidence, outside this repository, in `p6-design` for round 1 and `p6-design-r2` for this revision.

- The drop probe ran SHA-1 and SHA-256, files and reftable, on each Git, 8 configurations with 43 checks each. All 344 checks passed. It covers M1, M2, D1 to D6, G1 to G3, X1 to X4, and W1 above.
- The drop-time probe ran the drop transaction at 1,001, 2,001, 5,001, and 10,001 refs in all 8 configurations, with files loose, files packed, and reftable.
- The race probe ran real creates in a loop while a 5,000-item drop ran, with the creates starting 0, 0.1, 0.3, and 0.8 seconds after the drop, in all 8 configurations.
- The signal probe ran a 5,000-item drop on the files backend from a stand-in CLI that ignores the stop signals. It ran Git in the CLI's process group and in its own, and it killed the CLI with Git's output on a pipe and on a file.
- The kill probe ran 10,001-ref drops on the files backend, loose and packed, on both Gits, killed Git with `SIGKILL` at 18 delays, and then ran the stale-lock recovery.
- The recovery probe ran the runbook command on a record on both Gits.
- Round 1 measured storage, deletion, packed refs, worktrees, moved repositories, and ref scan times. Its review measured disk growth, unchaining, the pty driver, `TIOCSTI`, and the stale `packed-refs.lock`.

Not observed. Windows, Git older than 2.54, Linux, and an automatic `git gc` or `git maintenance` run.

## Assumptions

Each assumption the design relies on is listed with its label. An observed fact has a probe. An inferred fact follows from code or documentation that this record read but did not run. A requirement is a rule that a later pull request builds and the named test proves.

| Assumption | Label | Proven by |
| --- | --- | --- |
| A drop followed by `git gc --prune=now` frees the namespace's objects, and a plain `git gc` keeps them. | Observed (drop probe G1, G2) | `TestDropFreesObjectsAfterPrune`. |
| Deleting items while `latest` stays frees nothing. | Observed (round-1 storage probe) | None. Phase 6 builds no such delete. |
| A worktree's namespace derives from the common directory or `<common>/worktrees/<name>`. | Observed (drop probe M1, M2) | `TestRetentionReportMapsAWorktreeByItsCommonDirectory`. |
| `for-each-ref` skips a dangling symbolic ref without a warning, and `symbolic-ref -q` reads one. | Observed (X4) | `TestRetentionReportReadsEachLatestState`. |
| One stale old value refuses the whole drop transaction. | Observed (D1, D3) | `TestDropRefusesAConcurrentCreate`. |
| `--no-deref` deletes a symbolic ref with its resolved value and leaves the target. | Observed (X3) | `TestDropEachLatestState`. |
| `verify <ref> <zero-id>` refuses when the ref exists. | Observed (X1) | `TestDropEachLatestState`, missing case. |
| A create that read `latest` before a drop fails with `unable to resolve reference`, and writes nothing. | Observed (D6, race probe) | `TestCreateRetriesWhenLatestWasDeleted`. |
| A create after a drop starts a new chain on `HEAD`. | Observed (D5) | `TestDropRacesCreates`. |
| A 10,001-ref drop takes at most about 3 s on the files backend and under 0.1 s on reftable. | Observed (drop-time probe, shared machine) | `TestDropDeletesAtTheCap`. |
| Git in the CLI's process group can abort on a group signal after its first lock, and Git in its own group finishes. | Observed (signal probe) | `drop survives signals after Git took its first lock`. |
| Git with its streams on files finishes a drop after the CLI dies. | Observed (signal probe) | `drop finishes when the CLI is killed`. |
| A stale lock from a killed Git is safe to remove once no Git process runs. | Observed in 17 of 18 kill-probe runs, with one unexplained failure. Inferred from Git's lock-file protocol for every kill point. | The runbook row. |
| A dropped ref has no reflog, so the record is the only list of what a drop deleted. | Observed (D4). Inferred for `core.logAllRefUpdates=always`, which would log them. | `TestDropWritesTheRecordBeforeGit`. |
| An agent's shell tool starts the CLI with pipes on all three streams. | Inferred, as in [`RESTORE.md`](RESTORE.md) | `drop refuses without a terminal`. |
| A process that holds a terminal can confirm a drop. | Observed by round-1 review (pty driver, `TIOCSTI`). Accepted limit. | `drop through a pty driver confirms`. |
| `gc --auto` packs refs, because `gc.packRefs` defaults to true. | Inferred from Git's documentation | None. |
| Windows terminal checks are not proven, so the drop refuses on Windows. | Accepted limit | `drop refuses on Windows`. |
