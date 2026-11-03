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
| `name_collision` | Two result paths name one file on disk, because they are equal when case is folded. It also covers a parent directory of a written path that exists on disk only under a name that differs by case, unless the result tree holds a path below that parent that `HEAD` also holds. Git would write into that on-disk name (Phase 5 probe obs 7). Every capture seeds its index from `HEAD`, so only such a `HEAD` path keeps the result tree's spelling in the capture after the write. A kept untracked path does not keep it (#32). A case variant at the path itself is already `untracked_in_the_way`, because the file system resolves the path to it. Checked when `core.ignorecase` is true, or when the file system resolves a tracked path with its case changed to the same file. Unicode normalization is not checked, because Go's standard library has no normalization tables. The capture after a Phase 5 write does not catch it either. On APFS, a path created in NFC below an existing NFD directory captured to `result_tree` (round-trip probe). |
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
- The `serve` process that hooks and tools share never dispatches `snapshot.restore`, and no `SnapClient` option or `request` call reaches the restore entry. [A one-shot process applies a restore](#a-one-shot-process-applies-a-restore) says how the confirmation module reaches the core through `SnapClient`.
- The confirmation shows the snapshot, the scope, the totals for each action and flag, and the backup ID, and says how to list every entry. It lists the entries whose current content only the working tree and the backup hold before any other entries. A large plan does not fit in a prompt, so the person sees every count and the first 20 entries of each group, and the full list is one command away.
- In Pi, a restore runs only from the user-typed command `/agent-snap-restore`, and only when `ctx.hasUI` is true and `ctx.mode` is `tui`. In `rpc` mode the dialog goes to whatever drives the RPC, which may not be a person. Pi tools can open dialogs too, but a dialog that the agent opened invites a reflex yes, so no tool confirms a restore. The command pauses the extension's automatic snapshots while it applies.
- OpenCode 1.x and 2.x get the plan tool only, and its output ends with the CLI command to run. In 1.x, `ToolContext.ask` goes through OpenCode's permission settings, which a user can set to allow without asking, so it cannot prove that a person answered. The 2.x prompt has not been checked, and Phase 5 treats it the same way until it is.
- The CLI opens the controlling terminal, refuses when there is none or when standard input, output, or error is not that terminal, and asks the user to type the last six hex digits of the backup ID. It has no `--yes` and never reads the confirmation from standard input. The digits make the user read the backup ID. They are not a secret, because the plan prints them.

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
- Check a repeated apply first. A fresh capture that equals `result_tree` means the write has nothing to do, once the core has checked that `result_tree` really is the result of the request's snapshot and scope. Only then compare the capture with `current_tree`. Where a filter loses bytes, a repeat can capture to neither tree and fails as stale, which writes nothing.
- Git has no working-tree lock. With the settings that Blink pins, and a scratch index whose modification time Blink sets to 1 second after the epoch, Git compares the content of every planned path with the capture before its first write. It stops when a planned path was changed or replaced (probe obs 3, the stat-pin probe, and the racy-index probe). It does not stop when a planned path was removed, and writes that path as planned (rule probe). That narrows the race with a running agent but cannot close it.
- If a write fails, report where each planned path stands after it, at its target, at its current side, or at neither, and the backup ID. Recovery is a normal confirmed restore of the backup.
- After the last write, capture the working tree again. The outcome is `applied` only when Git exited `0` and that capture equals `result_tree`. The capture catches a filter that loses bytes and a concurrent writer. It misses the two cases at the end of the next section.

## The write is a two-way read-tree

The write runs this command:

```sh
git -c core.splitIndex=false -c core.trustctime=true -c core.checkStat=default \
    -c core.ignoreStat=false -c core.fsmonitor= -c core.sparseCheckout=false \
    -c core.hooksPath=<scratch>/hooks \
    read-tree -m -u <current_tree> <result_tree>
```

`GIT_INDEX_FILE` names the scratch index that the restore's own fresh capture filled, and `GIT_OBJECT_DIRECTORY` names the private object directory. Every other `GIT_*` variable is removed, as for capture. `<scratch>/hooks` is an empty directory in the scratch directory. The Git commands of the captures in steps 5 and 11 pass the same seven settings. Every other Git command of a restore passes `core.hooksPath` and none of the other six. The step-8 plan must read the repository's `core.sparseCheckout` to find the `sparse_checkout` blocker. With the pin, `git config --get-regexp` listed the repository's `true` and then the pin's `false` (config-read probe). [The write runs outside the core's process group](#the-write-runs-outside-the-cores-process-group) says how the process starts.

Each setting closes one way that a repository's configuration changes the write:

- The four stat settings keep Git's check of each planned path from trusting weaker stat data. The probes below show what each one stops.
- `core.fsmonitor=` with an empty value turns the fsmonitor off. On Git 2.54, `GIT_TRACE` showed that neither the empty value nor `false` starts a program (fsmonitor probe). Before Git 2.36, Git reads any non-empty value as a hook path and runs it, so `false` would start the program `false`, and an empty value turns the hook off. Those two facts about older Git are inferred from Git's source and were not run, because this revision could not install Git 2.30.
- `core.sparseCheckout=false` stops a sparse checkout that someone turned on after step 8 from removing files outside the plan. Without the pin, a plan that deleted only the untracked `src/a2` removed `c` and `docs/b` too when the sparse pattern was `/src/`. With the pin, both stayed (sparse probe, and the review of #33).
- `core.hooksPath` pointing at an empty directory keeps the repository's hooks from running. Without it, a `post-index-change` hook ran three times during a capture and once during the write. With it, the hook never ran, and the capture recorded the same tree (hooks probe).
- `core.splitIndex=false` keeps Git from writing a shared index into the real Git directory, as for every capture today.

Git's check of a planned path also depends on the scratch index's own modification time. Git's source compares whole seconds unless Git is built with nanosecond timestamps. In that source, an entry whose modification time is not older than the index file's is racily clean, and Git compares its content. Git trusts any other entry whose stat data matches. The scratch index lives outside the repository, often on another file system with another clock. When the worktree's clock lags, or its timestamps are coarse, the index looks newer than an edit made in the capture's second, and Git trusts the stale stat data. The review of #33 measured that on a FAT32 worktree. The racy-index probe for this revision reproduced it on APFS with all seven pins, with an edit that kept the size in the capture's second:

- With the index modification time 10 seconds in the future, which stands for a lagging worktree clock, Git exited `0` and overwrote the edit, 3 of 3 runs.
- With the index modification time set to 1 second after the epoch, Git refused with `Entry 'f.txt' not uptodate. Cannot merge.` and exit `128`, and the edit stayed, 3 of 3 runs.
- With the index modification time set to 0, Git exited `0` and overwrote the edit, 3 of 3 runs. Git's source reads a zero index time as unknown, and then no entry counts as racy.

So the core sets the scratch index's modification time to exactly 1 second after the epoch right before it starts the write, after its last other use of that index. Every entry is then racy, and Git compares content for every path it is about to change. Content comparison runs the clean filters during Git's check phase, before its first write.

It also costs one more pass over the worktree. Git writes the scratch index after the write. Git's source compares the content of every racy entry that it did not just write before it writes the index, and the filter counts below match that. The cost probe ran a clean filter that starts one process per file and counted its runs. With 1,000 files, the write ran the filter 1,000 times with the index at 1 second, both when all 1,000 files were planned and when one was, and 0 times with the capture's own index time. Each run cost about 6 ms. At 20,000 files, each capture ran the filter 20,000 times and took 139 to 149 seconds. The write of all 20,000 planned files took 2.2 seconds with the capture's index time, and 131.8 seconds with the index at 1 second, where it ran the filter 20,000 times. Without a filter, the same write took 2.4 and 2.9 seconds. With one planned file of 20,000, the write took 0.05 seconds with the capture's index time. With the index at 1 second, it ran the filter 20,000 times and took 212 seconds. That row's capture took 201 seconds, so the machine ran slower during it. So the write costs about one extra capture, and a restore already runs four captures, counting the plan and the backup. The index that Git writes is never read again.

These shapes were rejected:

- Putting the scratch index on the worktree's file system. FR-3 keeps scratch state outside the repository, and a directory beside the worktree may not be writable.
- Smudging the stat data of each planned entry instead of the index time. An entry whose stat data does not match makes Git refuse without comparing content, as the Phase 4 evidence shows for an index filled by `read-tree` alone. Every restore would then refuse.
- An index that holds only the planned entries, which would skip the extra pass. It changes the engine that the write-engine probe measured, so it needs its own probe first.

Git decides whether a planned path changed after the capture from the stat data in the scratch index, and the repository's configuration decides which stat fields count. A stat-pin probe for this revision captured, waited, and then rewrote one planned file with content of the same size and its old modification time. It observed:

- With `core.trustctime=false` or `core.checkStat=minimal` in the repository and no pins, Git exited `0` and overwrote the edit. The backup was taken before the edit, so the edit was lost.
- With the four pins, Git refused with `Entry 'f3.txt' not uptodate. Cannot merge.` under the defaults, under each weakening setting, and under all three together. The edit stayed on disk.
- `core.ignoreStat=true` did not bypass the check without the pin. A capture under it recorded the edited content with and without the pin.
- In every row, the capture equaled the tree that `HEAD` holds, so the pins do not change the captured tree.

The review of #30 also ran an fsmonitor hook that reports no changes, and it did not bypass the check, because the scratch index has no fsmonitor bits. Blink pins `core.fsmonitor=` anyway, so no fsmonitor program runs during a restore and no repository setting can turn the check off.

A write-engine probe chose this engine. It ran on Git 2.54.0 (Apple Git-157) on a case-insensitive APFS volume, in fresh repositories with a private `HOME`, `GIT_CONFIG_NOSYSTEM=1`, and no inherited `GIT_*` variables. The coordinator reran observations 3, 5, 6, and 7 and got the same rows. It observed:

- The real index, `HEAD`, refs, and configuration stayed byte-identical, and the real object count did not change, on SHA-1 and SHA-256 (obs 1).
- The result matched `git checkout` of the snapshot for a smudge filter, `text=auto eol=crlf`, an executable-bit change, a deleted executable, symbolic links to a file and to a directory, and a repointed link (obs 2).
- When one planned path changed after the capture, Git exited 128 with `Entry '<path>' not uptodate. Cannot merge.` and wrote nothing. That held when the path sorted first, when it sorted last, and when the edit kept its size and the old modification time (obs 3). The probe ran with Git's default comparison settings, which the pins now enforce.
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

The window between the recomputed plan's disk checks and Git's write stays open. An ignored file or an empty directory that appears at a planned path in that window can still be overwritten. A tracked path that changes in that window makes Git refuse (obs 3), and so did a symbolic link that appeared at a created path (obs 5). A planned path that someone removes in that window does not. Git treats a missing file as up to date, so it writes the path as planned and can exit `0`. The rule probe removed a planned `modify` after the capture, and Git recreated it with the snapshot's content and exit `0`. The restore then reports `applied`. No content is lost, because the backup holds the removed file as the capture saw it. Git has no working-tree lock, so Blink narrows this window and cannot close it.

A two-way read-tree cannot restore the exact original bytes of a file whose clean filter loses bytes after a partial write or a kill (obs 8 and 9). [The backup section](#the-safety-backup-is-a-snapshot-of-the-planned-state) says what recovery restores instead.

The capture after the write misses two cases. A round-trip probe for this revision observed both:

- Unicode normalization. On APFS with `core.precomposeunicode=true`, which `git init` sets on macOS, the disk held a directory named `café` in NFD with an ignored file in it. The result tree created `café/y` in NFC. Git wrote `y` into the NFD directory and exited `0`, and the capture afterwards equaled `result_tree`. The file system treats the two spellings as one name, so no data is lost. The restore reports `applied` with the directory under a spelling that differs from the plan, and Blink does not detect it, because Go's standard library has no normalization tables.
- Line endings after `HEAD` moved. Capture seeds its index from the current `HEAD`, and Git's safe-CRLF rule reads that seed. Under `core.autocrlf=true`, a snapshot recorded `f.txt` with CRLF content, and `HEAD` later stored it with LF. A correct write put the snapshot's bytes on disk exactly, and the capture afterwards differed from both `result_tree` and `current_tree`. The restore reports `unknown`, and a repeat refuses as stale every time.

Blink documents the line-ending case and does not detect it. Detection would check out each planned path into a scratch area and clean it through the current seed, which doubles the plan's work for one configuration. The case loses no data, because the write put the snapshot's bytes on disk and the backup holds the state before it. The `unknown` text in the CLI and in Pi says that a line-ending rule causes this when `HEAD` changed since the snapshot, and that `restore-plan` toward the snapshot names the paths that differ.

## The write runs outside the core's process group

A terminal interrupt reaches every process in the terminal's foreground process group. The guardian's teardown signals reach every process in the guardian's group. Both groups hold the core. If they also held the write's Git and its filters, either signal could stop the write part way. Ignoring the signal in the core does not protect a filter, because a filter that installs its own handler replaces the ignore it inherited.

On POSIX, the core starts the write's Git in a new session with `setsid`. Git and every filter it starts then belong to a process group that no terminal and no guardian signals, and they have no controlling terminal. The core also gives the write's Git these streams and files:

- The null device as standard input and standard output.
- A file in the scratch directory as standard error. No pipe links the write's Git to the core, so a core that dies cannot end Git with `SIGPIPE`.
- The restore lock's file descriptor. The lock stays held while Git, or a filter that inherited the descriptor, still runs.

The standard-error file grows with whatever Git and the filters print, and only the filter carve-out bounds it on disk. The core bounds what it reads. After Git exits, it reads the file as a stream and holds at most 64 KiB of it in memory at a time. It checks each line against the check-phase refusals of step 11 and keeps the first 4096 bytes for the result's `error`. The file goes when the scratch directory goes.

The Git commands of the step-11 capture start in a new session too, each in its own. They need a pipe for `write-tree`'s output, so they get one, and the AGENTS.md rule against a pipe covers only the write. A core that dies during step 11 can end the capture with `SIGPIPE`, and the capture writes no working-tree file. Git started either way keeps default signal handling, so a person can still stop it on purpose.

The descriptor that Git inherits covers only a core that dies. A filter can leave a background process that keeps the descriptors it inherited, such as a credential daemon that a Git LFS smudge starts. If the core only closed its descriptor, that process would keep the lock held for minutes, and every later restore would refuse with `-32005`. So at the end of step 11 the core ends the lock with `flock(LOCK_UN)` before it closes the descriptor. `LOCK_UN` ends the lock for every holder of that open file description. A lock probe for this revision ran on macOS and on Linux 6.18. It took the lock, started a child that left a sleeping grandchild with the descriptor, and let the child exit:

- After the child exited, a second process could not take the lock on either system.
- After the holder closed its descriptor, a second process still could not take it while the grandchild lived.
- After the holder called `LOCK_UN` and then closed, a second process took the lock while the grandchild still lived.

An isolation probe for this revision ran a stand-in for the core in Go. It took the lock, caught and discarded the stop signals, and ran a 150-file write through a required smudge filter in Python that installs its own handlers for `SIGTERM`, `SIGINT`, and `SIGHUP`. It observed:

- Git in the core's process group, as the control. A `^C` written to a pseudo-terminal stopped the write with 16 files at the result, 133 at the current state, and one missing. A group `SIGTERM`, as the guardian sends it, killed a filter, and the write stopped with 23 files at the result and one missing.
- Git in its own session. Two `^C` reached only the core, which discarded both. Git exited `0` with all 150 files at the result. A group `SIGTERM` and then a group `SIGKILL` 2 seconds later killed the core. Git, now a child of `launchd`, wrote all 150 files and exited 4.6 seconds later. While it ran, `lsof` showed Git as the only holder of the lock file, and a second `flock` failed. After Git exited, the lock was free.
- A hung filter. A `SIGTERM` sent by hand to Git's process group ended Git and the hung filter at once. The core saw Git die from the signal and still held the lock.

When the core dies part way, Git and its filters finish the write. The lock stays held until the last process that inherited it exits, including a background process that a filter left. Nobody captures the result, removes the scratch directory, or answers the caller. `SnapClient.restore` reports that the outcome is unknown, that a write may still be running, and the backup ID. `restore-plan` toward the snapshot shows what is left once the write has ended. The scratch directory stays behind, as after any hard kill. Every later restore refuses with `-32005` until the lock is free, and its message names the holders that the lock file records ([One restore at a time in a worktree](#one-restore-at-a-time-in-a-worktree)). That fails closed.

These shapes were rejected:

- Ignoring the signals in the core and letting Git inherit the ignore, as the first version of this record did. A filter's own handler replaces the ignore, which the review of #30 measured. Git's own handler replaces it too. It removes the scratch index lock and raises the signal again into the ignore, which ended a complete write with exit `128` in the signal probe.
- A new process group with `setpgid` in the same session. It also escapes terminal and guardian group signals. Git would keep the controlling terminal, so a filter that opens `/dev/tty` to prompt would stop on `SIGTTIN` and hang the write. In a new session, that open fails and the filter fails. This follows from POSIX job control and was not probed.
- Holding the lock only in the core. A core that dies mid-write would release the lock while Git still writes, and a second restore could start.

Windows has no sessions. The chosen mechanism starts the write's Git with `CREATE_NEW_PROCESS_GROUP` and `CREATE_NO_WINDOW`, so that Git and its filters run on their own hidden console. The person's console `Ctrl+C`, `Ctrl+Break`, and close events then never reach them. The lock is an exclusive-share open whose handle Git inherits. All of this is inferred from the Win32 documentation and was not observed. Until a Windows CI job shows that a console `Ctrl+C`, a `Ctrl+Break`, a console close, and a forced termination of the core alone each leave the write complete and the lock held until Git exits, the core refuses every restore on Windows before any Git work.

## A one-shot process applies a restore

`agent-snap serve --restore` is the only protocol entry that dispatches `snapshot.restore`. The binary accepts it only as that exact argument list. It answers `ping`, answers the first `snapshot.restore` request, and exits. It answers every other method with `-32601`. The shared `serve` keeps answering `snapshot.restore` with `-32601`, as it answers any unknown method.

Its `ping` result adds `restore`, which is `supported` or the message of the refusal that step 1 or step 2 would give, `restore is not supported on Windows yet` or `restore needs git 2.30 or later`. A client learns from one ping, before any plan or backup, whether this binary on this machine can restore.

Its read loop differs from the shared `serve` in two ways, so that every request it reads gets an answer:

- A cancel does not end the loop. `SIGINT`, `SIGTERM`, and the end of standard input cancel the request context only. The loop keeps reading until it has answered one `snapshot.restore` or reads the end of input. A restore request that it reads after a cancel answers `restore cancelled before the write`. #31's `Serve` returns as soon as the context ends and drops a line that it has not dispatched. So a `SIGTERM` just after the client sent the request would end the core with no answer.
- The end of standard input cancels the request context. Before step 10, the restore then stops and writes nothing. From step 10 on, the context of the write cannot be cancelled, so the end of input changes nothing. A host that dies closes the guardian's standard input, and the guardian's pipe passes that end on to the core, so a host that dies before the write gets nothing written. Without this rule, the core would go on into the write after the host is gone, and nobody would read the result.

`SnapClient.restore(confirmation, options)` starts it. The method is on `SnapClient` and not on `SnapLike`, so lifecycle code and agent tools, which receive `SnapLike`, cannot call it. It spawns the process through its own private path, under the guardian in restore mode, and it never sends a request to the shared transport. It sends `ping` first. A binary older than Phase 5 rejects `serve --restore` as an argument error and exits before it answers, so the client knows that nothing was sent and reports that the installed binary cannot apply restores. After the ping, the client sends one `snapshot.restore` request built from the confirmation, reads the response, and waits until the core has exited and its group is empty.

The rest of `SnapClient` is public, so the restore entry must be unreachable from it. Two rules close the path that the review of #30 found, `new SnapClient({serveArgs: ["serve", "--restore"]}).request("snapshot.restore", ...)`:

- The constructor copies `serveArgs` into a frozen array and keeps only the copy. It throws a `TypeError` when an element starts with `restore` after its leading `-` characters are removed. Every spawn checks the copy again with the same rule. A caller that keeps the array it passed and pushes `--restore` afterwards changes nothing, because the client holds the copy. The constructor checked only the caller's array, by reference, in the first version of this rule.
- `request` throws a `TypeError` when its method is not a primitive string, and for the method `snapshot.restore`, before it starts or reuses a process. A `String` object compares unequal to `"snapshot.restore"` and serializes to the same JSON, so the type check comes first.

Neither an option nor an environment variable selects the restore entry. Code that writes its own wrapper script and passes it as `bin` can still reach it. Such code can spawn processes, so it is outside the guarantee, like an agent with a shell. These rules land before any protocol entry dispatches `snapshot.restore`, so no merged commit has a reachable write without them.

The interactive CLI `restore` calls the Store in the same process, as every other CLI command does. It never starts `serve --restore`.

These shapes were rejected:

- A long-running restore mode of `serve`. A transport that stays open after a restore is a write capability that any later code holding the client can use. A per-restore process ends with the restore, and tearing it down cannot affect a read request on the shared transport. A restore runs at the pace of a person, so one process start per restore costs nothing that matters.
- `snapshot.restore` on the shared `serve`, guarded only by the TypeScript confirmation. Any code that reaches the shared transport, such as `SnapClient.request`, could then send the write.
- Removing `serveArgs`. Tests use it to run a fixture, and a refusal of the restore argument closes the path without a breaking change.
- Pi running the interactive CLI. Pi's terminal UI owns the terminal, so the CLI prompt would fight it for input.

The guarantee does not grow. A program with a shell can run `agent-snap serve --restore` itself, as it can already run Git. The entry exists so that `SnapClient` has a way in, and nothing on `SnapLike`, the shared transport, `serveArgs`, or `request` leads to it.

## Only `confirmRestore` makes a confirmation

`packages/core/src/confirm.ts` exports `confirmRestore` and the `RestoreConfirmation` type. The package's `exports` map exposes only its index, so no other module can import the file's private state.

`confirmRestore(client, request, ask)` takes a `SnapClient`, a request, and an `ask` function from the host. The request holds `cwd`, the snapshot ID, a scope of either `{all: true}` or `{paths}` with 1 to 100 paths, and the host name for the backup's origin. The scope type has no empty or absent form, so no input means the whole tree by accident.

`confirmRestore` never calls a method through `client`. It calls the package's own `SnapClient.prototype.restorePlan`, `create`, and `get` with `.call(client, ...)`, and those three methods reach the transport only through a `#`-private method. A value that is not a `SnapClient` instance then throws a `TypeError` at its first private access, before any request. A subclass that overrides `restorePlan` or `request`, and an own property that shadows either, cannot supply the plan, the backup, or its tree. The first version of this rule used `instanceof`, which a subclass passes.

`confirmRestore` runs these steps in order:

1. Ask whether this machine can restore, through a package-internal function that `index.ts` does not export. It spawns `serve --restore` through the client's private path, sends `ping`, and closes the core's standard input. When the binary exits before it answers, or the `restore` field of the ping result is not `supported`, return `unsupported` with the reason. No plan, no backup, and no prompt happen. A Windows machine, an old Git, and a binary older than Phase 5 all stop here.
2. Request the first page of the restore plan. When the plan is not applicable, return `blocked` with the plan. No backup is made.
3. Create the backup with reason `restore backup`, source `restore-backup`, and the request's host.
4. Read the backup with `get`. When its tree differs from the plan's `current_tree`, return `changed`. The person is not asked.
5. Build the prompt from the plan and the backup, then call `ask` with it. `ask` shows the prompt to a person and returns the text that person typed, or `undefined`.
6. When the typed text equals the last six hex digits of the backup ID, mint the confirmation and return `confirmed`. Otherwise return `declined`.

The confirmation is a frozen object with `cwd`, `snapshot`, `paths` (absent for the whole tree), `currentTree`, `resultTree`, and `backup`. Those are the five bound values and the directory. The module keeps every confirmation it mints in a private `WeakSet`. `SnapClient.restore` removes the confirmation from that set before it starts a process, and throws a `TypeError` before any process starts when the object is not in the set. An object literal, a cast, a copy, a confirmation that was already used, and a confirmation from another copy of the package all fail that way.

`confirmRestore` builds the prompt itself from the validated plan that `SnapClient.restorePlan` returned, so a host cannot show one move and confirm another. It takes a `SnapClient` and not a `SnapLike`, because a `SnapLike` wrapper, even a buggy one, could return entries from one plan and trees from another. The prompt holds the snapshot ID, its creation time and reason, the scope, and the totals for each action and flag. It then lists entries in two groups, each in byte order of path and each cut at 20. The first group holds the entries flagged `unstaged` or `untracked`, whose current content exists only in the working tree and the backup. The second group holds the other entries. Each group that is cut says how many entries it left out. The plan's first page holds at most 200 entries, so when `totals.entries` is larger, `confirmRestore` reads later pages with the plan's `current_tree` until it has 20 of each group or the last page. It names the backup ID and the commands that list every entry and show the content, `restore-plan` and `diff <backup> <snapshot>`. Each path and reason prints under the quoting rule that host tool text uses, so a file name cannot add a line to the prompt.

The confirmation stops a forged or replayed confirmation and keeps the write off `SnapLike`. It does not prove that a person typed the digits. Code that imports `confirmRestore` can pass an `ask` that returns the digits, because the prompt prints them. Blink's own hooks and tools never import `confirmRestore`. A test in each host package fails when a module other than Pi's restore command imports it. Code from another package in the same host process can spawn `agent-snap` itself, so it is outside the guarantee, like an agent with a shell.

These shapes were rejected:

- `ask` returning a boolean. A typed answer that must equal the backup's digits makes the person read the backup ID, in Pi as in the CLI.
- A class with a private constructor. A cast to the class type passes the TypeScript check and fails only where the code reads a private field. A set lookup fails at the one call that matters.
- A host-built prompt. Each host would need its own copy of the rule about what a person must see.
- A `SnapLike` with a check that the shown entries match the confirmed trees. The client cannot recompute a tree from entries, so the check could only compare values from the same wrapper.
- Requiring that the client's prototype is exactly `SnapClient.prototype`. An own property named `request` on the instance would still reach the transport through the public `restorePlan`.
- A prompt that lists the first 20 entries in byte order. The deletions and the overwrites of unstaged changes can all sort after the twentieth entry, so the person would confirm them unseen.

## The request binds five values, and the result names every path

The request is `{cwd, snapshot, paths?, current_tree, result_tree, backup}`. `snapshot` and `backup` are snapshot IDs. `paths` follows the FR-15 rules, and its absence means the whole tree, as in the plan. `current_tree` and `result_tree` are full object IDs, 40 or 64 lowercase hex digits.

An absent `paths` cannot widen a scoped restore. The core recomputes the result tree from `snapshot` and the scope it was sent, and refuses when that tree differs from `result_tree`. For the whole tree, that check is that `result_tree` equals the snapshot's tree, and it runs right after the lookup. When the two trees are equal, both scopes make the same move.

The result is the restore result object in section 8.6 of the spec. It holds the outcome, the snapshot and the backup as `{id, commit, tree}`, the scope, the two bound trees, and `after_tree`, the tree that the capture after the write recorded. Its `totals` count the planned paths, and the paths in each of four lists. `at_target` names the planned paths that the capture holds at their target, `at_current` those it holds at their current side, and `at_neither` those it holds at neither. `unplanned` names the other paths where the capture differs from `result_tree`. The names say what the capture shows and nothing about what Git tried. Git keeps going after a write error, so a path whose old file it failed to remove still holds its current side. The review of #33 measured that with a read-only directory, and the rule probe reproduced it. Each list holds at most the first 100 of its kind, in byte order, and a list is cut short exactly when its total is larger than its length. `restore-plan` toward the snapshot lists what is left, and `restore-plan` toward the backup lists what changed. With four lists of 100 paths of at most 4096 bytes each, and an error text of at most 4096 bytes, a result holds at most about 1.6 MiB before JSON escaping.

`error` is present exactly when the write's Git exited with a status other than `0` or died from a signal. Its first line names the status or the signal. The first bytes of Git's standard error follow, which include what the filters printed. A filter can print file content, so hosts MUST NOT log `error`, and show it only to the person who confirmed the restore. `SnapRestoreIncompleteError.message` never includes it.

The outcome is `applied`, `already_applied`, `stopped`, or `unknown`. A refusal is not an outcome. It is an error response, and every error response from `snapshot.restore` means that this request wrote nothing. Refusal data was rejected. The request already holds every value a refused result would carry, and the protocol has no error payloads today, so every client would need a second validator. A write that Git started always returns a result, with one exception. When Git refused in its check phase before its first write, the core answers `-32002` (see [step 11](#a-restore-runs-in-eleven-steps)).

`SnapClient.restore` resolves for `applied` and `already_applied`. It rejects with `SnapRestoreIncompleteError`, which carries the validated result, for `stopped` and `unknown`. A host that treats a resolved promise as success is then right, and a host that only logs the error message still reports a failure.

After the request is sent, every failure that is not an error response rejects with `SnapRestoreOutcomeUnknownError`. Its message names the backup ID, says that a write may still be running, and says to plan again once `restore` no longer refuses with `-32005`. That covers a process exit with no response, a line that is not JSON, a malformed error object, and a result that fails validation. A plain `SnapProtocolError` never reaches the caller once files may have changed. The validator skips the order and uniqueness rules for a path in `unplanned` that holds U+FFFD, because the server repairs invalid UTF-8 after it sorts. The plan validator does the same for repaired entries.

## The error codes say what to do next

| Condition | Code | Message |
| --- | --- | --- |
| The working tree no longer captures to `current_tree` or to `result_tree` | `-32002` | `working tree changed since the plan` |
| Git refused in its check phase, before its first write | `-32002` | `working tree changed during the restore` |
| The recomputed plan is not applicable | `-32003` | `restore plan is not applicable` |
| The backup's tree differs from `current_tree` | `-32004` | `backup does not match current_tree` |
| The recomputed result tree differs from `result_tree` | `-32004` | `restore plan does not match result_tree` |
| Another restore holds the worktree's restore lock | `-32005` | `another restore is running in this worktree: <holder>` |

`<holder>` is the content of the lock file, as [One restore at a time in a worktree](#one-restore-at-a-time-in-a-worktree) defines it, or `unknown` when the file is empty or unreadable. `-32002` keeps its FR-15 meaning. Each new code names a different next step. A blocked plan needs the person to change the repository state or the scope. A mismatch means the caller sent values from different plans or backups, so it plans again. A busy worktree needs the person to wait. The parameter errors, `-32001` for a missing snapshot or backup, and `-32603` for a failure before the write follow FR-15. Three `-32603` messages are fixed, because a client or the CLI acts on them. They are `restore cancelled before the write`, `restore is not supported on Windows yet`, and `restore needs git 2.30 or later`. A failure part way is not an error code. It is a `stopped` result with its lists.

## A restore runs in eleven steps

Each step names where it refuses and what is on disk when it fails.

1. Validate the parameters, with no Git work. The Store's restore takes a `snapshot.Scope` and refuses an unset scope with `ErrNoScope` before any Git work. On Windows, refuse with `restore is not supported on Windows yet`.
2. Resolve the repository and refuse with `restore needs git 2.30 or later` when `git version` is older. Look up `snapshot` and then `backup` through FR-12. A missing one refuses with `-32001`. For the whole tree, refuse with `-32004` when `result_tree` differs from the snapshot's tree.
3. Compare the backup's tree with `current_tree`. A difference refuses with `-32004`.
4. Take the worktree's restore lock without waiting, and write the core's process ID into the lock file. A held lock refuses with `-32005`. A lock that cannot be taken for another reason, such as `ENOLCK` on a network file system, refuses with `-32603` and the operation's message.
5. Create the scratch directory and its private object directory, and capture the working tree into a scratch index, as the plan does, with the seven pinned settings. No later step writes this index before step 10.
6. When the capture equals `result_tree`, recompute the result tree from that capture with the request's snapshot and scope. When it equals `result_tree` too, report `already_applied` and write nothing. Otherwise refuse with `-32004`. This check comes before the stale check.
7. When the capture differs from `current_tree`, refuse with `-32002`.
8. Recompute the whole plan from that capture, every entry and not one page, with the request's snapshot and scope. When it is not applicable, refuse with `-32003`. When its result tree differs from `result_tree`, refuse with `-32004`. The checks that read the file system run last in this step.
9. For a scoped restore, write the result tree's tree objects into the private object directory through a second scratch index, and check that the tree ID equals `result_tree`. The index of step 5 stays byte-identical.
10. Start catching and discarding `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT`. Then check the request context, and refuse with `restore cancelled before the write` when it was cancelled. Then set the modification time of the step-5 index to 1 second after the epoch, and run the write on that index, in a new session and on a context that cannot be cancelled. Write the write's process group ID into the lock file.
11. Capture the working tree into a new scratch index, on the same context, with each Git command in a new session whose process group ID the lock file names while it runs. Decide the outcome and remove the scratch directory. End the lock with `LOCK_UN`, close its descriptor, and write the response. Then stop discarding signals.

After a failure in steps 1 to 9, or a refusal at the start of step 10, this request has written no working-tree file, no index, no ref, and no object outside its scratch directory. The lock file may now exist and hold a stale process ID, and capture may have refreshed object modification times, as a plan does. The scratch directory is removed, and the lock is ended with `LOCK_UN`. After Git starts in step 10, every failure ends in a result, except a refusal in Git's check phase. A Git failure in step 10 can leave some planned paths at their target, and the result lists where each one stands. A kill of the core during step 10 leaves Git running to its end, no result, the scratch directory, and the lock held until Git exits. A failure in step 11 leaves a finished or failed write whose outcome is `unknown`, with no `after_tree` and empty lists.

Step 11 decides the outcome from Git's exit and the capture together:

- `applied` when Git exited `0` and the capture equals `result_tree`.
- `stopped` when Git failed and the capture differs from `result_tree`.
- `unknown` when Git exited `0` and the capture differs from `result_tree`, when Git failed and the capture equals `result_tree`, or when the capture failed.

A Git failure with a capture that equals `result_tree` is not `applied`. With a filter that is not marked `required`, Git writes the raw blob when the filter dies, and the capture can clean that blob back to `result_tree`. The review of #30 measured exactly that, with a filter killed by a group `SIGTERM`. Git in its own session no longer gets that signal, and the signal probe's `index.lock write error` case no longer arises, so a correct write exits `0`.

One Git failure wrote nothing, and step 11 answers it with `-32002` `working tree changed during the restore` instead of a result. Git's check phase runs before its first write. When it refuses one path, `unpack-trees` prints one line for that path and stops, so the line names only the first changed path. The rule therefore rests on the capture, and on the refusal line only as a sign of the check phase. It needs three things together:

- Git exited with status `128` and not from a signal.
- At least one line of its standard error ends with one of the check-phase messages of `unpack-trees`. There are seven of them.
  - `Entry '<path>' not uptodate. Cannot merge.`
  - `Entry '<path>' would be overwritten by merge. Cannot merge.`
  - `Untracked working tree file '<path>' would be overwritten by merge.`
  - `Untracked working tree file '<path>' would be removed by merge.`
  - `Updating '<path>' would lose untracked files in it`
  - `Refusing to remove '<path>' since it is the current working directory.`
  - `Submodule '<path>' cannot checkout new HEAD.`

  Git's source has one more, for overlapping paths, which only `read-tree --prefix` prints, and the write never passes `--prefix`. Other lines may come before or after the refusal. With the index time at 1 second, the clean filters run in the check phase, so what they print comes first.
- No planned path that the result tree holds is at its target in the capture. A planned `delete` whose path is gone does not count, because Git treats a missing file as up to date, so a deletion by someone else looks the same as one by Git.

The rule probe ran five planned `modify` entries and one planned `delete`, with the index time at 1 second and all seven pins. With two planned paths edited after the capture, and with three, Git printed one refusal line for the first edited path and exited `128`. The capture held every other planned path at its current side and each edited path at neither side, and the rule answered `-32002`. The same held with a clean filter that prints a line to standard error for each file, whose line came before the refusal, and when someone also removed the planned `delete`. The control, a read-only directory, printed two `unable to unlink old` lines, held two paths at their target, and the rule answered `stopped`.

The person then plans again. The backup does not hold the change that made Git refuse, so the advice never says to restore the backup.

Git's source keeps these messages untranslated, because scripts read them, and translates only the `error: ` prefix. A Git 2.56.0 with German messages printed `Fehler: Entry 'f2.txt' not uptodate. Cannot merge.` in the rule probe, and the rule matched it. The Apple Git 2.54 and Homebrew Git 2.56 binaries each hold all seven templates above, and the German catalog has none of them (template probe). The catalog does translate the overlap message, which is one more reason to leave it out. So the core matches the end of each line and leaves the locale alone. Pinning `LC_MESSAGES=C` was rejected. It works only when `LC_ALL` is unset, and changing `LC_ALL` would change the character encoding that the filters run under.

Git prints these messages only from its check phase, and a refusal there ends `unpack-trees` before its first write. That is read from Git's source, and every refusal in obs 3 and in the rule probe wrote nothing. So the rule can answer `-32002` after a write only when a filter printed a line that ends like a refusal. Even then, the third condition answers `stopped` for any write that left a planned file or link at its target. A filter is the repository's own program and can already write anywhere, so that case sits inside the filter carve-out. In the other direction, a concurrent change that happens to leave a planned path at its target gives `stopped` although nothing was written. That is safe and only less precise.

Each planned path is `at_target` when the capture holds its target, `at_current` when the capture holds its current side, and `at_neither` otherwise. A `delete`'s target is the absent path. Each other path where the capture differs from `result_tree` is `unplanned`.

## A started write runs to its end

The core honors a cancel up to its last check before the write. It defers every stop signal after that check:

1. Before step 10, a cancel works as it does for every request once #31 lands. `SIGINT` or `SIGTERM` cancels the request context, and in `serve --restore` so does the end of standard input. The core stops its Git children, removes its scratch directory, ends the lock, writes nothing, and answers `restore cancelled before the write`.
2. At the start of step 10, the core starts catching the four stop signals and discards each one. Then it checks the request context. A cancel that already landed refuses as in rule 1, with nothing written.
3. Steps 10 and 11 run on a context that cannot be cancelled, made with `context.WithoutCancel`. No Git command of those steps has a cancel function, so the core never sends `SIGTERM` or `SIGKILL` to it. The core keeps discarding stop signals until its response is written.

Rule 3 closes a race in the Git helpers that #31 adds. They send Git `SIGTERM` when the request context ends, and kill it after a 500 ms `WaitDelay`. A signal that reached the core just before rule 2, and cancelled the context just after it, would otherwise kill Git mid-write. A signal that lands that late counts as one that arrived during the write. The write runs, and the caller receives its real outcome.

The core catches the signals and does not ignore them. A Go program passes an ignored signal on to the processes it starts (signal probe), and the write's Git needs default handling so that a person can stop it on purpose. In the isolation probe, the core caught the signals, and Git died from a `SIGTERM` sent by hand to its group.

`SnapClient.restore` honors an `AbortSignal` and the client's timeout, as every client method does. A per-call `timeoutMs` can replace the client's default of 30 seconds. The request counts as sent from the moment before the client calls `write()` with the `snapshot.restore` line. Before that moment, the client checks the signal and the timer. After it, the client never treats the request as unsent, even when the write callback reports an error. An abort and a timeout act the same way:

| When it comes | What the client does | What the caller receives |
| --- | --- | --- |
| Before the request is sent | Sends the guardian `shutdown` and closes the core's standard input. The guardian tears the group down as it does for any request, with `SIGTERM` and then `SIGKILL` after `KILL_GRACE_MS`. The core holds no restore request, so nothing can be written. | `AbortError`, or the timeout error. The confirmation is spent. |
| After the request is sent, before step 10 | Sends the guardian `cancel`, which sends one `SIGTERM` to the core's group, keeps standard input open, and waits for the response. The core answers `restore cancelled before the write`, also when it reads the line after the signal. | `AbortError`, or the timeout error, saying that nothing was written. |
| During steps 10 and 11 | Sends the same one `SIGTERM`, which the core discards, and waits for the response. | The real outcome. The promise resolves for `applied` and `already_applied`, and rejects with `SnapRestoreIncompleteError` for `stopped` and `unknown`. |

The client cannot tell the second row from the third when it sends the signal. The response tells it. An error response means nothing was written, and a result means the write ran. The wait after the signal has no time limit, because the only way to end it early is to tear the write.

No client path tears down a running write:

- `SnapClient.restore` starts the guardian in restore mode. In that mode, the guardian takes two commands. `shutdown` tears the group down as in normal mode, and the client sends it only before the request is sent. `cancel` sends one group `SIGTERM`, and the client sends it only after. The guardian does nothing when its standard input ends or its control channel closes, and it ignores errors on its own standard output. It exits after the core exits and its group is empty.
- `SnapClient.restore` keeps the core's standard input open until the response arrives or the core exits.
- A `child-exit` report from a restore process starts no teardown. The client only observes the group until it is empty. When that observation fails after a result arrived, for example because a filter left a process in the group, the client logs it and still settles with the result.
- `close()` starts no teardown of a restore whose request was sent. It waits up to `GUARDIAN_WAIT_MS` for that restore to settle. If the restore is still running then, `close()` rejects with an error that names the write's process group from the lock file and says that the write was not stopped. AGENTS.md lets `close()` resolve only when no descendant may be alive, and this keeps that rule without tearing the write. The restore's own promise stays pending until the response or the core's exit.

When the host exits during a restore, the guardian's standard input ends and its control channel closes, and in restore mode it signals nothing. The guardian's pipe ends the core's standard input. Before step 10, the core then cancels and writes nothing. From step 10 on, it finishes the write and the capture, and its response has no reader.

A filter that never ends keeps the write from ending, and neither a cancel nor a timeout ends it. Blink does not kill it, because a kill tears the write. The person decides, and the lock file tells them what to stop. The Store's restore takes a function that it calls each time it discards a stop signal from step 10 on, with the step and the process group ID that the lock file names. `serve --restore` passes a function that does nothing. The CLI prints from it:

- During the write, that the interrupt does not stop the write, and the command that does, `kill -TERM -<pgid>`. That command ends Git and its filters and leaves the working tree part way. The core then reports `stopped` with its lists.
- During the step-11 capture, that the write has ended and Blink is checking the result. `kill -TERM -<pgid>` with the group it prints ends that check. It writes nothing to the working tree, and the outcome is then `unknown`.

In Pi, the person stops the same group from another terminal. `cat "$(git rev-parse --git-path agent-snap-restore.lock)"` in the worktree prints the group, and `kill -TERM -<pgid>` stops it. The lock file belongs to one worktree, so the recipe never names another worktree's restore, as a `pgrep` for `read-tree` could. The CLI step adds both recipes to `docs/DIAGNOSTICS.md`.

A hard kill of Git, such as a `kill -9` of its group or a power loss, can still stop the write part way, and so can the person's stop above. A kill of the core alone no longer stops the write. When `SnapClient.restore` sees the core exit without a response after it sent the request, it rejects with `SnapRestoreOutcomeUnknownError`.

For a write that has started, failing closed means that no caller is told that nothing changed when files may have changed. An error means nothing was written. A result reports what Git's exit and a capture saw. A missing or unreadable result is `unknown`, never "cancelled".

These shapes were rejected:

- Tearing down on cancel or timeout. A torn write leaves a tree that equals neither side (obs 9), and the caller cannot tell which paths it wrote. Without protection, a `SIGTERM` 0.6 seconds into a 20,000-file write left 5807 files written (signal probe).
- No `AbortSignal` and no timeout, as the first version of this record chose. A hung filter would hang the Pi command with no way out. A cancel before the write is safe to honor, so the exception to the AGENTS.md rule bought nothing.
- A timeout that ends the wait during the write. The caller would be told something before the core knows what the write did.

## One restore at a time in a worktree

Two restores in one worktree write the same files, so the core serializes them. Step 4 takes an exclusive lock on the file that `git rev-parse --git-path agent-snap-restore.lock` names. That path is in the worktree's own Git directory, so linked worktrees restore independently. The lock is one that the operating system releases when the last holder of its file descriptor ends, such as `flock` on POSIX and an exclusive-share open on Windows. The core opens it close-on-exec and passes it only to the write's Git, so a core that dies mid-write leaves the lock held until Git exits (isolation probe). At the end of step 11, and after a refusal before the write, the core ends the lock with `LOCK_UN`, so a background process that a filter left does not keep it (lock probe). A killed restore never leaves a lock that blocks the next one after its processes end. The file stays after the lock is released, and it is the only file Blink writes in the Git directory during a restore. A second restore does not wait. It refuses with `-32005`.

The lock file says who holds the lock. The core rewrites it in place while it holds the lock, with one line of at most 200 bytes. Step 4 writes `core <pid>`. Step 10 adds `write <pgid>` once the write's Git starts. Step 11 replaces that with `check <pgid>` for each capture command. The `-32005` message carries the line. A reader can catch the file part way through a rewrite, and the line is a hint for a person, never an input to a decision. A core that died leaves the line it last wrote, so a person can see a dead core process and a live group.

An automatic snapshot that runs during a restore reads the working tree and writes only objects and hidden refs. It needs no lock. At worst it records a half-written state, which a later snapshot replaces. Its capture cannot affect the restore, which binds trees and never reads `latest`. A restore plan during a restore can see a half-written state. Any restore bound to that plan then fails as stale.

These shapes were rejected:

- A lock file created with exclusive create. A kill leaves it in place, and every later restore refuses until a person deletes it.
- Taking the real `index.lock`. It would make the user's own Git commands fail during a restore, and a kill would leave it behind.
- The tree binding alone. Two restores planned from one state both pass the stale check when both capture before either writes.

## The CLI asks for six hex digits on the terminal

The command is `agent-snap restore <id> [--cwd <dir>] (--all | --path <path>...)`. It requires either `--all` or at least one `--path`, and refuses both together. Unlike `restore-plan`, it never reads a missing scope as the whole tree. It has no `--yes`, no `--current-tree`, and no `--offset`.

It runs in this order:

1. Parse the arguments. Bad arguments exit `1`.
2. Check that standard input, standard output, and standard error are each a terminal, and that each is the controlling terminal. Then open the controlling terminal, `/dev/tty` on POSIX or the console on Windows. When any check fails, it prints that a restore needs a terminal, makes no backup, and exits `2`.
3. Refuse on Windows, and refuse when `git version` is older than 2.30, with the messages of steps 1 and 2 of the restore. Each exits `1` with no plan and no backup.
4. Plan. A plan that is not applicable prints its blockers and the command that lists them, and exits `2` with no backup.
5. Create the backup, as `confirmRestore` does with source `restore-backup` and no host. When its tree differs from `current_tree`, it says that the working tree changed and exits `2`.
6. Print the prompt to the terminal. FR-16 defines its content, which is the content that `confirmRestore` shows.
7. Read one line from the terminal. Only the last six hex digits of the backup ID, in lowercase, confirm. Any other line, an empty line, or the end of input declines. A decline says that nothing was written and that the backup stays in the timeline, and exits `2`.
8. Restore in the same process with the five values. The core's own checks in steps 1 and 2 of the restore still run.
9. Print a summary to the terminal, and print the result object as JSON to standard output.

It never reads the confirmation from standard input. The digits are not a secret, because the prompt prints them. They make the person read the backup ID.

The three stream checks stop an agent's shell tool from raising the prompt in the person's terminal. Such a tool usually starts the CLI with pipes on all three streams, and the process can still open `/dev/tty`, so the terminal check alone would let the agent put a prompt in front of the person. A person at a shell has the terminal on all three. A wrapper such as `script` gives an agent a terminal, and it stays outside the guarantee, as in [A human confirms outside the agent's reach](#a-human-confirms-outside-the-agents-reach).

An interrupt before the write prints that nothing was written and exits `2`. An interrupt during the write prints the stop command described in [A started write runs to its end](#a-started-write-runs-to-its-end), and the CLI keeps waiting.

Exit codes:

| Code | When |
| --- | --- |
| `0` | `applied` or `already_applied`. |
| `1` | An error before any write. Bad arguments, an unresolved repository, a missing snapshot, Windows, an old Git, or another `-32603` that is not a cancel. |
| `2` | A refusal that wrote nothing. No terminal, blocked, stale, `working tree changed during the restore`, a mismatch, a busy worktree, a decline, or `restore cancelled before the write`. |
| `3` | `stopped` or `unknown`. |

## Pi restores only from a typed command in its terminal UI

Pi gets `/agent-snap-restore <id> --all` and `/agent-snap-restore <id> <path> [<path>...]`. This record checked the Pi API in `packages/pi/node_modules/@earendil-works/pi-coding-agent/dist/core/extensions/types.d.ts`, version 0.84.1. `ExtensionContext` has `mode`, typed `"tui" | "rpc" | "json" | "print"`, and `hasUI`, documented as true in TUI and RPC modes. `ExtensionUIContext` has `confirm(title, message)` and `input(title, placeholder)`. A command handler receives `ExtensionCommandContext`, which adds `waitForIdle()`, and `ExtensionContext` has `isIdle()`. In `dist/core/agent-session.js`, `sendUserMessage` passes `expandPromptTemplates: false`, so another extension cannot run a slash command through it.

The handler parses its own argument string. It splits the string on runs of spaces and tabs. The first token is the snapshot ID. The rest must be exactly `--all`, or one or more paths, none of which starts with `-`. An empty string, a lone ID, `--all` with paths, and any other option print the usage and do nothing. No input reaches the whole tree without the literal `--all`. A path that holds a space or a tab cannot be named here, and the usage says to use the CLI for it.

The handler then runs in this order:

1. Refuse unless `ctx.mode` is `tui` and `ctx.hasUI` is true. In `rpc` mode, dialogs go to whatever drives the RPC, which may not be a person.
2. Refuse while `ctx.isIdle()` is false, and ask the person to wait for the agent to finish.
3. Pause the extension's automatic snapshots. The scheduler holds new triggers and runs one coalesced snapshot when the pause ends.
4. Call `confirmRestore` with the extension's `SnapClient` and host `pi`. Its `ask` shows the prompt with `ctx.ui.confirm`, and after a yes, reads the digits with `ctx.ui.input`. On `unsupported`, report the reason and stop.
5. On `confirmed`, call `SnapClient.restore` with a `timeoutMs` of 10 minutes, then report the outcome, the backup ID, and the first lines of each list with `ctx.ui.notify`. The timeout ends only the steps before the write, and a large tree's capture alone took 3.7 seconds per 10,000 files (obs 10).
6. End the pause, on every path out of the handler.

After `SnapClient.restore` rejects with `SnapRestoreOutcomeUnknownError`, a write may still be running, and the next automatic snapshot can record a half-written tree. The handler still ends the pause. Holding the pause until the lock is free would need Pi to read the lock, and hosts never run Git. An extra checkpoint loses nothing, and a later snapshot replaces it. A plan that reports a held lock read-only would let the handler wait, and that is an FR-15 change that section 14 of the spec lists.

Only this handler receives the `SnapClient` that can restore. The extension's dependencies gain that client, which the default factory creates. Tools and hooks keep receiving `SnapLike`.

## OpenCode prints the CLI command

OpenCode keeps the plan tool and gets no restore. The plan tool's text ends with the CLI command a person runs in a terminal to apply the plan, `agent-snap restore <id> --all`, or one `--path` for each scope path. When a scope path holds any character other than ASCII letters, digits, `.`, `_`, `/`, `@`, `+`, and `-`, or starts with `-`, the text names the command without the paths and says to add one `--path` for each scope path listed above. A path pasted into a shell can then never run as a command. Pi's plan tool ends the same way, naming `/agent-snap-restore`. Nothing else changes in OpenCode.

An agent that reads the plan tool's text learns the command. If it runs the command through its shell tool, the stream checks in step 2 refuse before any backup, because the tool gives the CLI pipes.

## Phase 5 lands in this order

Each step is one pull request that ends in a check. The step that simplifies existing code comes before the steps that add to it. Each step that adds a method, an error code, a CLI command, or a host command updates FR-8 through FR-11, `README.md`, `docs/COMPATIBILITY.md`, and `docs/DIAGNOSTICS.md` in the same pull request.

1. This record and FR-16 (#30). The check was review and a link check.
2. This revision, after the review of #30. The check is review and a link check of the relative links.
3. Prerequisite. #31 (`fix/abort-scratch-cleanup`) lands, so that a cancel before the write stops Git and removes the scratch directory. Steps 7, 9, 10, and 11 depend on its signal handling and its Git helpers.
4. TypeScript. `SnapClient` copies and freezes `serveArgs`, refuses the restore entry in it at construction and at every spawn, and refuses a method that is not a primitive string and `snapshot.restore` in `request`. Tests `serveArgs refuses the restore entry` and `request refuses snapshot.restore` check that each throws a `TypeError` before a process starts, with `["serve", "--restore"]`, `["serve", "-restore"]`, `["serve", "--restore=1"]`, `"snapshot.restore"`, and `new String("snapshot.restore")`. Test `serveArgs pushed after construction does not reach the spawn` pushes `--restore` into the caller's array after construction and checks that the spawn uses only `serve`. This lands before step 10, so no merged commit has a reachable write without it.
5. Store. The second rule of `name_collision` (#32). On a case-insensitive volume, a write below a parent that the disk spells by case blocks as `name_collision`, unless the result tree holds a path below that parent that `HEAD` also holds. `TestPlanRestoreBlocksAWriteBelowACaseSpelledParentUnlessAHEADPathTheRestoreKeepsSpellsIt` covers the exemption, and the other tests in `store_restore_casespelled_test.go` cover the rest. The first rule still blocks the delete entry of a case-only directory rename, as `TestPlanRestoreBlocksTheDeleteOfACaseOnlyDirectoryRename` shows. These tests run on macOS only, because they need a volume that folds case. The check is `go test ./...`.
6. Store. Move the plan computation out of `PlanRestore` into one function that computes every entry from a capture, with no change in behavior. The check is that the existing plan tests pass unchanged.
7. Store. The restore's Git runner. It passes the seven settings to every capture and write command and `core.hooksPath` to every other command. It never passes the sparse pin to the step-8 config read. It removes inherited `GIT_*` variables and starts each command in a new session. For the write, it adds the null device, the standard-error file, and the lock descriptor, and reads that file with the 64 KiB bound. `TestRestoreGitPinsEverySetting` records each command's arguments through a fake `git` on `PATH`. `TestRestoreGitStartsInANewSession` checks that a child's session ID differs from the test's and that a `SIGTERM` to the test's group does not reach it. `TestPlanReadsSparseCheckoutWithoutThePin` checks that the step-8 plan still blocks on `sparse_checkout`. `TestRestoreStderrReadIsBounded` feeds a 10 MiB standard-error file and checks the 4096-byte `error` and a refusal line at its end. The check is `go test ./...`.
8. Store. The restore lock. It takes the lock with `flock` close-on-exec, rewrites the lock file's line, and ends the lock with `LOCK_UN`. `TestRestoreLockEndsWithAProcessThatInheritedIt` starts a child that leaves a sleeping grandchild with the descriptor and checks that a second lock fails before `LOCK_UN` and succeeds after it while the grandchild lives. `TestRestoreLockRefusesWhileHeld` checks `-32005` with the lock file's line in the message. `TestRestoreLockFailureIsNotBusy` replaces `flockFile`, an unexported package variable, with a function that returns `ENOLCK`, and checks `-32603`. The check is `go test ./...` on macOS and Linux.
9. Store. `Store.Restore`, with the adversarial suite below, and a refusal on Windows. Tests reach the points between steps through `afterRestoreStep`, an unexported package variable that holds a function the Store calls after each of steps 1 to 11. Production code leaves it a no-op, and only tests in the package replace it. Signal and terminal cases drive a pseudo-terminal that the test opens from the standard library alone. It opens `/dev/ptmx`, unlocks and names the subsidiary with `ioctl` through `syscall`, and starts the process with `Setsid` and `Setctty`. The pseudo-terminal probe did that on macOS, delivered a `^C` to the child, and saw all three streams report a terminal. The Linux `ioctl` names differ and were not run. The check is `go test ./...` on macOS and Linux, and on Windows for the refusal, plus one Linux CI job with Git 2.30.
10. Protocol. `serve --restore`, `snapshot.restore` validation, the `restore` field of its `ping`, and codes `-32003` to `-32005`. Table-driven tests cover every parameter rule and a shared `serve` that answers `-32601`. They cover a `serve --restore` that answers `ping` and then exits after one restore, and an error for every other method. Argument lists other than exactly `serve --restore` fail. `TestServeRestoreAnswersALineReadAfterACancel` cancels the context, then writes the request, and checks the answer `restore cancelled before the write`. `TestServeRestoreEndOfInputCancelsBeforeTheWrite` closes standard input while step 5 runs, through the seam, and checks that nothing was written.
11. TypeScript types for section 8.6, result validation with every cross-field rule, `SnapClient.restore`, and the guardian's restore mode. Tests cover ping first, an old binary that exits before the ping, and a missing response after the request. A malformed result becomes `SnapRestoreOutcomeUnknownError`, whose message says a write may still be running. Other cases are a repaired `unplanned` path out of order, a whole-tree result whose `result_tree` differs from `snapshot.tree`, a `SnapRestoreIncompleteError` whose message holds no `error` text, and the typed errors. Named tests cover the abort table with `abort before send`, `abort before the write`, `abort during the write`, and the same three for a timeout. Further named tests are `abort in the write callback counts as sent` and `host exit before the write writes nothing`. The last three are `host exit during the write`, `stdin end during the write`, and `core death during the write`. Each of the last three shows that the write finishes and that no `SIGKILL` reaches the core. `close during a hung write rejects after GUARDIAN_WAIT_MS` uses a filter that never ends and checks that the write's group is still alive afterwards. `group check failure after a result still settles` leaves a process in the group.
12. The confirmation module. Tests cover an object literal, a cast, a copy, and a reuse, each refused before a process starts. `confirmRestore ignores subclass and own-property overrides` passes a subclass that overrides `restorePlan`, and a client whose own `request` property returns a forged plan, and checks that the prompt shows the real plan. A plain object with the same methods throws a `TypeError` before any request. Another test answers `unsupported` before any plan on a fake binary whose ping reports `restore needs git 2.30 or later`. Others cover a blocked plan that makes no backup, a changed backup that never calls `ask`, wrong digits, and a prompt with quoted paths. `prompt lists unstaged and untracked entries first` uses 30 deletions that sort after 30 other entries.
13. CLI `restore`. Tests cover no terminal, which exits `2` with no backup, and each of the three streams as a pipe while `/dev/tty` opens, each refused with no backup. They also cover the scope arguments, a decline, and every exit code. An interrupt before the write exits `2`. An interrupt during the write prints `kill -TERM -<pgid>` with the group in the lock file and ends in `applied`. Other cases are an interrupt during the step-11 capture and a run under a pseudo-terminal in the smoke test. The terminal cases use the step 9 harness.
14. Pi `/agent-snap-restore` and the Pi plan tool's last line. Tests cover argument parsing, `rpc` and `print` modes, a busy agent, the paused scheduler, `unsupported`, the 10-minute timeout, and the import test.
15. OpenCode plan tool's last line, with the shell-safe path rule.
16. Windows. A Windows CI job runs the cases in [The write runs outside the core's process group](#the-write-runs-outside-the-cores-process-group). When it passes, this step removes the Windows refusal. Until then, Windows release notes say that restore is not supported.

The Store step's adversarial suite runs on real repositories, SHA-1 and SHA-256, with these cases:

- A dirty tree with staged, unstaged, untracked, ignored, assume-unchanged, and skip-worktree entries, deletions, mode changes, and renames, which the plan holds as a deletion and a creation, including a case-only rename.
- Nested repositories, which the plan blocks as `submodule`, and linked worktrees. A restore in one linked worktree leaves every other worktree byte-identical, and the locks of two worktrees do not block each other.
- Symbolic links. A directory swapped for a link to an outside directory after the plan refuses, and the outside directory is unchanged.
- Each refusal. Stale after an edit, blocked after an ignored file appears at a created path with the file kept, a backup mismatch, a result mismatch, a whole-tree `result_tree` that is not the snapshot's tree, a busy lock, and a lock that a `kill -9` released.
- The step order. A repeat apply returns `already_applied` and writes nothing, although its capture differs from `current_tree`. A repeat apply with a scoped `result_tree` and `paths` omitted refuses with `-32004`. A request with a backup mismatch and a busy lock refuses with `-32004`. A stale request with a busy lock refuses with `-32005`.
- Weak stat settings. `TestRestoreKeepsAnEditUnderWeakStatConfig` sets `core.trustctime=false`, `core.checkStat=minimal`, `core.ignoreStat=true`, and an fsmonitor hook that reports no changes, one per case. In each, an edit after the capture that keeps the size and the modification time makes Git refuse, the edit stays, and the restore answers `working tree changed during the restore`.
- A lagging worktree clock. `TestRestoreKeepsAnEditWhenTheIndexLooksNewer` sets the scratch index's modification time 10 seconds into the future through `afterRestoreStep` after step 9, then edits a planned path in the capture's second, keeping its size. The restore answers `working tree changed during the restore`, and the edit stays. `TestRestoreSetsTheIndexTimeToOne` reads the index's modification time from the seam after step 10 starts and expects exactly 1 second after the epoch, never 0.
- Git's check-phase refusal. `TestRestoreRefusalWithSeveralEditedPaths` edits two and then three planned paths after step 8, once with a clean filter that prints to standard error. Each answers `-32002` with nothing written. A third case also removes a planned `delete` and still answers `-32002`. A read-only directory, which fails after Git started writing, ends in `stopped` with the lists of the section 8.6 example.
- A planned `modify` that someone removes after step 8 is written as planned. The restore reports `applied`, and the backup holds the removed file.
- Repository settings that change after step 8. Turning on sparse checkout with a pattern that leaves out unplanned files keeps those files. A `post-index-change` hook that writes a marker file when it runs leaves no marker.
- Cancellation. A cancel before step 10 writes nothing. A cancel that lands between the signal catch and the context check refuses with nothing written. A cancel of the request context after the check does not stop Git.
- Signals during the write, each with a required smudge filter that installs its own `SIGTERM`, `SIGINT`, and `SIGHUP` handlers, during a 10,000-file write. `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT` sent to the core's process group, and a `^C` on a pseudo-terminal, each end in `applied`. A `SIGKILL` of the core's group leaves Git running to the result, with the lock held until Git exits. A `SIGTERM` sent to Git's own group ends in `stopped`. A smudge filter that leaves a sleeping background process ends in `applied`, and a second restore can take the lock right after the first answers.
- Failure between the backup and the write. A fault injected after each of steps 2 to 9 leaves the working tree, the real index, refs, `HEAD`, and configuration byte-identical.
- Failure during the write. A read-only directory ends in `stopped` with correct lists and an `error`. A Git failure whose capture equals `result_tree` ends in `unknown`. A capture failure injected in step 11 ends in `unknown` with no `after_tree`, and keeps Git's `error` when Git also failed. A `kill -9` of Git part way leaves a working tree that a new plan, backup, and restore bring to `result_tree`.
- `.gitattributes` changes, smudge filters, line endings, and the executable bit match `git checkout` of the result.
- A `HEAD` that moved from CRLF to LF content under `core.autocrlf=true` since the snapshot ends in `unknown`, with the snapshot's bytes on disk.
- A created path below an existing NFD directory on APFS ends in `applied`, as documented. This case runs on macOS only.
- A write that a concurrent change makes differ from `result_tree` ends in `unknown`, with the change in `unplanned`.
- An unset scope fails with `ErrNoScope` before Git starts. Git older than 2.30 refuses before any Git work other than `git version`.

One test fingerprints the whole worktree, the real index, every ref, `HEAD`, the configuration, and every other worktree before and after each case. It fails when any path outside the plan, any index entry, any ref, or any other worktree changed. Parent directories that Git creates for created paths, and directories that the deletions emptied, are part of the plan for this test. In the Git directory, only the lock file and object modification times may differ. A configured filter program can write elsewhere, as FR-15 allows, so the fingerprint cases use filters that write nothing in the Git directory. The repository's hooks do not run, so the fingerprint cases may keep them.

## How the design meets the spec's restore requirements

| Section 10 requirement | Met by |
| --- | --- |
| 1. Explicit selection | A snapshot ID through FR-12 lookup, and a literal path allow-list. |
| 2. Preflight | `snapshot.restore_plan` and its blockers, recomputed from a fresh capture immediately before the write (FR-16). |
| 3. Preview | Plan entries with their flags, and `snapshot.diff` between the backup and the snapshot. |
| 4. Backup | `snapshot.create` before the confirmation, checked again by tree before the write (FR-16). "Fresh" is read as above. |
| 5. Conflict preservation | Per-path `staged`, `unstaged`, and `untracked` flags, an index that is never written, the backup, and pinned stat settings so that Git refuses to overwrite a later edit. "Preserved" is read as above. |
| 6. Atomicity | Every refusal comes before the first write. A write that started runs in its own session and reports, for each planned path, whether the capture after it holds the target, the current side, or neither. Recovery is a restore of the backup (FR-16). |
| 7. Confirmation | The host rules above, the one-shot entry that no `SnapClient` option or request reaches, `confirmRestore`, and no write path for hooks or tools. |
| 8. Auditability | The restore result object (section 8.6), which names the snapshot, the backup, the scope, both bound trees, the tree after the write, and the outcome. |
| 9. Testing | The Phase 4 pull requests above, then the Phase 5 adversarial suite. |

## Review decisions

Review settled five product questions on 2026-11-01, each as this record proposed:

1. A restore deletes the untracked files that the snapshot lacks within the scope, by default. Opting in would leave an agent's new files behind after a default restore.
2. A restore overwrites an unstaged change after the user saw it flagged and the backup holds it. Blocking every `unstaged` entry would refuse the agent edits a restore exists to undo.
3. The confirmation guarantee covers Blink's own tools, hooks, and shared transport, and not an agent with a shell. Such an agent can already run Git, so no Blink mechanism would protect more.
4. OpenCode gets the plan tool only. Its permission prompt can be set to allow without asking, so it cannot prove that a person answered.
5. `snapshot.restore_plan` and `snapshot.restore` replace the roadmap's "`snapshot.restore` dry-run API".

#30 merged before its adversarial review finished. Two reviewers on different models then showed that the merged design could lose an edit under weak stat settings, write without a confirmation through public `SnapClient` options, and report `applied` after a filter died. This revision answers that review. It pins the stat settings, runs the write in its own session, refuses the restore entry everywhere outside `SnapClient.restore`, honors an `AbortSignal` before the write, and requires Git's exit status for `applied`. It also sets a minimum Git version and refuses on Windows until CI proves the write there.

#33, the revision, went through a second adversarial review on two models. One reviewer failed it and the other passed it with notes. The failing finding was measured, and the coordinator reproduced it. When the scratch index looks newer than an edit made in the capture's second, Git trusts stat data and overwrites that edit under all five pins of the first revision. This revision sets the scratch index's modification time to 1 second after the epoch, so Git compares content for every planned path. It also decides "nothing written" from the capture instead of from the one path that Git names. It pins sparse checkout and hooks off and ends the lock with `LOCK_UN`. It names the per-path lists by what the capture shows, and it makes the abort table hold against #31's `Serve`. The 2.30 floor is the minimum Git that `README.md` and `docs/RELEASE.md` already state. The restore uses no Git feature newer than that, and the floor refuses Git that no CI job has run the suite against.

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

The write-engine probe is summarized in [The write is a two-way read-tree](#the-write-is-a-two-way-read-tree). Its report numbers observations 1 to 11 and two extra rows, and this record cites them by those numbers. It did not observe Windows and NTFS, Git LFS and long-running process filters, `core.fsmonitor`, Git older than 2.54, racy-clean index timestamps, or case-sensitive volumes. Its timing runs slept 1.1 seconds before each capture, so no index entry was racily clean.

A signal probe for the first version of this record ran on the same Git and macOS. It built a 20,000-file repository, captured it into a scratch index, slept 1.1 seconds, and sent a signal to `read-tree -m -u` 0.6 seconds after it started. It observed:

- With default signal handling, `SIGTERM` stopped Git with exit `143`. 5807 files held the result, 14192 held the current state, and one was in flight.
- With `SIGTERM` ignored by the parent shell and so inherited by Git, every one of the 20,000 files held the result. Git then exited `128` with `index.lock write error: Bad file descriptor`. Git's own handler had removed the scratch index lock, then the inherited ignore swallowed the signal it raised again.
- With `SIGINT` ignored the same way, the result was the same as for `SIGTERM`.
- A Go 1.27.3 program that called `signal.Ignore` for `SIGINT` and `SIGTERM` after `signal.NotifyContext`, then ran `sh -c 'kill -TERM $$; kill -INT $$; echo child-survived'`, printed `child-survived`. Without `signal.Ignore`, the child died from `SIGTERM`. So a Go parent passes an ignored signal to its children.

This revision ran three more probes on Git 2.54.0 (Apple Git-157), Go 1.27.3, and Python 3.14 on macOS, each in fresh repositories under `mktemp -d` with a private `HOME`, `GIT_CONFIG_NOSYSTEM=1`, and no inherited `GIT_*` variables. They are summarized where the record uses them:

- The stat-pin probe, in [The write is a two-way read-tree](#the-write-is-a-two-way-read-tree). It also checked that `core.ignoreStat=true` leaves the captured content alone.
- The isolation probe, in [The write runs outside the core's process group](#the-write-runs-outside-the-cores-process-group). It drove the stand-in core through a pseudo-terminal for `^C`, and through `killpg` for the guardian's signals.
- The round-trip probe, at the end of [The write is a two-way read-tree](#the-write-is-a-two-way-read-tree). Its line-ending case reproduced the trees that the review of #30 reported, `8702e9fc` after the write against `2cd5cf83` for the snapshot.

The revision after the review of #33 ran these probes on Git 2.54.0 (Apple Git-157) and Go 1.27.3 on macOS, under the same rules. The lock probe also ran on Linux 6.18 in a Debian 11 container, and the locale case used Homebrew Git 2.56.0. Git 2.30.2 could not be installed in that container, so no probe ran on Git older than 2.54.

- The racy-index probe and the cost probe, in [The write is a two-way read-tree](#the-write-is-a-two-way-read-tree).
- The rule probe and the template probe, in [A restore runs in eleven steps](#a-restore-runs-in-eleven-steps). The rule probe also ran the read-only directory and the removed planned `modify`.
- The sparse probe, the config-read probe, the hooks probe, and the fsmonitor probe, in [The write is a two-way read-tree](#the-write-is-a-two-way-read-tree).
- The lock probe, in [The write runs outside the core's process group](#the-write-runs-outside-the-cores-process-group).
- The pseudo-terminal probe, in landing step 9 of [Phase 5 lands in this order](#phase-5-lands-in-this-order).

The Pi API facts in [Pi restores only from a typed command in its terminal UI](#pi-restores-only-from-a-typed-command-in-its-terminal-ui) come from reading the installed Pi 0.84.1 type declarations and `agent-session.js`. They were not run.

### Phase 5 assumptions

Each assumption the Phase 5 design relies on is listed with its label. An observed fact has a probe row. An inferred fact follows from code or documentation that this record read but did not run. A requirement is a rule that a later pull request builds and the named test proves.

| Assumption | Label | Proven by |
| --- | --- | --- |
| A two-way read-tree on the capture's scratch index never writes the real index, `HEAD`, refs, configuration, or the real object store. | Observed (obs 1) | Store step fingerprint test. |
| Git refuses before its first write when a tracked planned path was changed or replaced after the capture, with the four stat settings pinned and the scratch index's modification time at 1 second after the epoch. | Observed (obs 3, stat-pin probe, racy-index probe) | `TestRestoreKeepsAnEditUnderWeakStatConfig` and `TestRestoreKeepsAnEditWhenTheIndexLooksNewer`. |
| With the index's modification time in the future or at 0, Git trusts matching stat data and overwrites an edit made in the capture's second. | Observed (racy-index probe, and the review of #33 on FAT32) | `TestRestoreKeepsAnEditWhenTheIndexLooksNewer` and `TestRestoreSetsTheIndexTimeToOne`. |
| With the index time at 1 second, the write runs the clean filter once per file in the scratch index, planned or not. | Observed at 1,000 and 20,000 files (cost probe) | None. The cost is documented. |
| Git writes a planned path that someone removed after the capture, with exit `0`. | Observed (rule probe, and the review of #33) | Store step 9 case for a removed planned `modify`. |
| Without the pins, `core.trustctime=false` or `core.checkStat=minimal` lets Git overwrite an edit that kept its size and modification time. | Observed (stat-pin probe) | The same test, which fails if a pin is dropped. |
| The pins do not change the captured tree. | Observed (stat-pin probe) | Store step test that a `create` after a restore plan records `current_tree` under each setting. |
| Every check-phase refusal of `unpack-trees` comes before Git's first write. | Observed for `not uptodate` (obs 3, rule probe). Inferred from Git's source for the others. | Store step 9 test per check-phase message. |
| Git names only the first refused path, and the capture holds every unnamed planned path at its current side or, when someone changed it, at neither side. | Observed with two and three edited paths, with and without a filter that prints (rule probe, and the review of #33) | `TestRestoreRefusalWithSeveralEditedPaths`. |
| Git translates the `error: ` prefix and not the check-phase message. | Observed on Homebrew Git 2.56.0 in German (rule probe). Both binaries hold the seven templates, and the German catalog holds none of them (template probe). | `TestRestoreRefusalWithSeveralEditedPaths` with `LANG` set to a translated locale where the CI Git has one. |
| `core.sparseCheckout=false` keeps a sparse checkout turned on after step 8 from removing unplanned files, and a `-c` pin hides the repository's value from `git config`. | Observed (sparse probe, config-read probe, and the review of #33) | Store step 9 sparse case and `TestPlanReadsSparseCheckoutWithoutThePin`. |
| `core.hooksPath` set to an empty directory keeps `post-index-change` from running in the capture and the write, and does not change the captured tree. | Observed (hooks probe, and the review of #33) | Store step 9 hook case. |
| Git refuses an untracked, not ignored path that appears at a created path after the capture. | Observed for one symbolic link (obs 5). Inferred for other untracked files. | Store step test with an untracked file. |
| An ignored file or empty directory that appears between the plan's disk checks and the write can be overwritten. | Observed (obs 6). Accepted as a known window. | None. The window is documented. |
| The capture after a successful write equals `result_tree`, including a file whose clean filter loses bytes. | Observed (extra row 2) | Store step repeat-apply test. |
| The capture after a correct write differs from `result_tree` when `HEAD` moved from CRLF to LF content under `core.autocrlf=true`. | Observed (round-trip probe, and the review of #30) | Store step `autocrlf` case. |
| A path created below an existing NFD directory on APFS captures to `result_tree`. | Observed (round-trip probe) | Store step NFD case. |
| Git in its own session receives no terminal `^C` and no group signal sent to the core's group, and a filter with its own handlers finishes. | Observed (isolation probe) | Store step signal cases. |
| Git in the core's process group stops part way on a terminal `^C` or a group `SIGTERM` when its filter installs its own handler. | Observed (isolation probe, and the review of #30) | None. This is why the session exists. |
| An `flock` lock stays held while a child that inherited its descriptor runs, after the parent died or closed its descriptor. `LOCK_UN` ends it for every holder. | Observed on macOS (isolation probe), and on macOS and Linux 6.18 (lock probe) | `TestRestoreLockEndsWithAProcessThatInheritedIt`, and the Store step 9 `SIGKILL` case. |
| A Go test can open a pseudo-terminal and make it a child's controlling terminal with the standard library alone. | Observed on macOS (pseudo-terminal probe). Inferred for Linux, whose `ioctl` names differ. | Store step 9 signal cases and the CLI step terminal cases, on macOS and Linux CI. |
| Git started from a core that catches the stop signals has default signal handling. | Observed for `SIGTERM` (isolation probe) | Store step case that stops Git's group by hand. |
| A Go parent passes an ignored signal to the processes it starts. | Observed (signal probe) | None. This is why the core catches the signals. |
| The core discards `SIGHUP` and `SIGQUIT` as it does `SIGINT` and `SIGTERM`. | Inferred from the Go `os/signal` documentation | Store step signal cases. |
| In a new session, a filter that opens `/dev/tty` fails instead of stopping on `SIGTTIN`. | Inferred from POSIX job control | Store step case with such a filter. |
| On Windows, `CREATE_NEW_PROCESS_GROUP` and `CREATE_NO_WINDOW` keep console events from Git and its filters, and an exclusive-share handle that Git inherits keeps the lock. | Inferred, not observed | Windows step 16 CI job. Until it passes, the core refuses every restore on Windows. |
| `core.fsmonitor=` with an empty value turns the fsmonitor off on every supported Git and starts no program. | Observed on Git 2.54 (fsmonitor probe). Inferred from Git's source for 2.30 to 2.35. | Store step 9 CI job at Git 2.30, with `GIT_TRACE`. |
| An agent's shell tool starts the CLI with pipes on standard input, output, and error. | Inferred | CLI step stream tests. A tool that gives the CLI a terminal is outside the guarantee. |
| An old binary rejects `serve --restore` before it answers, and its shared `serve` answers `snapshot.restore` with `-32601`. | Observed on a binary built from `6d3629e`. It printed `command "serve" takes no arguments`, exited `1`, and wrote nothing to standard output. | Protocol step test with an older binary. |
| Pi runs a slash command only from text a person typed in TUI mode, and never from `sendUserMessage`. | Inferred from `agent-session.js` 0.84.1 | Pi step test of the mode checks. |
| No agent turn starts while the Pi command handler runs. | Inferred, not verified | Pi step test. If it fails, the handler waits with `waitForIdle()` and refuses when a turn starts. |
| `restore-plan` toward the snapshot lists every path a stopped write left behind. | Observed (obs 9) | Store step `kill -9` test. |
