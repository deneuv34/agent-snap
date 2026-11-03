# Restore design

**Status:** Accepted. Phase 4, [#4](https://github.com/deneuv34/agent-snap/issues/4). Confirmed writes stay disabled until Phase 5, [#5](https://github.com/deneuv34/agent-snap/issues/5).

This record decides what a Blink restore does, and how Phase 4 prepares it without writing a user file. It answers the Phase 4 work items in [`BLINK_DEVELOPMENT_PLAN.md`](BLINK_DEVELOPMENT_PLAN.md) and shows how the design meets each acceptance requirement in section 10 of [`BLINK_SPEC.md`](BLINK_SPEC.md). The exact wire contract goes into the spec as a new functional requirement after this record is approved.

A developer who wants to undo an agent's work gets a preview that names every path a restore would write or delete, and a safety snapshot of the current state before anyone is asked to confirm. A host developer gets one read-only call that is safe to give an agent, and a write call that agent tools and lifecycle hooks cannot reach. The next maintainer gets one definition of "the current state", shared by capture, the preview, and the backup.

## A restore moves the working tree between two captured trees

Every restore is a move from a current tree to a result tree. Both are trees in the sense of FR-2, the state that `git add -A -- .` would place in an index seeded from `HEAD`.

- The current tree is the tree that `snapshot.create` would capture at that moment.
- For a full-tree restore, the result tree is the snapshot's tree.
- For a path-scoped restore, the result tree is the current tree with every covered path replaced by its version in the snapshot, or removed when the snapshot lacks it.

A restore writes working-tree files only. It never writes the real index, `HEAD`, branches, or normal refs. After a restore, `git status` shows the restored content as unstaged changes against the index. That is the same result as `git restore --worktree --source=<snapshot>`, plus the deletions described in [the state table](#each-repository-state-has-one-response).

Ignored files are outside both trees. A restore never writes, overwrites, or deletes an ignored file. A planned write that would land on one, or replace a directory that still holds one, blocks the plan. A directory whose tracked files the restore deletes keeps its ignored files, because Git removes only empty directories.

## Restore is three operations with one safety class each

| Operation | Safety class | Phase | Does |
| --- | --- | --- | --- |
| `snapshot.restore_plan` | Read-only | 4 | Computes the move and every blocker. Writes nothing to the repository. |
| `snapshot.create` with reason `restore backup` | Capture | Shipped | Records the safety backup of the current state. |
| `snapshot.restore` | Working-tree write | 5 | Applies one planned move after a human confirmed it. |

A method name says how much it can change. A validation bug in the plan method cannot become a write, because the plan method has no write path. Every snapshot method today is `snapshot.<verb>`, so the restore methods keep that prefix and put the safety class in the suffix. Only `snapshot.restore` restores.

The roadmap names the dry run "`snapshot.restore`". This record moves that name to the Phase 5 write and calls the dry run `snapshot.restore_plan`. A method called `restore` that does not restore invites a caller to send it expecting a write, or worse, to send the write expecting a preview.

A new method, not a new parameter, carries the plan. A binary older than this work answers `-32601`, and the client reports that restore is not supported by the installed binary. A new optional parameter on an existing method would instead fail as `unknown param` on every older binary.

These shapes were rejected:

- One method with a `mode` or `dry_run` field. It puts three safety classes behind one name and one validator.
- One `prepare` call that creates the backup and returns the plan. It records a checkpoint for every preview, moves `latest`, and lets any agent tool that can preview also write hidden refs. The roadmap requires the dry run to leave the repository unchanged.
- A separate backup method that records only when the captured tree equals the plan. The Phase 5 write checks the backup's tree against the plan anyway, so the extra method would repeat that check at the cost of a protocol method, a `Store` method, a CLI command, and their types.
- A stateful plan held by the server. The CLI is one process per command, and `SnapClient` restarts its transport, so server state would be lost between the plan and the confirmation.

## The plan writes nothing to the repository

`snapshot.restore_plan` captures the current tree with the same steps as `Store.Create`. The steps move into one function that both call. `Create` writes new objects into the real object store. The plan writes them into a private object directory in the scratch directory, which sits outside the repository by the FR-3 rule.

1. Create the private object directory, and write its `info/alternates` file with the real object directory, as `Store.Diff` does for its evidence repository.
2. Run `read-tree HEAD`, or `read-tree --empty` on an unborn branch, then `add -A -- .` and `write-tree`, with a scratch `GIT_INDEX_FILE` and `GIT_OBJECT_DIRECTORY` set to the private directory.

Git runs in the real Git directory, so excludes, `core.excludesFile`, attributes, filters, and line-ending rules are the repository's own. Every step sees the real objects through the alternates file, so the tree ID is the one `Create` would record. The plan hashes every working-tree file, as `Create` does, and writes only the objects the real store lacks.

The guarantee is that the plan adds, removes, or changes no file under the Git directory, and writes no ref, index, configuration, log, or working-tree file. Git refreshes the modification time of an existing object that it would otherwise write again, so a plan can touch the modification time of a loose object or a pack file. That only delays when `git gc` may prune an unreachable object.

The plan runs every program the repository configures for capture, exactly as `Create` does. That includes clean filters, long-running process filters such as Git LFS, and a `core.fsmonitor` hook. Blink cannot control what those programs write. Git LFS, for example, keeps a copy of each file it cleans under `.git/lfs`. The guarantee covers Git and Blink, and the spec will say so.

The plan then compares the current tree with the result tree in a private evidence repository, as `Store.Diff` does, whose alternates list both the real and the private object directories. The scratch directory is removed when the plan finishes, fails, times out, or is cancelled. A cancelled plan returns no result.

Reads of the real index use plumbing under `GIT_OPTIONAL_LOCKS=0`. `ls-files -z -v -s` reads entries and flags, and `diff-index --cached` finds staged paths. The plan never runs `git status`, which rewrites the index.

These shapes were rejected:

- Hiding the real objects from `add`, so that Git cannot refresh their modification times. On a repository whose `HEAD` holds CRLF content under `core.autocrlf=true`, that capture produced a different tree from `Create`, because Git reads the `HEAD` blob to decide how to convert line endings. The plan and the backup would then disagree.
- Capture inside the evidence repository. That repository has no `info/exclude` and no repository configuration, so its tree differs from what `Create` records.

## The plan lists every path the restore would change

A plan holds the target snapshot (`id`, `commit`, `tree`), the `paths` it covers (absent for a full tree), `current_tree`, `result_tree`, `applicable`, the repository blockers, the unmatched scope paths, totals over the whole plan, and one page of entries.

Each entry names one path and one action, which is `create`, `modify`, `type_change`, or `delete`. It carries the current and target `{mode, oid}`, three flags, and at most one path blocker. The flags compare three trees at that path, so they never depend on file timestamps:

- `staged` when the real index differs from `HEAD`.
- `unstaged` when the current tree differs from the real index.
- `untracked` when the current tree has the path and the real index does not.

Entries come from `diff-tree -r -z --raw --no-renames` in byte order of the path. A restore acts per path, so a rename is a `delete` and a `create`, and rename detection belongs to `snapshot.diff`.

A page holds at most 200 entries, as an FR-14 diff page does, and `next_offset` names the next page. A request for a later page must repeat `current_tree`. Each page captures the working tree again, and if it no longer captures to that tree, the plan fails as stale instead of mixing two states in one listing. A host that shows the totals and the first page pays for one capture. `applicable` is true exactly when there is no repository blocker, no unmatched scope path, and no entry with a blocker.

The plan carries no hunks. After the backup exists, `snapshot.diff <backup> <snapshot>` shows the content of a full-tree move with FR-14 citations. For a path-scoped move, that diff also shows changes outside the selected paths.

## The safety backup is a snapshot of the planned state

The backup is an ordinary `snapshot.create` with reason `restore backup` and source `restore-backup`, made after the plan and before the confirmation. The host shows the backup ID to the user before it asks for confirmation, which is the Phase 4 exit criterion. Phase 4 needs no backup code of its own. Its work is a test that the plan's `current_tree` equals the tree that `create` records right after it, in every state the plan supports.

The backup is valid for a plan when its tree equals the plan's `current_tree`. The Phase 5 write checks that again. If the working tree changed between the plan and the backup, the host plans again. Equality is proven by tree ID, never by origin, which FR-13 says is not evidence.

FR-5 deduplication stays. When `latest` already has the current tree, the backup is that snapshot, reported with `reused: true` and its own reason and origin. Section 10 asks for a "fresh" safety snapshot. This record reads that as a fresh capture whose tree is proven equal to the state before the restore, which a reused commit is. The spec pull request words the requirement that way.

The backup holds the cleaned form of every file, as every snapshot does. After a write that stopped part way or was killed, a file whose clean filter loses bytes holds neither its original bytes nor a copy Blink can return to them (Phase 5 probe obs 8 and 9). Recovery restores the cleaned form from the backup, not the original bytes.

A backup becomes `latest` and the parent of the next snapshot, so the timeline stays linear across a restore. A declined restore leaves its backup in the timeline as an ordinary checkpoint. Nothing marks a reused backup as a backup. Phase 6 decides what retention protects, and until Phase 6 Blink deletes no snapshot.

## A restore is bound to five values, not a token

The Phase 5 write takes `snapshot`, `paths`, `current_tree`, `result_tree`, and `backup`. The core re-derives each one before it writes:

- The backup resolves through FR-12 lookup, and its tree equals `current_tree`.
- A fresh capture of the working tree equals `current_tree`.
- The result tree recomputed from the snapshot and `paths` equals `result_tree`.
- The recomputed plan is applicable.

If any check fails, nothing is written. A plan computed against a different snapshot, scope, or working-tree state cannot be applied. The values are content addresses, so there is no secret, no expiry, and no server state, and a one-process CLI works the same way as `serve`.

These shapes were rejected:

- A core-minted token, such as a nonce or an HMAC. It is returned to whoever called the plan, so an agent that can preview holds it. A one-process CLI would also need key or nonce state on disk.
- A digest over the entry list. `result_tree` already pins the exact move.

## Each repository state has one response

| State | Response |
| --- | --- |
| Staged change | Kept. No restore method writes the real index, so the staged content stays staged. The entry is flagged `staged`. |
| Unstaged change | Overwritten in the working tree after the user saw the entry flagged `unstaged`. The backup holds it. |
| Untracked file, not ignored | Captured in the current tree. A restore deletes it when the snapshot lacks it within the scope, and flags the entry `untracked`. The backup holds it. |
| Ignored file | Never written, overwritten, or deleted. See `untracked_in_the_way`. |
| Ignored file that was force-added to the index | Not captured, because capture seeds from `HEAD`. Never touched, and the index keeps it. |
| Empty directory | Not captured. One in the way of a created path blocks that path. A directory that the restore's own deletions empty is removed. |
| File whose clean filter loses bytes | The backup holds the cleaned form, as every snapshot does. Restoring the backup writes what a checkout of that form writes, which can differ from the original bytes. Line-ending normalization of a file with mixed endings, `ident`, `working-tree-encoding`, and Git LFS all do this. |
| `HEAD` moved since the snapshot | Not a blocker. A restore moves between trees, not commits. |

A restore deletes untracked files on purpose. The main use is to undo what an agent did since the session started, and new files are most of that. Each deletion is a named entry in the plan, and the backup can bring it back.

Staged content is not a blocker. A restore changes only the working tree, so the index keeps every staged blob, and the backup holds every working-tree file the restore replaces, in its cleaned form.

Section 10 asks that staged and unstaged changes be "detected and preserved". This record reads "preserved" as "named in the plan, held by the backup, and never overwritten without a confirmation". A stricter reading, that a restore must leave every unstaged change in place, would block every `unstaged` entry instead, including the agent edits a restore exists to undo. Review chose the first reading.

Any blocker makes the plan inapplicable. Blink never skips a blocked path and applies the rest. The user narrows `paths` or resolves the state with Git.

Repository blockers:

| Blocker | Detected by |
| --- | --- |
| `operation_in_progress` | Any of `MERGE_HEAD`, `CHERRY_PICK_HEAD`, `REVERT_HEAD`, `REBASE_HEAD`, `BISECT_LOG`, `rebase-merge/`, `rebase-apply/`, or `sequencer/`, each resolved with `rev-parse --git-path` so a linked worktree finds its own. |
| `unmerged_index` | Any index entry above stage 0. |
| `sparse_checkout` | `core.sparseCheckout` is true. Capture keeps paths outside the sparse cone from `HEAD`, and a restore could write paths the user chose not to check out. |

Path blockers:

| Blocker | Detected by |
| --- | --- |
| `untracked_in_the_way` | Something on disk that the current tree lacks sits where the restore must write or remove. That is the path itself or a parent directory the restore must create, or anything inside a directory the restore turns into a file or a symbolic link. It covers ignored files, ignored symbolic links, and empty directories. Git's own writes remove such entries silently. A lookup that finds a path the same plan deletes under a different case is a case-only rename, not a blocker. |
| `parent_conflict` | In a path-scoped restore, an entry the snapshot has below a scope path whose parent, outside the scope, is a file, symbolic link, or submodule in the current tree. The restore could write the entry only by changing a path the user did not select, and the plan has no `result_tree`. |
| `submodule` | Mode `160000` on either side. Blink does not write submodules or nested repositories. |
| `index_flag` | The real index marks the entry assume-unchanged or skip-worktree (`ls-files -v` shows a lowercase tag or `S`). A write there would be hidden from `git status`. |
| `name_collision` | Two result paths name one file on disk, because they are equal when case is folded. It also covers a parent directory that the restore must create, when that directory exists on disk only under a name that differs by case and would survive the restore's own deletions. Git would write into that on-disk name (Phase 5 probe obs 7). A case variant at the path itself is already `untracked_in_the_way`, because the file system resolves the path to it. Checked when `core.ignorecase` is true, or when the file system resolves a tracked path with its case changed to the same file. Unicode normalization is not checked, because Go's standard library has no normalization tables. The Phase 5 capture after the write catches such a collision. |
| `unsafe_path` | A path segment that Git refuses to check out, such as `.git` or one of its file-system aliases. Only a tampered tree reaches this. |
| `unaddressable_path` | The path bytes are not valid UTF-8, so the JSON protocol cannot name the path exactly. |

## Path scope is a literal allow-list

`paths` is absent for a full-tree restore. When present, it holds 1 to 100 repository paths. Each is valid UTF-8, at most 4096 bytes, separated by `/`, with no leading or trailing `/`, no empty, `.`, or `..` segment, and no NUL byte. There are no globs and no pathspec magic.

A path covers itself and everything below it, matched by whole segment, in either tree. `src` covers `src/a.go` and not `srcx`. Duplicate or overlapping paths fail as invalid parameters. An empty list fails too, so an empty selection never means the whole tree.

A path that matches nothing in either tree goes into `unmatched` and makes the plan inapplicable, so a typo stops the restore instead of doing nothing. A path whose name is not valid UTF-8 cannot be named in `paths`. A plan that would change such a path, full-tree or scoped, blocks it as `unaddressable_path`, because the plan could not show the user its exact name.

## A human confirms outside the agent's reach

The core cannot tell a person from an agent. It enforces the binding in the previous sections, so nobody can apply a move against a state that is not backed up or that changed since the plan. The hosts enforce that a person confirmed, through structure that a lifecycle hook or an agent tool cannot reach:

- `SnapLike`, the port that lifecycle code and agent tools receive, gains only `restorePlan`. The write is not on it.
- The write takes a confirmation object that only `confirmRestore` creates, after a human answered yes. Each confirmation works once, and a cast or an object literal fails at run time. This stops code in the same process, such as a hook or a tool. It is not a secret.
- The `serve` process that hooks and tools share never dispatches `snapshot.restore`. [A one-shot process applies a restore](#a-one-shot-process-applies-a-restore) says how the confirmation module reaches the core through `SnapClient`.
- The confirmation shows the snapshot, the scope, the totals for each action and flag, and the backup ID, and says how to list every entry. A user is never asked to confirm a move they were not shown.
- In Pi, a restore runs only from the user-typed command `/agent-snap-restore`, and only when `ctx.hasUI` is true and `ctx.mode` is `tui`. In `rpc` mode the dialog goes to whatever drives the RPC, which may not be a person. Pi tools can open dialogs too, but a dialog that the agent opened invites a reflex yes, so no tool confirms a restore. The command pauses the extension's automatic snapshots while it applies.
- OpenCode 1.x and 2.x get the plan tool only, and its output ends with the CLI command to run. In 1.x, `ToolContext.ask` goes through OpenCode's permission settings, which a user can set to allow without asking, so it cannot prove that a person answered. The 2.x prompt has not been checked, and Phase 5 treats it the same way until it is.
- The CLI opens the controlling terminal, refuses when there is none, and asks the user to type the last six hex digits of the backup ID. It has no `--yes` and never reads the confirmation from standard input. The digits make the user read the backup ID. They are not a secret, because the plan prints them.

Blink does not claim to stop an agent that has a shell. Such an agent can run Git itself, start its own `serve`, or give the CLI a terminal with a tool such as `script`. The guarantee covers Blink's own tools, hooks, and shared transport, and the spec will say so. Phase 5 work item 2 asks for the confirmation to be enforced "at the protocol boundary". The core enforces the plan there. It cannot enforce that a person confirmed. Review accepted that limit, and the spec pull request rewords the work item to match.

## Phase 4 lands in this order

Each step is one pull request that ends in a check. The steps that simplify existing code come before the steps that add to it.

1. This record.
2. Spec. Add FR-15 for the plan, the error code `-32002` for a stale plan, the backup rule, and the reading of "fresh" and "preserved" above. Replace the restore items in section 10 and the restore decision in section 14 with references to FR-15. Name the plan's private object directory next to the scratch index in section 9 and in `AGENTS.md`. State that the confirmation guarantee does not cover an agent with a shell. In the roadmap, update the method name and reword Phase 5 work item 2 so that the protocol boundary enforces the plan, and the hosts enforce the confirmation.
3. Move the capture steps out of `Store.Create` into one function, with no change in behavior.
4. Make the `validateResult` default an error. Today it passes the result of an unknown method through unchecked.
5. `Store.PlanRestore`, with real-repository tests for each state and blocker above. They cover SHA-1 and SHA-256, linked and nested worktrees, renames, a `HEAD` with CRLF content under `core.autocrlf`, sparse checkout, an unborn branch, and cancellation. One test fingerprints the paths, sizes, and contents under the Git directory before and after a plan. Another records a `create` after each plan and checks that its tree equals `current_tree`.
6. Protocol method `snapshot.restore_plan`, with table-driven validation.
7. TypeScript types, result validation, a typed error for `-32601`, and cancellation tests.
8. CLI `restore-plan`, with smoke and pack-check coverage. It prints JSON, like every other command.
9. The `agent_snap_restore_plan` tool in Pi and in both OpenCode adapters. Its description and output formatting go in `packages/opencode/src/snapshots.ts`, which the 1.x and 2.x adapters share.

Phase 4 landed in these pull requests.

- #16. This record (step 1).
- #17. The spec (step 2).
- #18. The capture steps moved out of `Store.Create` (step 3).
- #19. `validateResult` rejects a result it cannot validate (step 4).
- #21. `Store.PlanRestore` (step 5).
- #22. Protocol method `snapshot.restore_plan` (step 6).
- #24. The whole-tree restore scope made explicit in `Store.PlanRestore`. This follows step 5.
- #25. TypeScript types, validation, and the typed error for `-32601` (step 7).
- #26. CLI `restore-plan` (step 8).
- #27. The `agent_snap_restore_plan` tool in Pi and both OpenCode adapters (step 9).

Phase 4 work item 5 asks for tests of failures between backup and write. Phase 4 has no write, so those tests move to Phase 5. The confirmation module, the Pi `/agent-snap-restore` command, the interactive CLI `restore`, the error code for a blocked restore, and `snapshot.restore` itself are Phase 5.

## Phase 5 writes follow these rules

Phase 5 needed its own design review before any write lands. Phase 4 fixed these rules for that review, and the Phase 5 design below keeps every one:

- Write through Git's checkout machinery, so smudge filters, line-ending conversion, symbolic links, and the executable bit come out as a checkout would write them. Writing blob bytes directly would skip smudge filters and change files on disk.
- Never write through a symbolic link, and never overwrite or remove a path the plan did not list. Git's writes remove ignored files, blocking files, and whole untracked directories silently, so the `untracked_in_the_way` check runs again immediately before the first write.
- Never write the real index.
- Check a repeated apply first. When a fresh capture already equals `result_tree`, the write does nothing and reports success. Only then compare the capture with `current_tree`. Where a filter loses bytes, a repeat can capture to neither tree and fails as stale, which writes nothing.
- Git has no working-tree lock. Git checks every planned path against the capture before its first write, and stops when one changed (probe obs 3). That narrows the race with a running agent but cannot close it.
- If a write fails, report the paths written, failed, and not yet started, and the backup ID. Recovery is a normal confirmed restore of the backup.
- After the last write, capture the working tree again. When the capture differs from `result_tree`, report the outcome as unknown. This catches what the plan cannot predict, such as a Unicode name collision or a filter that loses bytes.

## The write is a two-way read-tree

The write runs `git -c core.splitIndex=false read-tree -m -u <current_tree> <result_tree>`. `GIT_INDEX_FILE` names the scratch index that the restore's own fresh capture filled, and `GIT_OBJECT_DIRECTORY` names the private object directory. Every other `GIT_*` variable is removed, as for capture.

A write-engine probe chose this engine. It ran on Git 2.54.0 (Apple Git-157) on a case-insensitive APFS volume, in fresh repositories with a private `HOME`, `GIT_CONFIG_NOSYSTEM=1`, and no inherited `GIT_*` variables. The coordinator reran observations 3, 5, 6, and 7 and got the same rows. It observed:

- The real index, `HEAD`, refs, and configuration stayed byte-identical, and the real object count did not change, on SHA-1 and SHA-256 (obs 1).
- The result matched `git checkout` of the snapshot for a smudge filter, `text=auto eol=crlf`, an executable-bit change, a deleted executable, symbolic links to a file and to a directory, and a repointed link (obs 2).
- When one planned path changed after the capture, Git exited 128 with `Entry '<path>' not uptodate. Cannot merge.` and wrote nothing. That held when the path sorted first, when it sorted last, and when the edit kept its size and the old modification time (obs 3).
- The result tree's `.gitattributes` applied to every other path, in both directions, as `git checkout` applies it (obs 4).
- A directory that became a symbolic link to an outside directory converged with nothing written outside. A swap of the directory for that link after the capture made Git refuse, both with a tracked file inside and for a plan that only creates (obs 5).
- Directories that the deletions emptied were removed. A directory that still held an ignored file was kept (obs 11).
- Directory-to-file and file-to-directory swaps matched `git checkout`, and a capture afterwards equaled `result_tree` (extra row 1).
- After a successful write that included a file whose clean filter loses bytes, a fresh capture equaled `result_tree`, so a repeat apply finds nothing to do (extra row 2).
- A `kill -9` half way through a 10,000-file write left 5005 files at the result, 4994 at the current state, one empty file in flight, and no `.git/*.lock`. A fresh capture equaled neither tree. A plan toward the snapshot listed the rest, and the same write on a fresh capture converged (obs 9).
- The write took 0.143 s for 1,000 files and 1.344 s for 10,000 files, median of 3. The capture before it took 0.436 s and 3.699 s, so the capture dominates a restore (obs 10).

The rejected engine staged each file with `checkout-index --prefix` and renamed it into place. Without checks, it overwrote a path that changed after capture and wrote snapshot content through a swapped symbolic link (obs 3 and 5). With per-path checks and a journal, it refused and rolled back to the exact original bytes, including a file whose clean filter loses bytes (obs 3 and 8). It still applied the old `.gitattributes` rules (obs 4), needed its own loop to prune emptied directories (obs 11), left a path missing between its two renames once (obs 9), and took about 1.8 times as long (obs 10). Exact-byte rollback is the one thing it adds. The read-tree engine takes every check above from Git, and Blink adds the two checks below. The staged engine would make Blink own every check on the write path.

Git leaves two gaps, and Blink closes both in the plan that it recomputes immediately before the write:

- Git overwrote an ignored file at a created path, replaced an empty directory there, and deleted an ignored file inside a directory that became a file, each with exit code `0` (obs 6). The recomputed plan runs `untracked_in_the_way` again, so each of these blocks the restore.
- On a volume that folds case, Git wrote `dir/y` into an existing `Dir/` that held an ignored file, and a capture afterwards recorded `Dir/y` instead of `result_tree` (obs 7). The plan now blocks such a path as `name_collision`, and FR-15 says so. A case variant at the created path itself was already `untracked_in_the_way`. A probe engine that read each parent directory with exact case refused before any write (obs 7).

The window between the recomputed plan's disk checks and Git's write stays open. An ignored file or an empty directory that appears at a planned path in that window can still be overwritten. A tracked path that changes in that window makes Git refuse (obs 3), and so did a symbolic link that appeared at a created path (obs 5). Git has no working-tree lock, so Blink narrows this window and cannot close it.

A two-way read-tree cannot restore the exact original bytes of a file whose clean filter loses bytes after a partial write or a kill (obs 8 and 9). [The backup section](#the-safety-backup-is-a-snapshot-of-the-planned-state) says what recovery restores instead.

## A one-shot process applies a restore

`agent-snap serve --restore` is the only protocol entry that dispatches `snapshot.restore`. It answers `ping`, answers the first `snapshot.restore` request, and exits. It answers every other method with `-32601`. The shared `serve` keeps answering `snapshot.restore` with `-32601`, as it answers any unknown method.

`SnapClient.restore(confirmation)` starts it. The method is on `SnapClient` and not on `SnapLike`, so lifecycle code and agent tools, which receive `SnapLike`, cannot call it. It spawns the process through the same guardian and process-group rules as the shared transport, and it never sends a request to the shared transport. It sends `ping` first. A binary older than Phase 5 rejects `serve --restore` as an argument error and exits before it answers, so the client knows that nothing was sent and reports that the installed binary cannot apply restores. After the ping, the client sends one `snapshot.restore` request built from the confirmation, reads the response, and waits until the process tree is gone.

The interactive CLI `restore` calls the Store in the same process, as every other CLI command does. It never starts `serve --restore`.

These shapes were rejected:

- A long-running restore mode of `serve`. A transport that stays open after a restore is a write capability that any later code holding the client can use. A per-restore process ends with the restore, and tearing it down cannot affect a read request on the shared transport. A restore runs at the pace of a person, so one process start per restore costs nothing that matters.
- `snapshot.restore` on the shared `serve`, guarded only by the TypeScript confirmation. Any code that reaches the shared transport, such as `SnapClient.request`, could then send the write.
- Pi running the interactive CLI. Pi's terminal UI owns the terminal, so the CLI prompt would fight it for input.

The guarantee does not grow. A program with a shell can run `agent-snap serve --restore` itself, as it can already run Git. The entry exists so that `SnapClient` has a way in, and nothing on `SnapLike` or the shared transport leads to it.

## Only `confirmRestore` makes a confirmation

`packages/core/src/confirm.ts` exports `confirmRestore` and the `RestoreConfirmation` type. The package's `exports` map exposes only its index, so no other module can import the file's private state.

`confirmRestore(snap, request, ask)` takes a `SnapLike`, a request, and an `ask` function from the host. The request holds `cwd`, the snapshot ID, a scope of either `{all: true}` or `{paths}` with 1 to 100 paths, and the host name for the backup's origin. The scope type has no empty or absent form, so no input means the whole tree by accident. `confirmRestore` runs these steps in order:

1. Request the first page of the restore plan. When the plan is not applicable, return `blocked` with the plan. No backup is made.
2. Create the backup with reason `restore backup`, source `restore-backup`, and the request's host.
3. Read the backup with `get`. When its tree differs from the plan's `current_tree`, return `changed`. The person is not asked.
4. Build the prompt from the plan and the backup, then call `ask` with it. `ask` shows the prompt to a person and returns the text that person typed, or `undefined`.
5. When the typed text equals the last six hex digits of the backup ID, mint the confirmation and return `confirmed`. Otherwise return `declined`.

The confirmation is a frozen object with `cwd`, `snapshot`, `paths` (absent for the whole tree), `currentTree`, `resultTree`, and `backup`. Those are the five bound values and the directory. The module keeps every confirmation it mints in a private `WeakSet`. `SnapClient.restore` removes the confirmation from that set before it starts a process, and throws a `TypeError` before any process starts when the object is not in the set. An object literal, a cast, a copy, a confirmation that was already used, and a confirmation from another copy of the package all fail that way.

`confirmRestore` builds the prompt itself, so a host cannot show one move and confirm another. The prompt holds the snapshot ID, its creation time and reason, the scope, and the totals for each action and flag. It lists at most the first 20 entries with their action and flags. It names the backup ID and the commands that list every entry and show the content, `restore-plan` and `diff <backup> <snapshot>`. Each path and reason prints under the quoting rule that host tool text uses, so a file name cannot add a line to the prompt.

The confirmation stops a forged or replayed confirmation and keeps the write off `SnapLike`. It does not prove that a person typed the digits. Code that imports `confirmRestore` can pass an `ask` that returns the digits, because the prompt prints them. Blink's own hooks and tools never import `confirmRestore`. A test in each host package fails when a module other than Pi's restore command imports it. Code from another package in the same host process can spawn `agent-snap` itself, so it is outside the guarantee, like an agent with a shell.

These shapes were rejected:

- `ask` returning a boolean. A typed answer that must equal the backup's digits makes the person read the backup ID, in Pi as in the CLI.
- A class with a private constructor. A cast to the class type passes the TypeScript check and fails only where the code reads a private field. A set lookup fails at the one call that matters.
- A host-built prompt. Each host would need its own copy of the rule about what a person must see.

## The request binds five values, and the result names every path

The request is `{cwd, snapshot, paths?, current_tree, result_tree, backup}`. `snapshot` and `backup` are snapshot IDs. `paths` follows the FR-15 rules, and its absence means the whole tree, as in the plan. `current_tree` and `result_tree` are full object IDs, 40 or 64 lowercase hex digits.

An absent `paths` cannot widen a scoped restore. The core recomputes the result tree from `snapshot` and the scope it was sent, and refuses when that tree differs from `result_tree`. When the two trees are equal, both scopes make the same move.

The result is the restore result object in section 8.6 of the spec. It holds the outcome, the snapshot and the backup as `{id, commit, tree}`, the scope, the two bound trees, and `after_tree`, the tree that the capture after the write recorded. Its `totals` count the planned, written, failed, not started, and unplanned paths. Four lists name those paths. Each list holds at most the first 100 of its kind, in byte order, and a list is cut short exactly when its total is larger than its length. `restore-plan` toward the snapshot lists what is left, and `restore-plan` toward the backup lists what changed. With four lists of 100 paths of at most 4096 bytes each, and an error text of at most 4096 bytes, a result holds at most about 1.6 MiB before JSON escaping.

The outcome is `applied`, `already_applied`, `stopped`, or `unknown`. A refusal is not an outcome. It is an error response, and every error response from `snapshot.restore` means that this request wrote nothing. Refusal data was rejected. The request already holds every value a refused result would carry, and the protocol has no error payloads today, so every client would need a second validator. A write that started always returns a result, never an error.

`SnapClient.restore` resolves for `applied` and `already_applied`. It rejects with `SnapRestoreIncompleteError`, which carries the validated result, for `stopped` and `unknown`. A host that treats a resolved promise as success is then right, and a host that only logs the error message still reports a failure.

## The error codes say what to do next

| Condition | Code | Message |
| --- | --- | --- |
| The working tree no longer captures to `current_tree` or to `result_tree` | `-32002` | `working tree changed since the plan` |
| The recomputed plan is not applicable | `-32003` | `restore plan is not applicable` |
| The backup's tree differs from `current_tree` | `-32004` | `backup does not match current_tree` |
| The recomputed result tree differs from `result_tree` | `-32004` | `restore plan does not match result_tree` |
| Another restore holds the worktree's restore lock | `-32005` | `another restore is running in this worktree` |

`-32002` keeps its FR-15 meaning. Each new code names a different next step. A blocked plan needs the person to change the repository state or the scope. A mismatch means the caller sent values from different plans or backups, so it plans again. A busy worktree needs the person to wait. The parameter errors, `-32001` for a missing snapshot or backup, and `-32603` for a failure before the write follow FR-15. A failure part way is not an error code. It is a `stopped` result with its lists.

## A restore runs in eleven steps

Each step names where it refuses and what is on disk when it fails.

1. Validate the parameters, with no Git work. The Store's restore takes a `snapshot.Scope` and refuses an unset scope with `ErrNoScope` before any Git work.
2. Resolve the repository, then look up `snapshot` and then `backup` through FR-12. A missing one refuses with `-32001`.
3. Compare the backup's tree with `current_tree`. A difference refuses with `-32004`.
4. Take the worktree's restore lock without waiting. A held lock refuses with `-32005`.
5. Create the scratch directory and its private object directory, and capture the working tree into a scratch index, as the plan does.
6. When the capture equals `result_tree`, report `already_applied` and write nothing. This check comes before the stale check.
7. When the capture differs from `current_tree`, refuse with `-32002`.
8. Recompute the whole plan from that capture, every entry and not one page, with the request's snapshot and scope. When it is not applicable, refuse with `-32003`. When its result tree differs from `result_tree`, refuse with `-32004`. The checks that read the file system run last in this step.
9. For a scoped restore, write the result tree's tree objects into the private object directory, and check that the tree ID equals `result_tree`.
10. Ignore the stop signals, then run the write on the scratch index from step 5.
11. Capture the working tree into a new scratch index, decide the outcome, remove the scratch directory, and report.

After a failure in steps 1 to 9, this request has written no working-tree file, no index, no ref, and no object outside its scratch directory. The lock file may now exist, and capture may have refreshed object modification times, as a plan does. The scratch directory is removed, and the lock is released. After step 10 starts, every failure ends in a result. A Git failure in step 10 leaves some planned paths written, and the result lists them. A kill during step 10 leaves the split that probe obs 9 saw, no result, possibly the scratch directory, and no held lock. A failure in step 11 leaves a finished or failed write whose outcome is `unknown`, with no `after_tree` and empty lists.

Step 11 decides the outcome from the capture, never from Git's exit status alone. When the capture equals `result_tree`, the outcome is `applied`, even when Git failed. The signal probe in [Evidence](#evidence) saw Git write every file and then exit `128`, because its own signal handler had removed the scratch index lock. Otherwise the outcome is `stopped` when Git failed and `unknown` when Git succeeded. Each planned path is `written` when the capture holds its target, `not_started` when the capture holds its current side, and `failed` otherwise. Each other path where the capture differs from `result_tree` is `unplanned`.

## A started write runs to its end

A stop before step 10 is honored. In the CLI, `SIGINT` or `SIGTERM` cancels the request, as it cancels every command once the scratch-cleanup fix on branch `fix/abort-scratch-cleanup` lands. The core stops its Git children, removes its scratch directory, releases the lock, and writes nothing. The CLI says that nothing was written and exits `2`.

A stop after step 10 starts is deferred. The core ignores `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT` from just before it starts the write's Git process until the process exits. Git inherits those signals as ignored, so a terminal interrupt or the guardian's group `SIGTERM` stops neither the core nor Git. The write and the capture after it run to their end, and the response reports the outcome they observed. The ignore is process-wide, which is one more reason that only the one-shot entries call the Store's restore.

`SnapClient.restore` takes no `AbortSignal` and has no timeout. A person just confirmed the restore. The capture before the write took 3.699 seconds for 10,000 files (obs 10), so the steps before the write are short. `close()` waits for a restore whose request was sent, and tears down only after that process exits.

A hard kill cannot be deferred. `SIGKILL`, the guardian's `SIGKILL` 2 seconds after its group `SIGTERM`, a Windows forced termination, and a power loss can each stop the write part way. When `SnapClient.restore` sees the process exit without a response after it sent the request, it rejects with `SnapRestoreOutcomeUnknownError`, which names the backup ID and says to plan again. The guardian sends its group signals to a restore process only when the host process dies, because neither `SnapClient.restore` nor `close()` starts a teardown of a restore process that is still running.

For a write that has started, failing closed means that no caller is told that nothing changed when files may have changed. An error means nothing was written. A result reports what a capture saw. A missing result is `unknown`, never "cancelled".

These shapes were rejected:

- Tearing down on cancel or timeout. A torn write leaves a tree that equals neither side (obs 9), and the caller cannot tell which paths it wrote. Without the ignored signals, a `SIGTERM` 0.6 seconds into a 20,000-file write left 5807 files written (signal probe).
- A cooperative cancel through a new guardian command, honored until the write starts. It needs a guardian change and another error code. It buys a cancel of a capture that takes seconds, right after a person confirmed.
- The default 30-second timeout. A large restore, or one that runs a network smudge filter such as Git LFS, can exceed it, and the timeout would then tear the write.

## One restore at a time in a worktree

Two restores in one worktree write the same files, so the core serializes them. Step 4 takes an exclusive lock on the file that `git rev-parse --git-path agent-snap-restore.lock` names. That path is in the worktree's own Git directory, so linked worktrees restore independently. The lock is one that the operating system releases when the process ends, such as `flock` on POSIX and an exclusive-share open on Windows. A killed restore never leaves a lock that blocks the next one. The file stays after the lock is released, and it is the only file a restore writes in the Git directory. A second restore does not wait. It refuses with `-32005`.

An automatic snapshot that runs during a restore reads the working tree and writes only objects and hidden refs. It needs no lock. At worst it records a half-written state, which a later snapshot replaces. Its capture cannot affect the restore, which binds trees and never reads `latest`. A restore plan during a restore can see a half-written state. Any restore bound to that plan then fails as stale.

These shapes were rejected:

- A lock file created with exclusive create. A kill leaves it in place, and every later restore refuses until a person deletes it.
- Taking the real `index.lock`. It would make the user's own Git commands fail during a restore, and a kill would leave it behind.
- The tree binding alone. Two restores planned from one state both pass the stale check when both capture before either writes.

## The CLI asks for six hex digits on the terminal

The command is `agent-snap restore <id> [--cwd <dir>] (--all | --path <path>...)`. It requires either `--all` or at least one `--path`, and refuses both together. Unlike `restore-plan`, it never reads a missing scope as the whole tree. It has no `--yes`, no `--current-tree`, and no `--offset`.

It runs in this order:

1. Parse the arguments. Bad arguments exit `1`.
2. Open the controlling terminal, `/dev/tty` on POSIX or the console on Windows. When it cannot, it prints that a restore needs a terminal, makes no backup, and exits `2`.
3. Plan. A plan that is not applicable prints its blockers and the command that lists them, and exits `2` with no backup.
4. Create the backup, as `confirmRestore` does with source `restore-backup` and no host. When its tree differs from `current_tree`, it says that the working tree changed and exits `2`.
5. Print the prompt to the terminal. FR-16 defines its content, which is the content that `confirmRestore` shows.
6. Read one line from the terminal. Only the last six hex digits of the backup ID, in lowercase, confirm. Any other line, an empty line, or the end of input declines. A decline says that nothing was written and that the backup stays in the timeline, and exits `2`.
7. Restore in the same process with the five values.
8. Print a summary to the terminal, and print the result object as JSON to standard output.

It never reads the confirmation from standard input. The digits are not a secret, because the prompt prints them. They make the person read the backup ID.

Exit codes:

| Code | When |
| --- | --- |
| `0` | `applied` or `already_applied`. |
| `1` | An error before any write. Bad arguments, an unresolved repository, a missing snapshot, or `-32603`. |
| `2` | A refusal that wrote nothing. No terminal, blocked, stale, a mismatch, a busy worktree, a decline, or an interrupt before the write. |
| `3` | `stopped` or `unknown`. |

## Pi restores only from a typed command in its terminal UI

Pi gets `/agent-snap-restore <id> --all` and `/agent-snap-restore <id> <path> [<path>...]`. This record checked the Pi API in `packages/pi/node_modules/@earendil-works/pi-coding-agent/dist/core/extensions/types.d.ts`, version 0.84.1. `ExtensionContext` has `mode`, typed `"tui" | "rpc" | "json" | "print"`, and `hasUI`, documented as true in TUI and RPC modes. `ExtensionUIContext` has `confirm(title, message)` and `input(title, placeholder)`. A command handler receives `ExtensionCommandContext`, which adds `waitForIdle()`, and `ExtensionContext` has `isIdle()`. In `dist/core/agent-session.js`, `sendUserMessage` passes `expandPromptTemplates: false`, so another extension cannot run a slash command through it.

The handler parses its own argument string. It splits the string on runs of spaces and tabs. The first token is the snapshot ID. The rest must be exactly `--all`, or one or more paths, none of which starts with `-`. An empty string, a lone ID, `--all` with paths, and any other option print the usage and do nothing. No input reaches the whole tree without the literal `--all`. A path that holds a space or a tab cannot be named here, and the usage says to use the CLI for it.

The handler then runs in this order:

1. Refuse unless `ctx.mode` is `tui` and `ctx.hasUI` is true. In `rpc` mode, dialogs go to whatever drives the RPC, which may not be a person.
2. Refuse while `ctx.isIdle()` is false, and ask the person to wait for the agent to finish.
3. Pause the extension's automatic snapshots. The scheduler holds new triggers and runs one coalesced snapshot when the pause ends.
4. Call `confirmRestore` with host `pi`. Its `ask` shows the prompt with `ctx.ui.confirm`, and after a yes, reads the digits with `ctx.ui.input`.
5. On `confirmed`, call the restore function, then report the outcome, the backup ID, and the first lines of each list with `ctx.ui.notify`.
6. End the pause, on every path out of the handler.

Only this handler receives the restore function. The extension's dependencies gain a `restore` function, which the default factory binds to `SnapClient.restore`. Tools and hooks keep receiving `SnapLike`.

## OpenCode prints the CLI command

OpenCode keeps the plan tool and gets no restore. The plan tool's text ends with the CLI command a person runs in a terminal to apply the plan, `agent-snap restore <id> --all`, or one `--path` for each scope path. When a scope path holds any character other than ASCII letters, digits, `.`, `_`, `/`, `@`, `+`, and `-`, or starts with `-`, the text names the command without the paths and says to add one `--path` for each scope path listed above. A path pasted into a shell can then never run as a command. Pi's plan tool ends the same way, naming `/agent-snap-restore`. Nothing else changes in OpenCode.

## Phase 5 lands in this order

Each step is one pull request that ends in a check. The step that simplifies existing code comes before the steps that add to it. Each step that adds a method, an error code, a CLI command, or a host command updates FR-8 through FR-11, `README.md`, `docs/COMPATIBILITY.md`, and `docs/DIAGNOSTICS.md` in the same pull request.

1. This record and FR-16. The check is review and a link check of the relative links.
2. Store. The second rule of `name_collision`. On a case-insensitive volume, a snapshot `dir/y` with `Dir/` and an ignored file on disk blocks as `name_collision`, and so does the mirror case, each whole-tree and scoped. A case-only directory rename whose old directory the plan empties does not block. The check is `go test ./...`.
3. Store. Move the plan computation out of `PlanRestore` into one function that computes every entry from a capture, with no change in behavior. The check is that the existing plan tests pass unchanged.
4. Store. `Store.Restore`, with the adversarial suite below. The check is `go test ./...` on macOS, Linux, and Windows.
5. Protocol. `serve --restore`, `snapshot.restore` validation, and codes `-32003` to `-32005`. Table-driven tests cover every parameter rule, a shared `serve` that answers `-32601`, a `serve --restore` that answers `ping` and then exits after one restore, and an error for every other method.
6. TypeScript types for section 8.6, result validation with every cross-field rule, and `SnapClient.restore`. Tests cover ping first, an old binary that exits before the ping, a missing response after the request, no timeout, a `close()` that waits, a malformed result, and the typed errors.
7. The confirmation module. Tests cover an object literal, a cast, a copy, and a reuse, each refused before a process starts. They also cover a blocked plan that makes no backup, a changed backup that never calls `ask`, wrong digits, and a prompt with quoted paths.
8. CLI `restore`. Tests cover no terminal, which exits `2` with no backup, the scope arguments, a decline, every exit code, and a run under a pseudo-terminal in the smoke test.
9. Pi `/agent-snap-restore` and the Pi plan tool's last line. Tests cover argument parsing, `rpc` and `print` modes, a busy agent, the paused scheduler, and the import test.
10. OpenCode plan tool's last line, with the shell-safe path rule.

The Store step's adversarial suite runs on real repositories, SHA-1 and SHA-256, with these cases:

- A dirty tree with staged, unstaged, untracked, ignored, assume-unchanged, and skip-worktree entries, deletions, mode changes, and renames, which the plan holds as a deletion and a creation, including a case-only rename.
- Nested repositories, which the plan blocks as `submodule`, and linked worktrees. A restore in one linked worktree leaves every other worktree byte-identical, and the locks of two worktrees do not block each other.
- Symbolic links. A directory swapped for a link to an outside directory after the plan refuses, and the outside directory is unchanged.
- Each refusal. Stale after an edit, blocked after an ignored file appears at a created path with the file kept, a backup mismatch, a result mismatch, a busy lock, and a lock that a `kill -9` released.
- The step order. A repeat apply returns `already_applied` and writes nothing, although its capture differs from `current_tree`. A request with a backup mismatch and a busy lock refuses with `-32004`. A stale request with a busy lock refuses with `-32005`.
- Cancellation. A cancel before step 10 writes nothing. `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT`, each sent to the process group during a 10,000-file write that runs a smudge filter, end in `applied`. On Windows, a console interrupt during the write ends in `applied`.
- Failure between the backup and the write. A fault injected after each of steps 2 to 9 leaves the working tree, the real index, refs, `HEAD`, and configuration byte-identical.
- Failure during the write. A read-only directory ends in `stopped` with correct lists. A capture failure injected in step 11 ends in `unknown` with no `after_tree`. A `kill -9` part way leaves a working tree that a new plan, backup, and restore bring to `result_tree`.
- `.gitattributes` changes, smudge filters, line endings, and the executable bit match `git checkout` of the result.
- A write that a concurrent change makes differ from `result_tree` ends in `unknown`, with the change in `unplanned`.
- An unset scope fails with `ErrNoScope` before Git starts.

One test fingerprints the whole worktree, the real index, every ref, `HEAD`, the configuration, and every other worktree before and after each case. It fails when any path outside the plan, any index entry, any ref, or any other worktree changed. In the Git directory, only the lock file and object modification times may differ.

## How the design meets the spec's restore requirements

| Section 10 requirement | Met by |
| --- | --- |
| 1. Explicit selection | A snapshot ID through FR-12 lookup, and a literal path allow-list. |
| 2. Preflight | `snapshot.restore_plan` and its blockers, recomputed from a fresh capture immediately before the write (FR-16). |
| 3. Preview | Plan entries with their flags, and `snapshot.diff` between the backup and the snapshot. |
| 4. Backup | `snapshot.create` before the confirmation, checked again by tree before the write (FR-16). "Fresh" is read as above. |
| 5. Conflict preservation | Per-path `staged`, `unstaged`, and `untracked` flags, an index that is never written, and the backup. "Preserved" is read as above. |
| 6. Atomicity | Every refusal comes before the first write. A write that started reports its written, failed, and not-started paths, and recovery is a restore of the backup (FR-16). |
| 7. Confirmation | The host rules above, the one-shot entry, `confirmRestore`, and no write path for hooks or tools. |
| 8. Auditability | The restore result object (section 8.6), which names the snapshot, the backup, the scope, both bound trees, the tree after the write, and the outcome. |
| 9. Testing | The Phase 4 pull requests above, then the Phase 5 adversarial suite. |

## Review decisions

Review settled five product questions on 2026-11-01, each as this record proposed:

1. A restore deletes the untracked files that the snapshot lacks within the scope, by default. Opting in would leave an agent's new files behind after a default restore.
2. A restore overwrites an unstaged change after the user saw it flagged and the backup holds it. Blocking every `unstaged` entry would refuse the agent edits a restore exists to undo.
3. The confirmation guarantee covers Blink's own tools, hooks, and shared transport, and not an agent with a shell. Such an agent can already run Git, so no Blink mechanism would protect more.
4. OpenCode gets the plan tool only. Its permission prompt can be set to allow without asking, so it cannot prove that a person answered.
5. `snapshot.restore_plan` and `snapshot.restore` replace the roadmap's "`snapshot.restore` dry-run API".

## Evidence

Throwaway prototypes ran on Git 2.54.0 (Apple Git-157) on macOS, in fresh repositories with a private `HOME` and no inherited `GIT_*` variables. They observed:

- The capture above gave the same tree as `agent-snap create` followed by `agent-snap get` in 16 of 16 cases. Those were SHA-1 and SHA-256, each with a dirty tree, a packed repository, `core.autocrlf=true`, a `HEAD` with CRLF content under `core.autocrlf=true`, a clean filter, a sparse checkout, an unborn branch, and a linked worktree. The dirty tree held staged, unstaged, deleted, untracked, ignored, assume-unchanged, skip-worktree, symbolic-link, and mode changes.
- In every case, no path, size, or content under the Git directory changed, and the working tree was unchanged. Modification times changed on up to 5 existing objects or pack files per case, and on none for an unborn branch. The plan's tree object was absent from the real object store.
- Hiding the real objects from `add` left modification times unchanged too, but on the CRLF case it produced tree `fd52c844e625` where `create` recorded `0827cdfbd98d`.
- `ls-files`, `diff-index --cached`, and `ls-files --unmerged` under `GIT_OPTIONAL_LOCKS=0` left `.git` unchanged. A plain `git status` rewrote the index.
- `ls-files -s -t` shows an assume-unchanged entry as `H`. Only `ls-files -v` shows it as lowercase `h`.
- `checkout-index -f` from a scratch index left the real index unchanged, replaced a symbolic link to an outside directory with a real directory instead of writing through it, overwrote an ignored file, deleted an untracked directory that blocked a file, and wrote the smudged form of filtered files.
- A two-way `read-tree -m -u` from a tracked directory `D` to a file `D` deleted the ignored file `D/y.log` and exited `0`. A merge that deleted every tracked file in `D`, and wrote no file named `D`, left `D/y.log` in place.
- A two-way `read-tree -m -u` on an index filled by `read-tree` alone failed with "Entry 'a' not uptodate". On the index left by `read-tree HEAD` and `add -A`, the same merge succeeded.

Four things were assumed and not yet observed in Phase 4. They are what Git LFS and `core.fsmonitor` write during a plan, the `name_collision` probe, Windows and NTFS behavior, and Git versions older than 2.54. The Store pull request turns each observed fact that the plan relies on into a Go test. The Phase 5 write-engine probe observed two-way `read-tree -m -u` with smudge filters (obs 2).

### Phase 5 evidence

The write-engine probe is summarized in [The write is a two-way read-tree](#the-write-is-a-two-way-read-tree). Its report numbers observations 1 to 11 and two extra rows, and this record cites them by those numbers. It did not observe Windows and NTFS, Git LFS and long-running process filters, `core.fsmonitor`, Unicode normalization collisions, Git older than 2.54, racy-clean index timestamps, or case-sensitive volumes. Its timing runs slept 1.1 seconds before each capture, so no index entry was racily clean.

A signal probe for this record ran on the same Git and macOS. It built a 20,000-file repository, captured it into a scratch index, slept 1.1 seconds, and sent a signal to `read-tree -m -u` 0.6 seconds after it started. It observed:

- With default signal handling, `SIGTERM` stopped Git with exit `143`. 5807 files held the result, 14192 held the current state, and one was in flight.
- With `SIGTERM` ignored by the parent shell and so inherited by Git, every one of the 20,000 files held the result. Git then exited `128` with `index.lock write error: Bad file descriptor`. Git's own handler had removed the scratch index lock, then the inherited ignore swallowed the signal it raised again.
- With `SIGINT` ignored the same way, the result was the same as for `SIGTERM`.
- A Go 1.27.3 program that called `signal.Ignore` for `SIGINT` and `SIGTERM` after `signal.NotifyContext`, then ran `sh -c 'kill -TERM $$; kill -INT $$; echo child-survived'`, printed `child-survived`. Without `signal.Ignore`, the child died from `SIGTERM`. So a Go parent passes an ignored signal to its children.

The Pi API facts in [Pi restores only from a typed command in its terminal UI](#pi-restores-only-from-a-typed-command-in-its-terminal-ui) come from reading the installed Pi 0.84.1 type declarations and `agent-session.js`. They were not run.

### Phase 5 assumptions

Each assumption the Phase 5 design relies on is listed with its label. An observed fact has a probe row. An inferred fact follows from code or documentation that this record read but did not run. A requirement is a rule that a later pull request builds and the named test proves.

| Assumption | Label | Proven by |
| --- | --- | --- |
| A two-way read-tree on the capture's scratch index never writes the real index, `HEAD`, refs, configuration, or the real object store. | Observed (obs 1) | Store step fingerprint test. |
| Git refuses before its first write when a tracked planned path changed after the capture. | Observed (obs 3) | Store step stale-in-window test. |
| Git refuses an untracked, not ignored path that appears at a created path after the capture. | Observed for one symbolic link (obs 5). Inferred for other untracked files. | Store step test with an untracked file. |
| An ignored file or empty directory that appears between the plan's disk checks and the write can be overwritten. | Observed (obs 6). Accepted as a known window. | None. The window is documented. |
| The capture after a successful write equals `result_tree`, including a file whose clean filter loses bytes. | Observed (extra row 2) | Store step repeat-apply test. |
| An inherited ignored `SIGINT` or `SIGTERM` keeps `read-tree -m -u` running to its end. | Observed (signal probe) | Store step group-signal test. |
| An inherited ignore reaches smudge filters and process filters that Git starts. | Inferred | Store step test with a smudge filter. |
| A Go parent passes an ignored signal to the processes it starts. | Observed (signal probe) | Store step group-signal test. |
| `SIGHUP` and `SIGQUIT` behave as `SIGINT` and `SIGTERM` do. | Inferred | Store step group-signal test. |
| On Windows, ignoring the console interrupt in the core also protects the write's Git process. | Inferred, not observed | Store step on Windows CI. Until it passes, Windows release notes say that an interrupt can stop a write part way. |
| A `flock` lock on POSIX and an exclusive-share open on Windows are released when the process ends. | Inferred from the operating-system documentation | Store step busy-lock and `kill -9` test. |
| An old binary rejects `serve --restore` before it answers, and its shared `serve` answers `snapshot.restore` with `-32601`. | Observed on a binary built from `6d3629e`. It printed `command "serve" takes no arguments`, exited `1`, and wrote nothing to standard output. | Protocol step test with an older binary. |
| Pi runs a slash command only from text a person typed in TUI mode, and never from `sendUserMessage`. | Inferred from `agent-session.js` 0.84.1 | Pi step test of the mode checks. |
| No agent turn starts while the Pi command handler runs. | Inferred, not verified | Pi step test. If it fails, the handler waits with `waitForIdle()` and refuses when a turn starts. |
| A host shell tool can start the CLI with the host's controlling terminal, so the prompt can appear inside the host's terminal. | Inferred | None. A person still has to type the digits. |
| `restore-plan` toward the snapshot lists every path a stopped write left behind. | Observed (obs 9) | Store step `kill -9` test. |
