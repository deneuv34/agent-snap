# Restore design

**Status:** Proposed. Phase 4, [#4](https://github.com/deneuv34/agent-snap/issues/4). Confirmed writes stay disabled until Phase 5, [#5](https://github.com/deneuv34/agent-snap/issues/5).

This record decides what a Blink restore does, and how Phase 4 prepares it without writing a user file. It answers the Phase 4 work items in [`BLINK_DEVELOPMENT_PLAN.md`](BLINK_DEVELOPMENT_PLAN.md) and shows how the design meets each acceptance requirement in section 10 of [`BLINK_SPEC.md`](BLINK_SPEC.md). The exact wire contract goes into the spec as a new functional requirement after this record is approved.

A developer who wants to undo an agent's work gets a preview that names every path a restore would write or delete, and a safety snapshot of the current state before anyone is asked to confirm. A host developer gets one read-only call that is safe to give an agent, and a write call that agent tools and lifecycle hooks cannot reach. The next maintainer gets one definition of "the current state", shared by capture, the preview, and the backup.

## A restore moves the working tree between two captured trees

Every restore is a move from a current tree to a result tree. Both are trees in the sense of FR-2, the state that `git add -A -- .` would place in an index seeded from `HEAD`.

- The current tree is the tree that `snapshot.create` would capture at that moment.
- For a full-tree restore, the result tree is the snapshot's tree.
- For a path-scoped restore, the result tree is the current tree with every covered path replaced by its version in the snapshot, or removed when the snapshot lacks it.

A restore writes working-tree files only. It never writes the real index, `HEAD`, branches, or normal refs. After a restore, `git status` shows the restored content as unstaged changes against the index. That is the same result as `git restore --worktree --source=<snapshot>`, plus the deletions described in [the state table](#each-repository-state-has-one-response).

Ignored files are outside both trees. A restore never writes, overwrites, or deletes an ignored file. A planned write that would land on one, or a directory the restore removes while it still holds one, blocks the plan.

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
| `untracked_in_the_way` | Something on disk that the current tree lacks sits where the restore must write or remove. That is the path itself or a parent directory the restore must create, or anything inside a directory the restore deletes or turns into a file or a symbolic link. It covers ignored files, ignored symbolic links, and empty directories. Git's own writes remove such entries silently. A lookup that finds a path the same plan deletes under a different case is a case-only rename, not a blocker. |
| `parent_conflict` | In a path-scoped restore, an entry the snapshot has below a scope path whose parent, outside the scope, is a file, symbolic link, or submodule in the current tree. The restore could write the entry only by changing a path the user did not select. |
| `submodule` | Mode `160000` on either side. Blink does not write submodules or nested repositories. |
| `index_flag` | The real index marks the entry assume-unchanged or skip-worktree (`ls-files -v` shows a lowercase tag or `S`). A write there would be hidden from `git status`. |
| `name_collision` | Two result paths name one file on disk, because they are equal when case is folded or Unicode is normalized. Checked when `core.ignorecase` or `core.precomposeUnicode` is true, or when the file system resolves a tracked path with its case changed to the same file. |
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
- The `serve` process that hooks and tools share never dispatches `snapshot.restore`. Phase 5 decides how the confirmation module reaches the core through `SnapClient`.
- The confirmation shows the snapshot, the scope, the totals for each action and flag, and the backup ID, and says how to list every entry. A user is never asked to confirm a move they were not shown.
- In Pi, a restore runs only from the user-typed command `/agent-snap-restore`, and only when `ctx.hasUI` is true and `ctx.mode` is `tui`. In `rpc` mode the dialog goes to whatever drives the RPC, which may not be a person. Pi tools can open dialogs too, but a dialog that the agent opened invites a reflex yes, so no tool confirms a restore. The command pauses the extension's automatic snapshots while it applies.
- OpenCode 1.x and 2.x get the plan tool only, and its output ends with the CLI command to run. In 1.x, `ToolContext.ask` goes through OpenCode's permission settings, which a user can set to allow without asking, so it cannot prove that a person answered. The 2.x prompt has not been checked, and Phase 5 treats it the same way until it is.
- The CLI opens the controlling terminal, refuses when there is none, and asks the user to type the last six hex digits of the backup ID. It has no `--yes` and never reads the confirmation from standard input. The digits make the user read the backup ID. They are not a secret, because the plan prints them.

Blink does not claim to stop an agent that has a shell. Such an agent can run Git itself, start its own `serve`, or give the CLI a terminal with a tool such as `script`. The guarantee covers Blink's own tools, hooks, and shared transport, and the spec will say so. Phase 5 work item 2 asks for the confirmation to be enforced "at the protocol boundary". The core enforces the plan there. It cannot enforce that a person confirmed. Review accepted that limit, and the spec pull request rewords the work item to match.

## Phase 5 writes follow these rules

Phase 5 needs its own design review before any write lands. This record fixes the rules that review must keep:

- Write through Git's checkout machinery, so smudge filters, line-ending conversion, symbolic links, and the executable bit come out as a checkout would write them. Writing blob bytes directly would skip smudge filters and change files on disk.
- Never write through a symbolic link, and never overwrite or remove a path the plan did not list. Git's writes remove ignored files, blocking files, and whole untracked directories silently, so the `untracked_in_the_way` check runs again immediately before the first write.
- Never write the real index.
- Check a repeated apply first. When a fresh capture already equals `result_tree`, the write does nothing and reports success. Only then compare the capture with `current_tree`. Where a filter loses bytes, a repeat can capture to neither tree and fails as stale, which writes nothing.
- Git has no working-tree lock. The write checks each path immediately before it writes that path, and stops at the first one that changed. That narrows the race with a running agent but cannot close it.
- If a write fails, report the paths written, failed, and not yet started, and the backup ID. Recovery is a normal confirmed restore of the backup.

Two write engines meet these rules. Phase 5 picks one by experiment:

- A two-way `read-tree -m -u <current_tree> <result_tree>`. Git creates, deletes, and prunes, and refuses a path that changed after capture. It must run on the scratch index that the fresh capture filled. An index without file timestamps marks every entry as changed, and Git refuses the merge. Git reads attributes from the working tree, so a restore that changes `.gitattributes` may apply the old rules to other paths. Smudge behavior and whether every path is checked before the first write are not yet verified.
- `checkout-index --prefix` into a staging directory, then a rename of each original into a trash directory on the same file system and of each staged file into place. It can roll back to the exact original bytes, including files whose filters lose bytes, at the cost of more code.

The Phase 5 `restore` command exits `0` when the restore finished, `1` on an error before any write, `2` when it refused and wrote nothing (blocked, stale, declined, or no terminal), and `3` when it stopped part way or its outcome is unknown. The Phase 4 commands keep the current CLI rule. They exit `0` on success, including a plan that is not applicable, and `1` on any error.

An automatic snapshot that runs during a restore writes only objects and hidden refs, never the working tree. At worst it records a half-written state, which a later snapshot replaces.

## Phase 4 lands in this order

Each step is one pull request that ends in a check. The steps that simplify existing code come before the steps that add to it.

1. This record.
2. Spec. Add FR-15 for the plan, the error code `-32002` for a stale plan, the backup rule, and the reading of "fresh" and "preserved" above. Replace the restore items in section 10 and the restore decision in section 14 with references to FR-15. Name the plan's private object directory next to the scratch index in section 9 and in `AGENTS.md`. State that the confirmation guarantee does not cover an agent with a shell. In the roadmap, update the method name and reword Phase 5 work item 2 so that the protocol boundary enforces the plan, and the hosts enforce the confirmation.
3. Move the capture steps out of `Store.Create` into one function, with no change in behavior.
4. Make the `validateResult` default an error. Today it passes the result of an unknown method through unchecked.
5. `Store.PlanRestore`, with real-repository tests for each state and blocker above. They cover SHA-1 and SHA-256, linked and nested worktrees, renames, a `HEAD` with CRLF content under `core.autocrlf`, sparse checkout, an unborn branch, and cancellation. One test fingerprints the paths, sizes, and contents under the Git directory before and after a plan. Another records a `create` after each plan and checks that its tree equals `current_tree`.
6. Protocol method `snapshot.restore_plan`, with table-driven validation.
7. TypeScript types, result validation, a typed error for `-32601`, and cancellation tests.
8. CLI `restore-plan`, with smoke and pack-check coverage. Its output ends with the `create` command for the backup.
9. The `agent_snap_restore_plan` tool in Pi and in both OpenCode adapters. Its description and output formatting go in `packages/opencode/src/snapshots.ts`, which the 1.x and 2.x adapters share.

Phase 4 work item 5 asks for tests of failures between backup and write. Phase 4 has no write, so those tests move to Phase 5. The confirmation module, the Pi `/agent-snap-restore` command, the interactive CLI `restore`, the error code for a blocked restore, and `snapshot.restore` itself are Phase 5.

## How the design meets the spec's restore requirements

| Section 10 requirement | Met by |
| --- | --- |
| 1. Explicit selection | A snapshot ID through FR-12 lookup, and a literal path allow-list. |
| 2. Preflight | `snapshot.restore_plan` and its blockers. |
| 3. Preview | Plan entries with their flags, and `snapshot.diff` between the backup and the snapshot. |
| 4. Backup | `snapshot.create` before the confirmation, checked again by tree at apply. "Fresh" is read as above. |
| 5. Conflict preservation | Per-path `staged`, `unstaged`, and `untracked` flags, an index that is never written, and the backup. "Preserved" is read as above. |
| 6. Atomicity | The Phase 5 write rules and the exit codes. |
| 7. Confirmation | The host rules above, and no write path for hooks or tools. |
| 8. Auditability | The five bound values and the backup ID in every result. |
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
- A two-way `read-tree -m -u` from a tracked directory `D` to a file `D` deleted the ignored file `D/y.log` and exited `0`.
- A two-way `read-tree -m -u` on an index filled by `read-tree` alone failed with "Entry 'a' not uptodate". On the index left by `read-tree HEAD` and `add -A`, the same merge succeeded.

Five things are assumed and not yet observed. They are two-way `read-tree -m -u` with smudge filters, what Git LFS and `core.fsmonitor` write during a plan, the `name_collision` probe, Windows and NTFS behavior, and Git versions older than 2.54. The Store pull request turns each observed fact that the plan relies on into a Go test.
