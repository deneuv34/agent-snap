# Retention design

**Status:** Proposed, revision 3. Phase 6, [#7](https://github.com/deneuv34/agent-snap/issues/7). Nothing in this release deletes a snapshot ref.

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

- `git gc --prune=now` removed the oldest dropped commit, and the pack went from 1284 KiB to 1 KiB (G2).
- The linked worktree's namespace and its commit stayed (D4, G3).

A plain `git gc` keeps an unreachable object only while it is younger than `gc.pruneExpire`, two weeks by default. Git judges an object in a pack by the age of its pack. The gc-age probe dropped a namespace in all 8 configurations and then ran a plain `git gc`. When the pack had just been written, the oldest dropped commit stayed. When the pack was last written 30 days before, the commit was gone after that one `git gc`. Snapshots that an earlier `git gc` packed are usually that old. So recovery from a drop can end at the next `git gc`, and Git can start `gc --auto` after ordinary commands. That last fact is inferred from Git's documentation, and no probe ran an automatic gc.

A dropped namespace's refs have no reflog (D4), so Git keeps no other record of them.

A long timeline also slows ref scans. A `list`, and a `create` whose tree did not change, read every item ref of the namespace. `git pack-refs --all` packs loose refs into one file. Git's automatic `gc --auto` runs it, because `gc.packRefs` defaults to true. That is inferred from Git's documentation.

## Blink never deletes on its own

No create, list, host event, timer, or other automatic step deletes a snapshot ref. Only the CLI `drop` command deletes, after a person types the prompt's code on the terminal. This is the gate default `g-retention-default`, which the operator can change.

## Two operations with one safety class each

| Operation | Writes | Who can run it |
| --- | --- | --- |
| Retention report (FR-17) | Nothing. | The protocol, `SnapClient`, the CLI, and both host tools. |
| Drop (FR-18) | Deletes every ref of one namespace, and writes one record file outside the repository. | The CLI only, after a person types the code on the controlling terminal. |

No JSONL method deletes a ref. `serve` therefore cannot delete a snapshot. Lifecycle hooks and agent tools reach the core only through `SnapClient` and `serve`, so they cannot delete one either. This beats a `snapshot.drop` method behind a one-shot entry like FR-16's `serve --restore`. That entry exists because Pi applies a restore from its terminal UI. No host needs to delete, and a method that does not exist needs no guard. A test in `internal/protocol` keeps it that way. It parses the package's source and fails when any file names `Drop`.

The read-only report lands in every layer before the drop, in [Phase 6 lands in this order](#phase-6-lands-in-this-order).

## The report lists every namespace in the repository

`snapshot.retention_report` takes `cwd` and an optional `offset`. It never reads a commit message, so its cost does not depend on reason length.

### The report reads names and values, and types only `latest`

The report runs three reads. Every Git command runs with `LANGUAGE=C`, as on main, so Git's messages are in English.

1. One `git for-each-ref --format='%(refname) %(objectname) %(symref)' refs/agent-snapshots/`. It reads no object.
2. One `git cat-file --batch-check` with the object ID of each listed `latest`. It prints `missing` for an object the repository does not have.
3. `git symbolic-ref -q <latest>` for each namespace whose `latest` the first read did not list and did not name as broken.

Round 2 printed `%(objecttype)` in the first read. The broken-ref probe showed that one ref to a missing object then makes `for-each-ref` exit 128 with `fatal: missing object`, in all 8 configurations. The report failed for every namespace in the repository, and every drop failed with it. Without `%(objecttype)`, `for-each-ref` exited 0 and listed that ref, and the other namespace still reported. `cat-file --batch-check` then printed `<oid> missing` for a `latest` whose object was gone.

A ref whose file holds no object ID is broken. `for-each-ref` skips it and prints `warning: ignoring broken ref <ref>` on standard error, and exits 0 (broken-ref probe, files backend). The report counts each such line under `refs/agent-snapshots/` as a broken ref of its namespace. `symbolic-ref -q` on a broken `latest` exited 128 with `fatal: No such ref`. The report never runs it on a `latest` that the warning named, and it reports a `symbolic-ref` exit other than 0 and 1 as a broken `latest` too. Reftable stores no file that a person can corrupt this way, so the probe ran these cases on files only.

`for-each-ref` also skips a symbolic ref whose target does not exist, and prints no warning (drop probe X4). The third read finds a dangling symbolic `latest`. A dangling symbolic item ref stays invisible to every listing command, and only `git fsck` names it (X4). Blink never writes a symbolic ref, so such a ref exists only when something else wrote it. A runbook row covers it.

A namespace is the 16 lowercase hex digits after `refs/agent-snapshots/` and before the next `/`. A ref whose path does not have that form, such as `refs/agent-snapshots/README` or `refs/agent-snapshots/tmp/x`, belongs to no namespace. The report counts it in `totals.other` and no drop touches it. The broken-ref probe listed both of those example refs without an error.

For each namespace it reports:

- The namespace, and whether it is the namespace of `cwd`.
- The worktree it belongs to, when one does.
- The number of refs, of item refs whose names are snapshot identifiers, and of broken refs.
- The oldest and newest of those identifiers and their times.
- The state of `latest`, which is `commit`, `not_commit`, `missing_object`, `symbolic`, `missing`, or `broken`.
- A digest of every ref under the namespace with its value.

### A namespace maps to a worktree through the common Git directory

A worktree's namespace is the first 16 hex digits of the SHA-256 of its absolute Git directory, as `git rev-parse --absolute-git-dir` prints it. Git prints that path with symbolic links resolved. For the main worktree, it is the common Git directory C. For a linked worktree, it is `C/worktrees/<name>`.

The report finds C without `--path-format`, which Git 2.31 added:

1. G is what `git rev-parse --absolute-git-dir` prints in `cwd`.
2. When `G/commondir` exists, C is its content joined to G when it is relative, then cleaned, with symbolic links resolved. Otherwise C is G.
3. The main worktree's namespace comes from C. Each entry under `C/worktrees/` gives the namespace of `C/worktrees/<entry>`.

`git rev-parse --git-common-dir` is not used, because it printed `.git` in the main worktree and `../.git` in its subdirectory. A join of that relative path keeps a symbolic link in the path, such as macOS `/var`, that Git resolved to `/private/var` before it hashed. The mapping probe ran the derivation from the root and a subdirectory of the main worktree, a linked worktree, and a worktree made with `--relative-paths`, in a repository under `/var/folders`. In all 8 configurations it gave exactly the namespaces that `agent-snap create` wrote from each of them.

The report shows a worktree path for each mapped namespace. The main worktree's path is the first `worktree` line of `git worktree list --porcelain`. A linked worktree's path is the directory that holds the `.git` file named by the entry's `gitdir` file. The report reads at most 4096 bytes of that file, only when it is a regular file, and joins a relative path to the entry directory. Git 2.48 and later write a relative path there for `--relative-paths`, and the probe saw `../../../../wtrel/.git`. When the file cannot be read, the namespace still maps and the path is absent. After `git worktree remove`, the worktree's entry was gone and its namespace stayed (W1).

A new linked worktree that takes a removed worktree's name derives the same namespace, and the report shows the old snapshots under the new worktree. Round-1 review measured this. It is a Phase 4 bug on main, and [Follow-ups](#follow-ups) records it. The drop prompt shows the snapshot times, so a person can see that a timeline is older than its worktree.

### The digest covers every ref with its value

The digest is the lowercase hex SHA-256 of one line per ref under the namespace, in ref-name order. A line is the full ref name, a space, a value, a space, the symbolic target or `-`, and a line feed. The value is the object ID the ref resolves to, the zero object ID for a ref that resolves to nothing, or `broken` for a broken ref.

Any create, deletion, or change under the namespace changes the digest. The drop probe saw a create change it (D2).

### The report tool speaks to the person

Both hosts get `agent_snap_retention_report`. Its text lists each namespace's fields under the quoting rule of host tool text. It ends with a line for the person, not an instruction to the agent: "This report changed nothing. A person can drop one namespace from their own terminal with `agent-snap drop <namespace> --digest <digest>`."

## The drop deletes one namespace in one transaction

`agent-snap drop <namespace> --digest <hex> [--cwd <dir>]` takes the namespace and the digest from a report. It deletes in one `git update-ref --no-deref --stdin` transaction. The transaction holds one line for each ref the report read:

- `delete <ref> <object-id>` for a ref that resolves to an object ID, whether it is direct or symbolic, and whether or not the object exists. With `--no-deref`, Git deletes a symbolic ref and leaves its target. It checks the old value against the resolved object (X3). A ref to a missing object was deleted this way in all 8 configurations (broken-ref probe).
- `verify <latest> <zero-id>` when `latest` is missing. A `latest` that a create writes after the report then refuses the drop with `reference already exists` (X1).
- `delete <latest>` with no old value when `latest` is a dangling symbolic ref. Git 2.30 has no command that checks a symbolic target, and Git refused a zero old value for one (X4). No Blink operation writes a symbolic `latest`, because a create refuses to update one, so only a hand edit can change it in this window. That is an accepted limit.

A namespace with a broken ref is refused with exit `1`, and the message names the ref. No Git command deletes a broken ref. `git update-ref --no-deref -d` failed with `reference broken`, and so did a transaction `delete` line (broken-ref probe). Round 2 sent `verify <latest> <zero-id>` for a broken `latest`, and Git refused it in every files configuration, which looked like a create. A broken item was invisible, so round 2's drop exited 0 and left it. The runbook row removes the file by hand.

The drop probe showed these on Git 2.54 and 2.56, SHA-1 and SHA-256, files and reftable:

- One stale old value refused the whole transaction, and nothing was deleted (D1).
- With every old value right, the namespace was empty afterwards. The branches and the linked worktree's namespace were unchanged (D4).
- A `latest` that names a blob was deleted (X2). A symbolic `latest` and a symbolic item were deleted, and `refs/heads/main` was unchanged (X3).

The drop has no pins, no protection rules, and no selection. A person keeps the whole namespace or drops it.

### A drop deletes at most 10,001 refs

The drop-time probe ran the real drop transaction, one `delete` with its old value for each of N items and for `latest`. It ran on a machine that other agents' tests also used, so the times are upper bounds:

| Refs | Files, loose | Files, packed | Reftable |
| --- | --- | --- | --- |
| 1,001 | 0.25 to 0.28 s | 0.19 to 0.25 s | 0.03 to 0.06 s |
| 5,001 | 1.27 to 1.65 s | 0.82 to 1.16 s | 0.04 to 0.06 s |
| 10,001 | 2.67 to 3.12 s | 1.71 to 2.09 s | 0.06 to 0.08 s |

The range covers Git 2.54 and 2.56 with SHA-1 and SHA-256. Round-1 review measured 6 to 16 s for 10,000 deletes, but its transaction also verified 10,000 pin refs, which the drop does not have.

A namespace with more than 10,001 refs is refused before the prompt, because no larger transaction was measured. The prompt says how long the drop may take on the files backend. The refusal prints the by-hand commands of the runbook row and says that they have none of the drop's checks. Those commands use `--no-deref`. Without it, the same lines deleted `refs/heads/precious` through a symbolic `latest` in all 8 configurations (by-hand probe).

### A concurrent create meets the drop at one of six seams

A create writes one new item and moves `latest` from the value it read, in one transaction. The drop deletes `latest` with the value the report read. Their transactions therefore conflict on `latest`, and Git lets exactly one of them commit.

| Seam | What happens | Outcome of the drop | Evidence |
| --- | --- | --- | --- |
| 1. A create commits between the report and the drop's first read. | The drop's digest differs from `--digest`. It refuses before the prompt. | Exit `4`, no Git transaction. | D2 |
| 2. A create commits while the prompt waits. | The recompute after the code differs. | Exit `4`, no Git transaction. | D2 |
| 3. A create commits between the recompute and the transaction. | Git refuses the drop with `is at <oid> but expected <oid>` on `latest`, or with `reference already exists` when `latest` was missing. The items are unchanged and `latest` changed. | `refused`, exit `4`. | Seam probe, 8 configurations |
| 4. A create's transaction meets the drop's transaction. | Each waits for the other's lock. Whichever takes `latest` first commits, and the other refuses. | `dropped`, or `refused` when the create came first. | Seam probe, race probe on files |
| 5. A create read `latest` before the drop committed, and writes after it. | Git refuses the create with exactly `unable to resolve reference '<latest>'`, and the create writes nothing. Landing step 7 retries it, and the retry starts a new chain on `HEAD` (D5). | `dropped`. | Seam probe, 8 configurations, D6 |
| 6. A create waits for the drop's lock past its timeout. | On files, the create fails after 1 s with the `latest.lock` message. On reftable, it fails with `cannot lock references`. | `dropped`. | Seam probe, race probe on files |

The seam probe paused the drop's Git in its prepared state with a `reference-transaction` hook, so the overlap happened on every backend. The real drop runs no hook. The race probe of round 2 overlapped real creates with a drop on the files backend only, because a reftable drop finished before the create loop started.

Seam 6 can fail one create. A 10,001-ref drop holds `latest.lock` for up to about 3 s on files, and a create waits 1 s. On reftable, a create waits only for Git's own reftable lock timeout. Landing step 7 makes a create pass `-c reftable.lockTimeout=1000`, so a create waits 1 s on both backends. With that setting, a create transaction that met a paused drop waited, then got the seam-5 message in 7 of 8 configurations, and committed first in the eighth. Step 7 also rewords the lock message, so that it says a drop or another create may hold the lock and that the create can run again. The next create after the drop succeeds.

### The create retry matches one message and is bounded

Today's `commitRefs` retries only on `but expected` and `reference already exists`. Step 7 adds one condition. A create retries when it expected an old `latest`, and the last line of Git's message is exactly `fatal: prepare: cannot lock ref '<latest>': unable to resolve reference '<latest>'`. A `latest` that holds no object ID gives the same text followed by `: reference broken` (seam probe, files), so a substring match would retry a broken `latest` until the client's timeout. A create retries at most `refDirAttempts` times, 5 today, and then fails with Git's message.

### One decision separates a decline from a drop

Round 2 declined on a signal from a handler, until step 8 ignored the signals. In Go the handler runs in its own goroutine. A signal that arrived just before the ignore could reach it after Git had started, and it then printed that nothing was deleted and exited `4` while Git deleted the namespace. The boundary probe ran that design in a stand-in at 21 offsets around the ignore, 10 runs each. 2 of 210 runs printed the decline while the stand-in's Git deleted.

Revision 3 makes the main flow of the CLI the only place that declines:

- From step 1, `signal.Notify` sends the four stop signals to a buffered channel. No goroutine exits the process or prints a result.
- While the prompt waits, the main flow waits for the terminal line and the channel together. A signal there declines.
- A signal during the reads of steps 4 and 6 waits in the channel until the prompt or step 7.
- Step 7 calls `signal.Ignore` for the four signals, and then reads the channel once without waiting. A signal found there declines with exit `4`, and Git never starts.

Go's `os/signal` sends to a channel while it holds the lock that `signal.Ignore` takes, so once `Ignore` returns, a signal is either in the channel or never delivered. That is inferred from Go's source. The boundary probe observed it. With the main-flow decision, 0 of 630 runs printed the decline after Git started, and 227 of them declined before Git started.

Step 9 starts Git with `exec.Command`, not through `gitCmd`. `gitCmd` sends `SIGTERM` to Git when its context ends. The CLI's context ends on the first stop signal, so a context-bound Git could be stopped after it took its locks. In the boundary probe, a stand-in Git that installed its own `SIGTERM` handler was stopped by a cancelled context in 170 of 210 runs. `Store.Drop` therefore starts step 9's Git with no context, no `Cancel`, and no `WaitDelay`, and waits for it to exit.

### Git runs in its own process group with files for its streams

From step 7 until the CLI exits, the CLI ignores `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT`. Git inherits the ignored signals through `exec`. A child started after `signal.Ignore` survived a `SIGINT` it sent itself (boundary probe). Once Git takes its first lock, it installs its own handlers, which replace the ignore. The round-2 signal probe sent `SIGINT` and `SIGHUP` to the CLI's process group after Git took its first lock, the way a terminal sends Ctrl-C and a hangup:

- With Git in the CLI's process group, Git 2.54 aborted with exit 128 and `couldn't close '<ref>.lock'` in 1 of 3 runs, and nothing was deleted. The other runs finished.
- With Git in its own process group, every run on both Gits finished with exit 0 and an empty namespace.

So the CLI starts Git in its own process group, as FR-16 starts its write. A terminal's Ctrl-C and hangup then reach only the CLI, which ignores them.

The CLI writes the whole transaction, `commit` included, to the record file before Git starts. Git reads the descriptor that the CLI wrote and synced, from its start, and writes standard output and standard error to files beside the record. A rename onto the record path then cannot change what Git reads. A process that can write the user's files can still change it, and such a process is outside the guarantee. The round-2 signal probe killed the CLI with `SIGKILL` after Git took its first lock:

- With Git's standard output on a pipe to the dead CLI, every run left every ref in place.
- With Git's standard output on a file, every run finished the drop.

A pipe makes the result depend on when Git next writes. With files, Git finishes the transaction it was given whatever happens to the CLI, and the next report shows the result.

### The drop refuses while a restore runs in the worktree

A restore records its safety backup as an ordinary snapshot in the worktree's namespace ([`RESTORE.md`](RESTORE.md)). A drop of that namespace deletes the backup. If the drop ran while a restore wrote, a restore that then ended `unknown` would name a backup that no longer exists.

So before the recompute of step 6, the drop takes the restore lock of the worktree that maps to the namespace, and holds it until it exits. The lock file is `agent-snap-restore.lock` in that worktree's absolute Git directory. The mapping probe showed that `git rev-parse --git-path agent-snap-restore.lock` names that file in the main worktree and in linked ones. The drop opens it read-only with `O_NOFOLLOW` and `O_NONBLOCK`, never creates it, and takes `flock(LOCK_EX|LOCK_NB)`. A held lock refuses with exit `4` and `a restore is running in the worktree`. An absent file means that no restore runs there. A restore that starts after the check still captures its backup with a create, which meets the drop at one of the seams above. A restore that starts while the drop holds the lock refuses with FR-16's `-32005`.

The restore-lock probe showed the check's three states. It saw no file and created none, then took a free lock, then reported a lock that another process held, then took it once that process ended. FR-16's lock is specified and not built, so this check is part of landing step 8 and needs no Phase 5 code.

The prompt says that the timeline can hold the safety backup of a restore, and that dropping it removes that undo.

### A killed Git leaves files that a person can remove

A `SIGKILL` sent to Git itself, or a crash, can stop it while it holds locks. The kill probes ran 10,001-ref drops and killed Git at fixed delays:

- On files, loose and packed, a kill during `prepare` deleted nothing and left up to 10,002 `.lock` files. A kill during `commit` left every ref deleted, or a part. One round-2 Git 2.56 run left 2,499 of 10,001 refs, `latest` among them. Many of these runs also left `packed-refs.lock`.
- On files, packed, one Git 2.54 kill at 0.9 s left all 10,001 refs, 10,002 `.lock` files, `packed-refs.lock`, and `packed-refs.new`. Git does not overwrite a `packed-refs.new` that exists, so while it stays, the drop and `git pack-refs` fail with `unable to create file .../packed-refs.new: File exists`. Round 2's one unexplained failure had that signature after it removed only the `.lock` files, so this is its likely cause. That is inferred, because round 2 kept no message.
- On reftable, 13 of 18 kills left `reftable/tables.list.lock`. While it stays, every ref write fails with `cannot lock references`, which names no file, so `git commit` and `git branch` fail too.

The leftover probe planted each of these files in all 8 configurations and recorded the failure it causes. It then ran the runbook's removal from a linked worktree, and in every case the drop, `git pack-refs --all`, and `git fsck` succeeded afterwards. The round-3 kill probe ran the same removal after 38 real kills on both Gits, 20 packed and 18 reftable, and every run ended with an empty namespace, `git pack-refs` exit 0, and `git fsck` exit 0.

Git writes a ref's new state into its `.lock` file and renames it into place, and it writes `packed-refs.new` before it renames that into place. A file left by a dead process therefore holds nothing that Git still needs, and removing it is safe once no Git process runs in the repository. That is inferred from Git's lock-file protocol and observed in every kill-probe recovery. The runbook row checks for live processes first, and the `unknown` outcome covers a partial drop.

Snapshot refs are shared by all worktrees, so their locks are in the common Git directory, not in `.git/worktrees/<name>`. The row finds it with `cd "$(git rev-parse --git-common-dir)" && pwd -P`, run in a worktree of the repository. Run elsewhere, the relative path that Git prints names the wrong directory, which the broken-ref probe hit while it was written.

### The outcome comes from Git's status and the after-read together

After Git exits, the CLI reads the namespace again with the reads of the report. Each `delete` line of the transaction is a listed ref, and step 10 puts it in one of three states:

- `unchanged` when the after-read has the same line for it as the digest of step 6.
- `changed` when it exists with another line, for example a `latest` that a create moved.
- `absent` when it does not exist.

The `verify` line is a guard. It is not a listed ref, and no total counts it.

| Outcome | When | CLI exit |
| --- | --- | --- |
| `dropped` | Git exited 0, and no listed ref is `unchanged`. | `0` |
| `refused` | Git did not exit 0, and no listed ref is `absent`. | `4` |
| `unknown` | Any other result, or the after-read failed. | `3` |

At seam 3, Git refused the drop. The seam probe's after-read then found the three items `unchanged` and `latest` `changed`, in all 8 configurations, which is `refused`. A listed ref that is absent after a failed transaction is never reported as dropped. Another process may have deleted it. The outcome is then `unknown`, and the result names the refs that are left.

A `delete <latest>` line with no old value cannot tell whether this drop or another process removed a dangling symbolic `latest`. Git exits 0 either way (round-2 review). The outcome then counts it as dropped. Only a hand edit reaches that case.

The exit status of the CLI follows the outcome once Git has started, also when printing the result fails afterwards. Exit `1` means a failure before Git starts, including a failure to start it, so nothing was deleted. Exit `4` means nothing was deleted. Exit `0` means the namespace was dropped. Every other status, `3` and a death by signal included, means refs may be gone. The CLI never exits `2` on purpose, because Go exits `2` on a panic.

## The record is written before anything is deleted

An identifier cannot be rebuilt from its commit. `recordTree` takes the identifier's time from the clock after `commit-tree`, and the commit stores whole seconds. So the drop writes the transaction it will run to a record file before Git starts, and Git reads its transaction from that file. The record holds the ref names, which contain the identifiers, and the object IDs. It holds no reason, path, or file content.

The record is `<user cache directory>/agent-snap/drops/<UTC time>-<namespace>.txt`, where the user cache directory is Go's `os.UserCacheDir`. The CLI creates the directory with mode 0700, checks that it is a directory that the user owns with mode 0700, creates the file exclusively with mode 0600, refuses a path inside the worktree as the scratch rules do, and syncs the file and its directory before Git starts. When any of that fails, the drop exits `1` and deletes nothing. A kill after Git starts leaves the record in place, because the CLI never removes it.

A cache directory is a place that cleaners and backup tools may clear. The record stays there because it serves one recovery soon after a drop, and a `git gc` can end that window anyway. The prompt says both.

The prompt and the result name the record. To recover, a person runs this in a worktree of the repository:

```sh
sed -n 's/^delete \(refs\/agent-snapshots\/[0-9a-f]\{16\}\/[^ ]*\) \([0-9a-f]\{40,64\}\)$/\1 \2/p' <record> |
while read -r ref oid; do git update-ref --no-deref "$ref" "$oid" "" || echo "not recovered: $ref"; done
```

Each ref comes back on its own, only when it does not exist, and only while its object exists. The by-hand probe ran this loop in all 8 configurations:

- After a create had started a new chain, the three dropped items came back beside the new one, and the new `latest` stayed. Round 2's single transaction failed as a whole there with `reference already exists`.
- With one item's object gone, that item failed with `nonexistent object`, and the other items and `latest` came back.
- With `latest` a symbolic ref to `refs/heads/precious`, or a dangling one to `refs/heads/ghost`, the loop left `latest` and every branch unchanged, and created no branch.

A `delete <latest>` line with no old value names no object, so the loop skips it. Recovery cannot bring back a dangling symbolic `latest`.

## Only the CLI drops, after a person types a random code on the terminal

The command reuses the FR-16 CLI rules. It checks that standard input, standard output, and standard error are each the controlling terminal, then opens the terminal, and refuses with exit `4` when any check fails. It has no `--yes`, and it never reads the code from standard input.

The code is six hex digits from `crypto/rand`, made for each prompt and printed only on the terminal. No report, record, tool result, or earlier run holds it. Round-1 review showed why it must not come from the digest. The report tool returns the digest, so an agent could tell the person what to type without the person reading the prompt.

Blink does not claim to stop an agent that has a shell. Such an agent can run Git itself, start its own `serve`, or give the CLI a terminal. Round-1 review measured two ways. A driver built on Python's `pty.fork()` passed every terminal check and typed the code it read from the prompt. A process that inherited the person's controlling terminal pushed input into it with `TIOCSTI` on macOS. The guarantee covers `serve`, host tools, lifecycle hooks, `SnapClient`, and any caller that gives the CLI pipes. [`RESTORE.md`](RESTORE.md) states the same limit for a restore. The random code stops blind injection, because the injector cannot know the code. It does not stop a process that reads the terminal.

The prompt shows:

- The namespace, whether it is the namespace of `cwd`, and its worktree or that no current worktree maps to it.
- The number of refs, of snapshots, and of other refs, the oldest and newest identifier with their times, and the state of `latest`.
- That the timeline can hold the safety backup of a restore, and that dropping it removes that undo.
- The record path, that the system may clear its directory, and that recovery may already be impossible after the next `git gc`, including an automatic one.
- That the drop takes up to a few seconds and the terminal must stay open.
- The code.

Ctrl-C, `SIGTERM`, `SIGHUP`, or `SIGQUIT` at the prompt declines. The drop prints that nothing was deleted and exits `4`. A wrong line, an empty line, or the end of input declines the same way.

The drop refuses on Windows until a Windows CI job runs the terminal cases, as the restore does.

## Observability stays with the existing diagnostics

Phase 6 adds no event or metric. Work item 4 asks for observability that records operation metadata and never source contents or secrets. These already meet it for the drop:

- The drop result and the record hold ref names, identifiers, object IDs, counts, Git's exit status, and Git's message. [`DIAGNOSTICS.md`](DIAGNOSTICS.md) already lists identifiers and object IDs as safe to record.
- The report holds namespaces, counts, identifiers, times, and worktree paths, which a host already has as `cwd`.
- The core writes no log file, and hosts log through their own logger under the DIAGNOSTICS rules.

Metrics for snapshot latency, deduplication, create failures, and restore outcomes are deferred. Nothing in Phase 6 carries them. The first revision proposed a `SnapOperationEvent` from `SnapClient`. It could never see a drop, because the drop is CLI-only, and nothing in Phase 6 consumes it. A restore outcome event belongs with the Phase 5 restore.

## Runbooks

[`DIAGNOSTICS.md`](DIAGNOSTICS.md) already covers stuck child processes ("Teardown failures", and "Scratch directories left after a hard kill") and corrupt item refs for lookup (the rows for symbolic, foreign, and tampered item refs, and the ref lock row).

Each landing step adds the rows for what it adds. Every command in these rows runs in a worktree of the repository. The by-hand probe, the broken-ref probe, and the leftover probe ran each command that touches a ref or a lock, the ref commands against a symbolic `latest`.

**Check that no Git process runs.** Run `lsof -a -d cwd -c git -c agent-snap` and compare the last column with the paths that `git worktree list` prints. A line whose path is one of them, or inside one, is a live process in this repository. Close OpenCode and Pi sessions in the repository first. The process-check probe listed a Git process in a linked worktree and `agent-snap serve` in the main worktree, and listed a Git process of another repository with a path outside both. `lsof` does not list itself.

| Row | Says |
| --- | --- |
| `drop needs a terminal` | Run `agent-snap drop` from your own terminal. Nothing was deleted. |
| `drop declined` | The line did not match the code, or the drop was interrupted before Git started. Nothing was deleted. |
| `namespace changed since the report` | A create or another change moved the namespace. Run the report again, and drop with its new digest. Nothing was deleted. |
| `a restore is running in the worktree` | Wait for the restore to end, then run the report again. Nothing was deleted. |
| `namespace holds a ref that Git cannot read: <ref>` | Check that no Git process runs. Then run `rm "$(cd "$(git rev-parse --git-common-dir)" && pwd -P)/<ref>"`, and run the report again. Git has no command that deletes such a ref. Nothing was deleted. |
| `namespace holds more than 10001 refs` | These commands have none of the drop's checks. Run `git for-each-ref --format='delete %(refname) %(objectname)' refs/agent-snapshots/<namespace>/ > drop-<namespace>.txt`, keep that file as the record, and then run `{ echo start; cat drop-<namespace>.txt; echo prepare; echo commit; } \| git update-ref --no-deref --stdin`. Never leave out `--no-deref`. |
| `refused` outcome | Git refused the transaction, and nothing was deleted. The result holds Git's message. A `but expected` or `reference already exists` on `latest` means that a create ran. Run the report again. |
| `unknown` outcome | Refs may be gone. Run the report to see what is left, and keep the record. The result lists the listed refs that are left. |
| Files left after a killed drop | Git reports `Unable to create '<path>.lock': File exists`, `unable to create file <path>/packed-refs.new: File exists`, or, on reftable, `cannot lock references`. Check that no Git process runs. Then, in the directory that `cd "$(git rev-parse --git-common-dir)" && pwd -P` prints, remove the `.lock` files under `refs/agent-snapshots/<namespace>/`, `packed-refs.lock`, `packed-refs.new`, and `reftable/tables.list.lock`, and run the report again. Never remove one while a Git process runs. |
| Recovery after a drop | Run the loop in [The record is written before anything is deleted](#the-record-is-written-before-anything-is-deleted) on the record. Each ref comes back while its object exists. `latest` comes back only when no create has written a new one. |
| A dangling symbolic item ref | `git fsck` names it, and no listing shows it. Delete it with `git update-ref --no-deref -d <ref>`. |
| Large repositories | Run `git pack-refs --all` for faster `list` and unchanged creates. A drop followed by `git gc --prune=now` frees a namespace's objects. Deleting items never frees anything while `latest` stays. |

Step 7 also changes the create's lock row in DIAGNOSTICS. The message says that a drop or another create may hold the lock, and that the create can run again.

## Phase 6 lands in this order

Each step is one pull request that ends in a check. Each step that adds a method, an error code, a CLI command, or a host tool updates FR-8 through FR-11, `README.md`, `docs/COMPATIBILITY.md`, and `docs/DIAGNOSTICS.md` in the same pull request. Store tests run on real repositories, SHA-1 and SHA-256, files and reftable. The read-only report lands in every layer, steps 2 to 6, before step 7 starts the drop.

Only tests in `internal/snapshot` replace the Store's seams, which are unexported package variables. No seam is exported, and no environment variable reaches one. A case that needs the Store's internals is a Store test. CLI tests re-execute the test binary as the CLI from `TestMain` with a marker argument, as the Phase 5 CLI tests do, and they replace only variables of package `main`. The release binary has no hook that a caller can reach.

1. This record, FR-17, FR-18, sections 8.7 and 8.8, and the FR-3 exception. The check is review and a link check.
2. Store. `Store.RetentionReport`, read-only. Its check is `go test ./...` with these tests:
   - `TestRetentionReportListsEveryNamespace` creates snapshots in the main worktree and a linked one, and plants a namespace that no worktree maps. It checks each namespace's counts, oldest and newest, `current`, and `worktree`.
   - `TestRetentionReportMapsEveryWorktree` runs from the main worktree's root, from its subdirectory, and from a linked worktree, in a repository under a symbolic link, with one linked worktree made with `--relative-paths` where Git supports it. Each run checks that every namespace maps to the right worktree path.
   - `TestRetentionReportMarksARemovedWorktree` removes a linked worktree and checks that its namespace has no worktree.
   - `TestRetentionReportReadsEachLatestState` plants a missing, a blob, a symbolic, a dangling symbolic, a missing-object, and, on files, a broken `latest`, and checks each state and digest line.
   - `TestRetentionReportSurvivesARefToAMissingObject` points an item at a removed object and checks that every namespace reports.
   - `TestRetentionReportCountsBrokenRefs` writes a file that holds no object ID as an item and as `latest`, on files, and checks `broken` and the digest line.
   - `TestRetentionReportCountsOtherRefs` plants `refs/agent-snapshots/README` and `refs/agent-snapshots/tmp/x`, and checks `totals.other` and that no namespace includes them.
   - `TestRetentionReportDigestChangesOnEveryRefChange` creates, deletes, and repoints one ref in turn.
   - `TestRetentionReportIgnoresMessageSize` plants an item with a 4 MiB message, and checks that the report succeeds and holds no reason text.
   - `TestRetentionReportReadsAtMost4096BytesOfAGitdirFile` plants a larger `gitdir` file and a FIFO, and checks that the namespace maps with no path and the report does not block.
   - `TestRetentionReportWritesNothing` fingerprints every ref, the index, `HEAD`, the configuration, the object store, and the file list of the Git directory, before and after.
3. Protocol. `snapshot.retention_report` with its validation table and `-32601` from an older binary. `TestServeRetentionReportRejectsUnknownKeys` and `TestServeRetentionReportPagesNamespaces` cover the request rules and `next_offset`. `TestProtocolNeverNamesDrop` parses every non-test Go file of `internal/protocol` and fails when one names `Drop`. `TestServeHasNoDropMethod` sends `snapshot.drop` and checks `-32601`. The check is `go test ./...`.
4. TypeScript. The section 8.7 types and every cross-field rule, `SnapClient.retentionReport`, `SnapLike.retentionReport`, and `SnapRetentionReportUnsupportedError` for `-32601`. Tests cover a digest that is not 64 hex digits, namespaces out of order, a `next_offset` that breaks the paging rule, an identifier that fails the FR-12 syntax, `oldest` after `newest`, and a `broken` count above `refs`. The check is `bun run test` and `bun run test:node`.
5. CLI `retention-report [--cwd <dir>] [--offset <n>]`. `retention-report prints one page` and `retention-report refuses an unknown flag`. The check is `go test ./...`.
6. Hosts. `agent_snap_retention_report` in both hosts. `retention report tool text ends with the line for the person` checks both hosts, and checks that the text never tells the agent to run the command. The OpenCode and Pi tests check that neither host registers a drop tool. The check is `bun run test`.
7. Store. The create's side of the seams. `commitRefs` retries on the one message of seam 5, at most `refDirAttempts` times, passes `-c reftable.lockTimeout=1000`, and rewords the lock message. The check is `go test ./...` with these tests:
   - `TestCreateRetriesWhenLatestWasDeleted` deletes `latest` and its item at the seam between the create's read and its transaction, and checks that the create succeeds with `HEAD` as the parent.
   - `TestCreateDoesNotRetryABrokenLatest` writes a file that holds no object ID as `latest` at the same seam, on files, and checks one failed transaction with Git's message.
   - `TestCreateRetryIsBounded` deletes `latest` before each attempt, and checks that the create fails after `refDirAttempts` transactions.
   - `TestCreateNamesADropOnALockTimeout` holds `latest.lock` on files for 2 s, and checks the new message.
   - `TestCreateWaitsForAReftableLock` holds `reftable/tables.list.lock` for 0.5 s on reftable, and checks that the create succeeds.
8. Store. `Store.Drop`, with its tests below. The check is `go test ./...`.
9. CLI `drop`, with its tests below, driven through `internal/ptytest`. Phase 5 step 9 of [`RESTORE.md`](RESTORE.md) specifies that package. Whichever of the two steps lands first adds it, and the other uses it unchanged. The check is `go test ./...`.

`Store.Drop` takes two functions from the CLI. `confirm` shows the prompt for step 5 and returns whether the person typed the code. `beforeGit` runs step 7 and returns whether Git may start. The Store starts Git only after `beforeGit` returns true. Store tests reach points in the sequence through `afterDropStep`, an unexported package variable that the Store calls after each step and at `8.synced`, after the record sync and before Git starts. The Store drop step (8) adds these tests:

- `TestDropDeletesEveryRefOfTheNamespaceOnly` fingerprints every ref, the index, `HEAD`, the configuration, the object store, and the file list of the Git directory, with a linked worktree. It checks that only the namespace's refs changed, and that the Git directory gained no file.
- `TestDropRefusesAStaleDigest` creates a snapshot before the first read and expects `ErrNamespaceChanged` with nothing deleted.
- `TestDropRefusesAConcurrentCreate` runs a create at `8.synced`, on files and reftable, and expects `refused` with every item `unchanged` and `latest` `changed`.
- `TestDropRacesCreates` repeats the race probe on files, and drives the reftable overlap through `afterDropStep`. It checks that every run ends with either nothing deleted, or no listed ref `unchanged` and every snapshot created after the drop on a chain that starts at `HEAD`.
- `TestDropEachLatestState` drops namespaces with a missing, a blob, a symbolic, a dangling symbolic, and a missing-object `latest`, and a symbolic item, and checks that the namespace is empty and `refs/heads/main` is unchanged. For the missing case, it also writes `latest` at `8.synced` and expects `refused` with `unchanged` items and the full result object.
- `TestDropDeletesARefToAMissingObject` points an item and `latest` at removed objects, and expects `dropped`.
- `TestDropRefusesABrokenRef` writes a file that holds no object ID as an item, on files, and expects exit-1 refusal naming that ref, with nothing deleted.
- `TestDropNeverTouchesOtherRefs` plants `refs/agent-snapshots/tmp/x` and checks it after a drop.
- `TestDropUnderPackedRefs` packs every ref first. `TestDropOnReftable` runs the suite on a reftable repository.
- `TestDropDoesNotClaimARefDeletedByAnotherProcess` deletes one listed ref at `8.synced`, and expects `unknown` with that ref absent and every other ref `unchanged`.
- `TestDropReportsUnknownWhenTheAfterReadFails` replaces the after-read with one that fails, and expects `unknown`.
- `TestDropDecidesTheOutcomeFromStatusAndRefs` replaces the Git runner, an unexported package variable, with runners that exit 0, exit 128, and die by `SIGKILL`, each after it deleted none, half, or all of the listed refs, or moved `latest`. It checks the outcome table and the totals.
- `TestDropSelfCheckFailsToUnknown` makes the Git runner report exit 0 while every ref stays, and checks that the result check fails and the outcome is `unknown`.
- `TestDropGitIgnoresACancelledContext` cancels the context at `8.synced` and in the Git runner after Git starts, and checks that the namespace is dropped.
- `TestDropStartsNoGitWhenBeforeGitRefuses` passes a `beforeGit` that returns false, and checks that nothing was deleted and that no record exists.
- `TestDropRunsNoReferenceTransactionHook` plants a `reference-transaction` hook that aborts, and checks that the drop succeeds.
- `TestDropStripsGitEnvironment` sets `GIT_DIR` and `GIT_INDEX_FILE` to another repository, and checks that only the resolved repository changed.
- `TestDropRefusesAHeldLock` holds a listed ref's `.lock` file and expects `refused` with Git's message.
- `TestDropRefusesWhileARestoreHoldsItsLock` holds `flock` on the mapped worktree's `agent-snap-restore.lock` from a child process, and expects the refusal with nothing deleted. `TestDropHoldsTheRestoreLock` tries the lock at `8.synced` and checks that it is held. `TestDropCreatesNoRestoreLockFile` checks that a drop in a worktree with no lock file leaves none.
- `TestDropRefusesMoreThanTheCap` plants 10,002 refs with `git fast-import`. `TestDropDeletesAtTheCap` plants 10,001 and checks that the namespace is empty.
- `TestDropFreesObjectsAfterPrune` runs `git gc --prune=now` after a drop and checks that the oldest dropped commit is gone and the other namespace's commits remain. Its second case sets the pack's time back 30 days and checks that a plain `git gc` removes the commit.
- `TestDropWritesTheRecordBeforeGit` makes the Git runner check that the record exists, is synced, and equals its standard input.
- `TestDropRecordFile` checks the modes 0700 and 0600, that two drops in one second do not share a record, and that a record directory with another mode, another owner, or a symbolic link refuses with exit-1 meaning and nothing deleted.
- `TestDropCutsTheErrorText` makes Git's standard error longer than 4096 bytes with invalid UTF-8, and checks the cut and the repair.
- `TestDropRecordRecreatesTheNamespace` runs a create between the drop and the runbook loop, and checks that every dropped item is back with its value and the new `latest` stays.
- `TestRunbookCommands` runs each runbook command that touches a ref against a namespace whose `latest` is a symbolic ref to `refs/heads/precious`, and checks that every branch is unchanged.

The CLI drop step (9) adds these tests:

- `drop refuses without a terminal`, and once for each of the three streams as a pipe while `/dev/tty` opens. Each exits `4`, deletes nothing, and writes no record.
- `drop declines a wrong code`, `drop declines an empty line`, and `drop declines the end of input`. Each exits `4` and deletes nothing.
- `drop declines a code derived from the report` types the last six hex digits of the digest, and exits `4` with nothing deleted. The test fixes the random source so that the code differs from them.
- `drop codes differ between runs` runs the prompt twice for one namespace and digest.
- `drop declines an interrupt at the prompt` sends `SIGINT` while the prompt waits, and expects exit `4` with nothing deleted.
- `drop never declines after Git started` replaces the CLI's `beforeGit` in the re-executed test binary with one that sends `SIGINT` to the process at offsets around its call to `signal.Ignore`, over many runs. Each run that exits `4` has every ref present, and each run that exits `0` has an empty namespace.
- `drop deletes after the code` checks the result JSON, exit `0`, the record, and the refs.
- `drop refuses when the namespace changed during the prompt` creates a snapshot while the prompt waits, and exits `4` with nothing deleted.
- `drop survives signals after Git took its first lock` re-executes the test binary as the CLI with a 5,000-item namespace on the files backend. It polls for a `.lock` file under the namespace, sends `SIGINT` and `SIGHUP` to the CLI's process group, and expects exit `0` with the namespace empty.
- `drop finishes when the CLI is killed` kills the CLI with `SIGKILL` after Git took its first lock, waits for Git to exit, and checks that the namespace is empty and the record is in place.
- `drop exits by the outcome` feeds each Store result to the CLI's exit mapping, and checks `0`, `3`, and `4`, also when printing fails.
- `drop through a pty driver confirms` drives the CLI with a pty that reads the prompt and types the code. It passes, and it records that a process holding a terminal is outside the guarantee.
- `drop prompt quotes the worktree path` uses a path with a line feed and an escape byte.
- `drop refuses on Windows` runs on the Windows CI job.

## Proposed safety invariants for AGENTS.md

These invariants are proposed here. The coordinator adds them to AGENTS.md when the drop lands.

- Blink never deletes a snapshot ref on its own. No create, list, host event, or timer deletes one.
- No JSONL method deletes a ref. Only the CLI `drop` command deletes snapshot refs, and only after a person types a code from `crypto/rand` that the prompt printed on the controlling terminal. The guarantee covers `serve`, host tools, lifecycle hooks, `SnapClient`, and callers that give the CLI pipes. Blink does not claim to stop an agent that has a shell or a terminal.
- A drop deletes only refs under one namespace that its report read, in one `git update-ref --no-deref --stdin` transaction that checks each readable ref's old value. It never writes a ref, the index, `HEAD`, configuration, objects, or working-tree files. It refuses while a restore holds the worktree's restore lock.
- A drop writes its record outside the repository before Git starts, and runs Git in its own process group, with no cancellable context, and with files for its standard streams.
- Only the CLI's main flow declines a drop, and it declines no later than its read of the signal channel after `signal.Ignore`.
- A drop decides its outcome from Git's exit status and a read of the refs after Git exits. It never reports a ref as dropped unless Git exited 0, and it never tells the person that nothing was deleted when a listed ref is absent.
- Every by-hand command in a runbook that changes a ref uses `--no-deref`.

## How the design meets the Phase 6 exit criteria

| Exit criterion | Met by |
| --- | --- |
| Retention cannot delete a protected checkpoint or a ref outside Blink's namespace. | Phase 6 has no protected checkpoints. Retention deletes only a whole namespace that a person confirmed, and only refs under it that the report read, with an old-value check on each. Store step 8 tests it. |
| Operators can diagnose failures without accessing developer source code. | The runbook rows, the drop result, and the record, which hold no file content. |
| Storage behavior and cleanup safety are documented and tested. | [What a snapshot costs on disk](#what-a-snapshot-costs-on-disk), its probes, and `TestDropFreesObjectsAfterPrune`. |

## Decisions

| Decision | Beats | Because |
| --- | --- | --- |
| Drop one whole namespace. | A selective cleanup, an archive move, or unchaining in Phase 6. | It is the only delete that frees disk under chaining (G2). The others free nothing or change FR-2 and FR-6. |
| The report lists every namespace in the repository. | A report of the `cwd` namespace only. | Namespaces of removed worktrees and moved repositories are the ones no worktree can reach. |
| The report reads no object type in `for-each-ref`. | `%(objecttype)` in the one read. | One ref to a missing object stopped the read for the whole repository (broken-ref probe). |
| A broken ref refuses the drop with a runbook row. | Deleting it in the transaction, or skipping it. | No Git command deletes it, and skipping it leaves a ref after a drop reported `dropped`. |
| C comes from `--absolute-git-dir` and `commondir`. | `--git-common-dir`, or `--path-format=absolute`. | The first prints a relative path in the main worktree, and the second needs Git 2.31 (mapping probe). |
| No JSONL method deletes. | A `snapshot.drop` method behind a one-shot entry. | No host needs to delete, and a method that does not exist needs no guard. |
| The digest covers every ref with its value. | A digest of items only, or a count. | Any create or change between the report and the delete refuses the drop. |
| One transaction with an old value per ref, capped at 10,001 refs. | Batches, or no cap. | A failure refuses the whole drop, and the cap is the largest measured size (drop-time probe). |
| The main flow alone declines, after `signal.Ignore` and one read of the channel. | A goroutine that declines until step 7. | The goroutine declined after Git started in 2 of 210 stand-in runs, and the main flow in 0 of 630 (boundary probe). |
| Step 9's Git has no cancellable context. | `gitCmd` with the CLI's context. | A cancelled context stopped a stand-in Git that had installed its handler in 170 of 210 runs (boundary probe). |
| Git runs in its own process group with files for its streams. | Git in the CLI's group, or pipes. | Git's own handlers aborted a drop on a group signal, and a pipe to a dead CLI changed the result (round-2 signal probe). |
| A listed ref is `unchanged`, `changed`, or `absent`. | Present or absent by the listed value. | A create at seam 3 moves `latest`, and only the three states call that `refused` (seam probe). |
| The drop takes the restore lock and refuses while it is held. | Ignoring the lock. | A drop deletes the restore's backup, the restore's only undo. |
| Recovery runs one `update-ref` per ref with an empty old value. | One transaction of `create` lines. | After a create, the transaction recreated nothing, and the loop recreated every item (by-hand probe). |
| The record is written before Git starts and is Git's input. | Printing the result only. | A kill leaves no result, and identifiers cannot be rebuilt from commits. |
| The record stays in the user cache directory. | A state directory. | It serves one recovery soon after a drop, which a `git gc` can end anyway. The prompt says the directory can be cleared. |
| The code comes from `crypto/rand` for each prompt. | The digest's last six hex digits. | The report tool returns the digest, so an agent could tell the person the code. |
| No `SnapOperationEvent` in Phase 6. | An `observe` callback in `SnapClient`. | It could never see a drop, and nothing in the phase consumes it. |

## Questions for review

- Is 10,001 refs the right cap, and is a by-hand route with no checks acceptable above it?
- Should the record carry the repository's common Git directory, so that a person knows which repository a record belongs to?
- Should the report flag a mapped worktree whose directory no longer exists, and a namespace whose oldest snapshot is older than its worktree?

## Follow-ups

- Namespace reuse. A new linked worktree created under a removed worktree's name inherits its timeline. `list` shows the old snapshots, and a restore can target them. This is a Phase 4 bug on main.
- FR-16 confirmation code. The FR-16 code is the backup ID's last six hex digits, and the plan tool prints the backup ID. The same `TIOCSTI` injection works on it. Carry a random code into the FR-16 landing step that builds the CLI prompt.
- FR-16 landing notes. A restore that meets a drop's lock refuses with `-32005`, and the lock file is empty. The restore's message then names no holder. Phase 5 step 8 can name a drop as a possible holder.
- A reused CLI create. `agent-snap create` in an unchanged tree returns the automatic snapshot it reused, with that snapshot's origin. The CLI could print `reused` and the origin.
- Unchaining and selective retention. Both wait for the operator's decision under `g-retention-scope`.
- Metrics for snapshot latency, deduplication, create failures, and restore outcomes. They wait for an operator decision.
- A create whose `latest` names a commit with no item ref fails until the tree changes (round-1 latest probe). This is a Store fix outside Phase 6.

## Evidence

The probes ran on macOS on Git 2.54.0 (Apple Git-157) and Homebrew Git 2.56.0. Round 2 used an `agent-snap` built from main at `ff57d46`, and round 3 one built from main at `cd0497d`. Each probe ran in fresh `mktemp -d` repositories with a private `HOME`, `GIT_CONFIG_NOSYSTEM=1`, and no inherited `GIT_*` variables. The scripts and logs are kept with the Phase 6 program's evidence, outside this repository, in `p6-design` for round 1, `p6-design-r2` for round 2, and `p6-design-r3` for this revision. "All 8 configurations" means SHA-1 and SHA-256, files and reftable, on each Git.

Round 2:

- The drop probe, in all 8 configurations with 43 checks each. All 344 checks passed. It covers M1, M2, D1 to D6, G1 to G3, X1 to X4, and W1 above.
- The drop-time probe ran the drop transaction at 1,001, 2,001, 5,001, and 10,001 refs in all 8 configurations, with files loose, files packed, and reftable.
- The race probe ran real creates in a loop while a 5,000-item drop ran, with the creates starting 0, 0.1, 0.3, and 0.8 seconds after the drop, in all 8 configurations. Only the files rows overlapped.
- The signal probe ran a 5,000-item drop on the files backend from a stand-in CLI that ignores the stop signals.
- The kill probe ran 10,001-ref drops on the files backend, loose and packed, on both Gits, at 18 delays.

Round 3:

- The boundary probe ran a Go stand-in of steps 7 to 9 at 21 signal offsets around `signal.Ignore`, 10 runs each, for the goroutine design, the main-flow design (three times), and a context-bound Git. It also checked that a child started after `signal.Ignore` survives `SIGINT`.
- The broken-ref probe ran a missing-object item, a missing-object `latest`, a broken `latest`, a broken item, and refs outside a namespace, in all 8 configurations, the broken cases on files only. All 160 checks passed.
- The mapping probe derived the namespaces from five working directories in all 8 configurations, and checked the worktree paths and the restore lock path. All 96 checks passed.
- The by-hand probe ran the over-cap commands, the recovery loop, and the symbolic item deletes against a symbolic `latest`, and the recovery loop after a create and with one object gone, in all 8 configurations. All 112 checks passed.
- The leftover probe planted every file that a killed drop leaves, in all 8 configurations, and ran the runbook removal from a linked worktree. All 144 checks passed.
- The kill probe killed 10,001-ref drops 38 times on both Gits, files packed and reftable, and ran the runbook removal after each.
- The seam probe paused the drop at its prepared state and ran creates at seams 3 to 6, in all 8 configurations. All 32 checks passed.
- The gc-age probe dropped a namespace and ran a plain `git gc` with a new pack and with a 30-day-old pack, in all 8 configurations.
- The process-check probe ran the runbook's `lsof` check on both Gits. The restore-lock probe ran the drop's lock check against a second process.
- Round 1 measured storage, deletion, packed refs, worktrees, moved repositories, and ref scan times. Its review measured disk growth, unchaining, the pty driver, `TIOCSTI`, and the stale `packed-refs.lock`.

Not observed. Windows, Git older than 2.54, Linux, an automatic `git gc` or `git maintenance` run, and a real restore, which is not built.

## Assumptions

Each assumption the design relies on is listed with its label. An observed fact has a probe. An inferred fact follows from code or documentation that this record read but did not run. A requirement is a rule that a later pull request builds and the named test proves.

| Assumption | Label | Proven by |
| --- | --- | --- |
| A drop followed by `git gc --prune=now` frees the namespace's objects. | Observed (drop probe G2) | `TestDropFreesObjectsAfterPrune`. |
| A plain `git gc` keeps a dropped commit only while its pack is younger than `gc.pruneExpire`. | Observed (gc-age probe) | `TestDropFreesObjectsAfterPrune`, second case. |
| Deleting items while `latest` stays frees nothing. | Observed (round-1 storage probe) | None. Phase 6 builds no such delete. |
| The derivation from `--absolute-git-dir` and `commondir` gives every worktree's namespace from any working directory. | Observed (mapping probe) | `TestRetentionReportMapsEveryWorktree`. |
| `for-each-ref` without `%(objecttype)` lists a ref to a missing object and exits 0. | Observed (broken-ref probe) | `TestRetentionReportSurvivesARefToAMissingObject`. |
| `for-each-ref` names a broken ref only in `warning: ignoring broken ref <ref>`, and no Git command deletes it. | Observed (broken-ref probe, files). Not observed on Git 2.30. | `TestRetentionReportCountsBrokenRefs`, `TestDropRefusesABrokenRef`. |
| `for-each-ref` skips a dangling symbolic ref without a warning, and `symbolic-ref -q` reads one. | Observed (X4) | `TestRetentionReportReadsEachLatestState`. |
| One stale old value refuses the whole drop transaction. | Observed (D1, D3, seam probe) | `TestDropRefusesAConcurrentCreate`. |
| `--no-deref` deletes a symbolic ref with its resolved value and leaves the target. | Observed (X3, by-hand probe) | `TestDropEachLatestState`, `TestRunbookCommands`. |
| `verify <ref> <zero-id>` refuses when the ref exists. | Observed (X1, seam probe) | `TestDropEachLatestState`, missing case. |
| A create that read `latest` before a drop fails with exactly `unable to resolve reference '<latest>'`, and a broken `latest` adds `: reference broken`. | Observed (seam probe) | `TestCreateRetriesWhenLatestWasDeleted`, `TestCreateDoesNotRetryABrokenLatest`. |
| `-c reftable.lockTimeout=1000` makes a create wait for a drop's reftable lock. | Observed on Git 2.54 and 2.56 (seam probe). Inferred that older Gits ignore the unknown key. | `TestCreateWaitsForAReftableLock`. |
| A create after a drop starts a new chain on `HEAD`. | Observed (D5) | `TestDropRacesCreates`. |
| A 10,001-ref drop takes at most about 3 s on the files backend and under 0.1 s on reftable. | Observed (drop-time probe, shared machine) | `TestDropDeletesAtTheCap`. |
| Once `signal.Ignore` returns, a stop signal is in the channel or never delivered. | Inferred from Go's `os/signal` source. Observed in 630 stand-in runs (boundary probe). | `drop never declines after Git started`. |
| A child inherits ignored signals through `exec`, and Git replaces them with its own handlers at its first lock. | Observed (boundary probe, round-2 signal probe) | `drop survives signals after Git took its first lock`. |
| A cancelled context stops a Git that `gitCmd` started. | Observed with a stand-in (boundary probe) | `TestDropGitIgnoresACancelledContext`. |
| Git with its streams on files finishes a drop after the CLI dies. | Observed (round-2 signal probe) | `drop finishes when the CLI is killed`. |
| Files that a killed Git leaves are safe to remove once no Git process runs. | Observed in 38 of 38 round-3 kill recoveries and every planted case. Inferred from Git's lock-file protocol for every kill point. | The runbook row. |
| `lsof -a -d cwd -c git -c agent-snap` lists Git and `agent-snap` processes with their working directories. | Observed on macOS (process-check probe). Inferred for Linux, where `lsof` may not be installed. | The runbook row. |
| A read-only, non-blocking `flock` on the restore lock file tells a held lock from a free one, and creates no file. | Observed (restore-lock probe) | `TestDropRefusesWhileARestoreHoldsItsLock`, `TestDropCreatesNoRestoreLockFile`. |
| The recovery loop restores each ref whose object exists, and leaves a newer `latest`. | Observed (by-hand probe) | `TestDropRecordRecreatesTheNamespace`. |
| A dropped ref has no reflog, so the record is the only list of what a drop deleted. | Observed (D4). Inferred for `core.logAllRefUpdates=always`, which would log them. | `TestDropWritesTheRecordBeforeGit`. |
| An agent's shell tool starts the CLI with pipes on all three streams. | Inferred, as in [`RESTORE.md`](RESTORE.md) | `drop refuses without a terminal`. |
| A process that holds a terminal can confirm a drop. | Observed by round-1 review (pty driver, `TIOCSTI`). Accepted limit. | `drop through a pty driver confirms`. |
| `gc --auto` packs refs, because `gc.packRefs` defaults to true. | Inferred from Git's documentation | None. |
| Windows terminal checks are not proven, so the drop refuses on Windows. | Accepted limit | `drop refuses on Windows`. |
