# Blink Product and Technical Specification

**Status:** Draft
**Current implementation name:** `agent-snap`
**Product name:** Blink
**Tagline:** Craft confidently. Review intelligently. Restore instantly.

## 1. Purpose

Blink protects AI-assisted software-development sessions by recording safe, inspectable checkpoints of a Git working tree. It gives developers a trustworthy timeline of the changes an agent made, without modifying the repository's branch history, `HEAD`, real Git index, or working tree during snapshot creation.

Blink is designed to make agent work reversible and reviewable. The current MVP provides safe capture and discovery; intelligent review and restore are planned product capabilities and are explicitly not implemented yet.

## 2. Problem Statement

Coding agents can make many changes rapidly, often across an existing uncommitted working tree. Developers need to:

- preserve the exact pre-agent state before work begins;
- capture meaningful settled states during an agent session;
- identify what changed between checkpoints;
- recover confidently from an unwanted change without corrupting unrelated work;
- use the capability from the coding-agent environments they already use.

Normal Git commits are not sufficient for this purpose: committing affects the user's history and index, while copying files does not preserve Git's tree semantics or integrate naturally with repository inspection.

## 3. Product Principles

1. **Zero impact by default.** Creating or listing a Blink checkpoint must not move `HEAD`, change a branch, stage files in the user's real index, or write working-tree files.
2. **Safety before convenience.** Any future operation that writes to a working tree must be deliberate, validated, previewable, and recoverable.
3. **Git-native storage.** Checkpoints are Git commits and trees held behind hidden refs, not a separate copy of the repository.
4. **Agent-native integration.** Checkpoints are created at session lifecycle boundaries and are available as explicit agent tools and commands.
5. **No silent loss.** Timeout, cancellation, shutdown, and concurrency behaviour must fail closed rather than permit a late or ambiguous operation.
6. **Repository isolation.** Worktrees, repositories, and inherited Git environment variables must not redirect checkpoint storage or cause cross-repository interference.

## 4. Scope

### 4.1 Current MVP (implemented)

- Create a snapshot of a Git working tree.
- List snapshots for a repository, newest first.
- Look up one snapshot by identifier and inspect its commit, tree, parent, and changed paths.
- Compare two snapshots and page through their changed files and hunks.
- Preview a restore of all or selected paths, one page at a time, without writing anything. FR-15 specifies this. The Store, protocol, client, CLI, and both hosts build the plan.
- Record which host integration and which event or action asked for each snapshot.
- Expose a CLI and a long-running JSONL protocol server.
- Provide a type-safe TypeScript client for the Go core.
- Integrate automatic and manual snapshot creation with OpenCode and Pi Coding Agent.
- Deduplicate snapshots when the resulting working-tree tree is unchanged.
- Isolate refs per Git worktree and update the latest pointer atomically.

### 4.2 Planned capabilities (not implemented)

- Confirm a restore, require its safety backup, and apply it. FR-15 specifies the backup rule, and FR-16 specifies the confirmation and the write. Phase 5 builds them.
- Retention and garbage-collection policy.
- Product-facing naming, UX, and packaging under `blink`.

### 4.3 Non-goals for the MVP

- Replacing Git branches, commits, stashes, or code review.
- Synchronizing snapshots to a remote service.
- Capturing files excluded by Git ignore rules.
- Editing Git history, rebasing, merging, or staging user changes.
- Restoring files automatically or without an explicit user action.

## 5. Users and Primary Workflows

### 5.1 Developer starting an agent session

1. The agent integration identifies the session repository directory.
2. Blink creates a baseline snapshot before, or immediately after, the session starts according to the host's lifecycle semantics.
3. The developer continues working normally; Blink does not alter their branch, index, or files.

**Success condition:** the baseline is discoverable later and represents the Git-visible working tree at the session boundary.

### 5.2 Developer allowing an agent to complete work

1. The host reports that the agent has settled or become idle.
2. Blink coalesces bursty idle/settled events for 1.5 seconds.
3. Blink creates one checkpoint for the stable state unless the agent becomes busy again or the session shuts down.
4. If the tree is identical to the latest checkpoint, Blink reports the existing snapshot as reused.

**Success condition:** routine agent event bursts do not create redundant checkpoints.

### 5.3 Developer creating a named manual checkpoint

1. The developer or agent invokes the create tool/command with an optional reason.
2. Blink records the current tree and returns a snapshot identifier, commit ID, reason, creation time, changed files when newly created, and deduplication status.

**Success condition:** the user can mark a meaningful boundary such as `before refactor` without affecting normal Git state.

### 5.4 Developer inspecting checkpoints

1. The developer or agent invokes the list tool/command for a repository.
2. Blink returns up to the requested number of snapshots in descending creation order.
3. The caller selects a snapshot identifier and invokes the get tool/command to see its commit, tree, parent, and changed paths.
4. The caller can pass two identifiers to the diff tool/command to see the changed files and hunks between them.
5. The caller can use the returned commit and tree IDs with normal read-only Git inspection commands where appropriate.

**Success condition:** the checkpoint timeline is usable without exposing hidden refs as normal branches or asking the caller to construct ref paths.

### 5.5 Future: developer reviewing and restoring

The target Blink experience is:

1. Select a checkpoint or a pair of checkpoints.
2. Inspect a clear file-level and content-level diff.
3. Choose a full-tree or path-scoped restore.
4. Preview the exact writes and conflicts.
5. Confirm the operation after Blink creates a safety backup of the current state.
6. Apply the restore atomically where possible and report the outcome.

This workflow is a product requirement for the restore release, not an MVP behaviour.

## 6. Functional Requirements

### FR-1: Repository resolution

- Every core operation accepts a working directory (`cwd`).
- Blink resolves the repository root with Git, rather than trusting the caller's directory structure.
- Operations fail with a clear error when `cwd` is not inside a Git repository.
- Git environment variables inherited from the host process are removed for every Git invocation so the resolved repository remains the target.

### FR-2: Snapshot creation

- The operation MUST represent the state that `git add -A -- .` would place in a Git index rooted at the resolved repository.
- `.gitignore` MUST be honoured.
- The operation MUST support repositories with and without an existing `HEAD` commit.
- If no nonblank reason is supplied, the stored reason is `manual`.
- The operation MAY receive origin values (`source`, `host`) and stores them as defined in FR-13.
- New snapshot commits MUST use the latest Blink snapshot as their parent when one exists; otherwise, they MAY use the current `HEAD` as parent.
- The operation MUST return the snapshot object defined in section 8.
- Every object ID, ref record, and path list that Blink reads from Git MUST come from Git's standard output alone. A clean filter, a process filter such as Git LFS, or Git itself can print to standard error while a command succeeds, and that text MUST NOT change or fail the create. Blink reads standard error only to report or classify a failed command.

### FR-3: No-impact capture safety

- Snapshot creation MUST use a private temporary `GIT_INDEX_FILE`.
- The temporary directory MUST be outside the target repository, including after symlink resolution, so Blink's own scratch files cannot be captured.
- Scratch files MUST be removed when the operation completes, fails, or is cancelled.
- The binary MUST treat SIGTERM and SIGINT as cancellation of the request in flight. It stops that request's Git children, removes the request's scratch directory, and then exits. A second signal ends the process at once. SIGKILL and a Windows forced termination run no cleanup, so a scratch directory can remain after them.
- Snapshot creation MUST NOT write the real Git index, modify `HEAD`, update user branches, or modify working-tree files.
- Capture and create MUST run no repository hook and no fsmonitor program. A hook would receive the scratch index, and an fsmonitor answer would decide which files Git reads. Once Blink has resolved the repository, every Git command it runs there passes `core.fsmonitor=` with an empty value, which turns the fsmonitor off on every supported Git. Every Git command of a create or a restore plan also passes `core.hooksPath` naming an empty `hooks` directory in the operation's scratch directory, and `hook.<name>.enabled=false` for each hook that configuration defines with `hook.<name>.command` or `hook.<name>.event`.
- Blink lists the configured hook names once per create or plan, from every configuration scope, with `git config -z --get-regexp '^hook\..*\.(command|event)$'`. A name is the text between `hook.` and the last `.` of the key. A name that contains `=` cannot be named in `-c`, so the operation fails with `-32603` before it captures anything. The message is `list configured hooks: hook name "<name>" contains '=', so it cannot be disabled on the Git command line`. A hook added to the configuration after the listing can still run, like any other program the repository configures.

### FR-4: Hidden, worktree-scoped storage

- Blink MUST store checkpoint refs below `refs/agent-snapshots/` until the product namespace is formally migrated.
- The namespace MUST be derived from the resolved absolute Git directory, so linked worktrees do not share a checkpoint timeline accidentally.
- A snapshot is stored at `refs/agent-snapshots/<namespace>/items/<snapshot-id>`.
- The newest snapshot is stored at `refs/agent-snapshots/<namespace>/latest`.
- The implementation MUST reject symbolic refs at either target path and use `git update-ref --no-deref`.

### FR-5: Deduplication

- Before creating a commit, Blink MUST compare the newly generated tree with the latest snapshot tree.
- If they are identical, Blink MUST return the existing snapshot with `reused: true` and MUST NOT create a duplicate commit or snapshot ref.
- If a new commit is created, it MUST return `reused: false` (omitted from JSON is equivalent to false in the current wire format).

### FR-6: Concurrent creation

- Updates to the per-worktree item ref and `latest` ref MUST occur in one Git ref transaction.
- Moving `latest` MUST use compare-and-swap against the observed prior value.
- If another create wins the race, the operation MUST re-read state and retry rather than overwrite the other snapshot.
- Git waits up to one second for a snapshot ref lock (`core.filesRefLockTimeout`). When the lock is still held after that wait, the create fails with `-32603` and the message `snapshot ref lock <ref>.lock in the Git directory stayed held for 1s; remove it only when no agent-snap or Git process is running in this repository`. It creates no ref.
- Blink MUST NOT remove, rename, or rewrite a ref lock file, because it cannot tell a lock that a dead Git left from one that a live Git holds.
- A concurrent `git pack-refs --all --prune`, which `git gc` and `git maintenance` run, can remove a snapshot ref's directory between Git's `mkdir` and its lock. Git then prints `cannot lock ref '<ref>': unable to create directory for <path>`. Blink retries only that failure. It makes at most 5 attempts and waits 10, 20, 40, and 80 ms between them. A permission failure on that directory prints the same message, so after the fifth attempt the create fails with `-32603` and a message that starts `update refs:`. It creates no ref.
- Every other `cannot lock ref` failure apart from a held lock, such as a directory/file name conflict with an existing ref, a permission failure on the lock file, or a non-directory in the way, fails the create at once with `-32603` and a message that starts `update refs:`. It creates no ref.
- Blink tells a lost compare-and-swap, a held lock, and a removed ref directory apart by Git's English messages. It runs every Git command with `LANGUAGE=C`, so a Git built with GNU gettext prints them in English whatever the caller's locale. It keeps the caller's `LC_ALL`, `LC_CTYPE`, and `LANG`, so a clean or smudge filter sees the caller's character encoding. A filter also sees `LANGUAGE=C`.
- The resulting snapshot-parent chain MUST remain linear per worktree.

### FR-7: Snapshot listing

- Listing MUST return an empty list when no snapshots exist.
- The default list limit is 20.
- JSONL callers may request an integer limit from 1 through 100; the CLI requires a positive integer.
- Results MUST be sorted newest first by snapshot identifier.
- Listing MUST skip an item ref whose name is not a snapshot identifier (FR-12). A skipped ref does not count toward the limit. A create that reuses the latest snapshot (FR-5) reports it under an item ref name that passes the same check.
- Listing and that reuse lookup MUST parse only Git's standard output, never its diagnostics, as FR-12 lookup does. A warning about an item ref whose name Git cannot read MUST NOT hide or rename a real snapshot.
- Listed entries include identifier, commit, reason, and creation time, plus `source` and `host` when the snapshot stores them. `changed_files` and `reused` are creation-result metadata and are not guaranteed on list results.

### FR-8: Core CLI

The current CLI name is `agent-snap`; Blink branding is a future packaging decision.

| Command | Required behaviour |
| --- | --- |
| `create [reason] [--cwd <dir>]` | Create one snapshot with source `cli` and no host, and print JSON. `--reason` and positional reason are mutually exclusive. |
| `list [--cwd <dir>] [--limit <n>]` | List snapshots and print JSON. |
| `get <id> [--cwd <dir>]` | Print the snapshot detail object for one snapshot as JSON. |
| `diff <from> <to> [--cwd <dir>] [--offset <n>] [--max-patch-bytes <n>]` | Print one page of the diff result object (FR-14) as JSON. `--offset` takes an integer from 0 through 2147483647, and `--max-patch-bytes` an integer from 1024 through 1048576. |
| `restore-plan <id> [--cwd <dir>] [--path <path>]... [--offset <n>] [--current-tree <oid>]` | Print one page of the restore plan object (FR-15) as JSON. `--path` may repeat, and with none the plan covers the whole tree. `--offset` takes an integer from 0 through 2147483647 and needs `--current-tree` when above 0. `--current-tree` takes a lowercase object ID. The command exits `0` when it prints a plan, including a plan that is not applicable, and `1` on any error. It writes nothing. |
| `ping` | Print protocol status and version JSON. |
| `version` | Print the version string. |
| `serve` | Process newline-delimited JSON requests on standard input and emit one JSON response per request on standard output. |

### FR-9: JSONL protocol

- Each request and response is one JSON object per line.
- Empty input lines are ignored.
- The server MUST continue to process later requests after a request-level parse, validation, or operation error.
- The maximum input line size is 4 MiB.
- An error message that carries Git's standard error keeps only its last 8 KiB. When Git printed more, the kept text starts with `[<n> earlier bytes of standard error omitted]`.
- Supported methods are `ping`, `snapshot.create`, `snapshot.list`, `snapshot.get`, `snapshot.diff`, and `snapshot.restore_plan`.
- Unknown methods return code `-32601`.
- Malformed JSON returns `-32700`; invalid parameters return `-32602`; internal operation failures return `-32603`; a well-formed snapshot identifier with no snapshot in the worktree namespace returns `-32001`, for `snapshot.get`, for either side of `snapshot.diff`, and for the `id` of `snapshot.restore_plan`.
- A `snapshot.restore_plan` request whose `current_tree` the working tree no longer captures to returns `-32002` (FR-15).
- Parameter validation MUST reject unknown keys, missing `cwd`, blank `cwd`, malformed types, invalid list limits, malformed snapshot identifiers, invalid origin values, reasons that end with an origin paragraph (FR-13), a diff `offset` or `max_patch_bytes` outside its range (FR-14), and restore plan `paths`, `offset`, and `current_tree` values that break FR-15. The server rejects every invalid `paths` value, and never reads one as a plan of the whole tree.

### FR-10: TypeScript client

- `SnapClient` launches `agent-snap serve` lazily and reuses one transport while healthy.
- `AGENT_SNAP_BIN` overrides the executable path; the fallback is `agent-snap` on `PATH`.
- The client MUST runtime-validate successful and error responses before exposing them to integrations.
- The client MUST reject a successful result for a method it has no validator for as a protocol error. It never returns such a result unchecked.
- Requests support `AbortSignal` and a default 30-second timeout.
- Timeout or cancellation MUST start teardown of the spawned process tree, preventing a timed-out request from later producing an unobserved checkpoint.
- After a crash or close, the client MAY spawn a fresh transport lazily for a subsequent request.
- `close()` MUST prevent a replacement transport from being created until close completes.
- `SnapClient.diff(cwd, from, to, page?, signal?)` sends `snapshot.diff` and returns the validated diff result object. Validation checks every field's type and enum, and the cross-field rules in section 8.4.
- `SnapClient.restorePlan(cwd, id, options?, signal?)` sends `snapshot.restore_plan` and returns the validated restore plan object. Validation checks every field's type and enum, and the cross-field rules in section 8.5. It also checks that entry paths increase in byte order, and it leaves out entries that carry `unaddressable_path`, because their repaired paths can fall out of order. When the binary answers `-32601`, it rejects with `SnapRestorePlanUnsupportedError`. That class extends `SnapRpcError`, and its message says the installed `agent-snap` cannot plan restores.
- Result validation checks `source` and `host` only for type: each is a string when present. A later release can widen their syntax without breaking older clients.
- Result validation checks every snapshot identifier against the FR-12 syntax. The checked fields are `id` in create, list, and get results, `parent_id`, the diff `from.id` and `to.id`, and the restore plan `snapshot.id`. One malformed identifier rejects the whole result with a `SnapProtocolError` whose message names the field.
- When a create that carries origin values is rejected with `-32602` and the message `unknown param: source` or `unknown param: host`, the client MUST retry that create once without origin values. That rejection is how a 0.1.0 binary answers, and it comes before the repository is resolved, so the retry cannot duplicate a checkpoint. The retry is a new request under the same signal, with its own timeout. The client logs the first time it drops origin. A plugin upgraded ahead of its binary therefore keeps taking snapshots.
- A 0.1.0 binary does not check reasons. The client MUST NOT send that retry when the reason contains `\n\nAgent-Snap-`; the create fails instead, so the retry never stores a reason that a newer reader would take as origin. A create sent without origin values reaches a 0.1.0 binary unchanged, as it did with a 0.1.0 client.

### FR-11: Agent-host integrations

#### OpenCode

- Create a baseline snapshot for `session.created`; failures are logged and must not become unhandled rejections.
- Create a coalesced snapshot after `session.idle` using a 1.5-second window.
- A transition to busy cancels pending idle work and aborts idle work already in flight.
- `session.deleted` cancels work for that session and removes session state.
- During disposal, reject new events, flush pending snapshots, abort remaining work, and close the core client.
- Expose `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`, `agent_snap_get(id, cwd?)`, `agent_snap_diff(from, to, offset?, max_patch_bytes?, cwd?)`, and `agent_snap_restore_plan(id, paths?, offset?, current_tree?, cwd?)` with caller cancellation support.
- Send host `opencode` on every create, with source `session-start` (`session.created`), `session-idle` (`session.idle`), or `tool` (`agent_snap_create`).

#### Pi Coding Agent

- At `session_start`, create a `session-start` baseline and await it as a session-start barrier. Failure is logged rather than propagated.
- At `agent_settled`, create an `agent-settled` snapshot through the same 1.5-second coalescing policy.
- At `session_shutdown`, flush queued work, dispose the scheduler, await tracked baselines, and close the client.
- Expose `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`, `agent_snap_get(id, cwd?)`, `agent_snap_diff(from, to, offset?, max_patch_bytes?, cwd?)`, `agent_snap_restore_plan(id, paths?, offset?, current_tree?, cwd?)`, `agent-snap-create`, `agent-snap-list`, `agent-snap-get <id>`, and `agent-snap-diff <from> <to>`. The diff command shows the first 12 lines of the tool's text.
- Send host `pi` on every create, with source `session-start` (`session_start`), `agent-settled` (`agent_settled`), `tool` (`agent_snap_create`), or `command` (`agent-snap-create`).

#### Both hosts

- The diff tool returns the page as text that lists every file on the page with its status and line counts, then the included hunks, then the `offset` or `max_patch_bytes` to request next when the page does not finish the diff. It ends by naming the citation form from FR-14.
- The diff tool result is Git evidence only. Hosts MUST NOT add generated summaries, rankings, or opinions to it.
- Host tool text prints each value that comes from the repository or a caller through one quoting rule. The values are entry and file paths, `old_path`, scope `paths`, `unmatched`, `changed_files`, snapshot `reason`, and origin `source` and `host`. A value that holds a C0 control character, DEL, a C1 control character, U+2028, U+2029, a double quote, or a backslash is printed in double quotes. Inside the quotes, a backslash is written `\\`, a double quote is written `\"`, and the controls `\a`, `\b`, `\t`, `\n`, `\v`, `\f`, and `\r` use their C escapes. Any other character of those classes is written as the three-digit octal escape of each UTF-8 byte, as in `\033`. A value with none of those characters is printed unchanged, including spaces and non-ASCII text. Non-ASCII text stays raw, as with Git's `core.quotePath=false`. Octal escapes would make every non-English name unreadable, and a letter cannot start a new line. A quoted value is one token on one line, so a name cannot forge a header, an entry, a total, or the closing sentence. Hunk text is evidence and is printed as Git printed it. The JSON results are unchanged.
- The restore plan tool returns the page as text. It says whether the plan is applicable, lists each repository blocker and unmatched scope path, and gives the totals. It then lists every entry on the page with its action, flags, and blocker. When `next_offset` is present, it names the `offset` and `current_tree` to request next. It ends by saying that the plan changed nothing and that this release cannot apply a restore.
- The restore plan tool maps `current_tree` to the client option `currentTree` and passes an option only when the caller set it. The tool schema bounds `paths` to 1 to 100 strings and `offset` to 0 through 2147483647. The binary decides every other rule on `paths`, `offset`, and `current_tree`, and the tool lets its errors reach the host.
- The restore plan tool result is Git evidence only. Hosts MUST NOT add generated summaries, rankings, or opinions to it.

### FR-12: Snapshot lookup

- Lookup takes `cwd` and a snapshot identifier, and resolves the identifier only inside the worktree namespace derived from `cwd`. Identifiers from another worktree are not found.
- An identifier MUST match `<YYYYMMDD>T<HHMMSS>.<nanoseconds>Z-<12 lowercase hex digits>` before any ref path is built from it.
- Lookup MUST fail closed when the item ref is symbolic, does not point at a commit, points at a commit with more than one parent, points at a commit whose ID does not begin with the identifier's 12-digit suffix, or points at a commit without the `agent-snap: ` message prefix and the fixed product author and committer identity. The same rules decide whether a parent is reported as a snapshot (`parent_id`).
- Lookup MUST read objects with Git replace objects disabled, and MUST parse only Git's standard output, never its diagnostics. Unparseable Git output is an internal error, not "not found".
- Lookup MUST return the snapshot detail object defined in section 8.2. `changed_files` lists paths changed between the snapshot and its parent (or every path when there is no parent), is capped at 1000 entries, and sets `changed_files_truncated: true` when more than 1000 paths changed.
- Lookup MUST NOT write the index, refs, objects, `HEAD`, or working-tree files.

### FR-13: Snapshot origin

- `snapshot.create` accepts two optional origin values. `source` names the event or action that asked for the snapshot. `host` names the integration that sent the request.
- An origin value MUST match `^[a-z][a-z0-9]*(-[a-z0-9]+)*$` and be at most 64 characters. Any other value, including a blank string or JSON `null`, is invalid.
- Origin is reported by the caller and is never evidence. Any caller can send any valid value, a 0.1.0 writer stores whatever reason it is given, and listing does not verify item refs, so a snapshot can carry an origin that does not describe how it was made. Display and filter on origin; do not draw conclusions from it.
- Callers MUST NOT put session, user, machine, or path identifiers in origin values. Blink enforces only the syntax, which cannot tell such an identifier from an event name. No integration in this repository stores a session identifier.

Stored form:

- With at least one origin value, the commit message is `agent-snap: ` + reason + `\n\n` + the origin lines joined by `\n`, with no trailing newline. The lines are `Agent-Snap-Source: <source>` then `Agent-Snap-Host: <host>`, each only when supplied.
- Without origin values, the message is the 0.1.0 form, `agent-snap: ` + reason.
- Blink writes and parses the origin paragraph itself. It MUST NOT depend on Git's trailer parsing or trailer configuration.

Reading:

1. Remove the `agent-snap: ` prefix and cut the rest at its last `\n\n`. Without a `\n\n`, the whole text is the reason and there is no origin.
2. The text after the cut is an origin paragraph when, split on `\n`, every line matches `^Agent-Snap-([A-Za-z0-9]+(-[A-Za-z0-9]+)*): ([!-~]([ -~]{0,253}[!-~])?)$` and no key repeats. The value is printable ASCII, 1 to 255 characters, with no leading or trailing space.
3. If it is an origin paragraph, the reason is the text before the cut. Otherwise the whole text is the reason and there is no origin.
4. `source` and `host` come from the `Source` and `Host` lines, and each is reported only when its value is a valid origin value. Lines with other keys are ignored, so a later release can add keys whose values fit the line grammar without a schema version.

Bytes are compared exactly. Nothing is trimmed, and `\r` is an ordinary character.

Validation:

- A reason is invalid when reading step 2, applied to the reason alone after its own last `\n\n`, finds an origin paragraph, or when the reason contains a NUL byte, which Git refuses to store. This holds whether or not origin values are supplied. As a result, a reason Blink stores never reads back as origin, and always reads back unchanged.
- Reason and origin values are checked before any Git work. A rejected create writes nothing, even when the tree is unchanged and the create would have reused a snapshot.
- Invalid input fails with `-32602` and one of these messages: `param must be an origin value: source`, `param must be an origin value: host`, `param must not contain a NUL byte or end with an origin paragraph: reason`. A non-string value, including `null`, fails with `param must be a string: <key>`.

Results:

- Deduplication ignores origin. A reused snapshot reports the origin stored on it, not the origin of the request that reused it.
- Create, list, and lookup results include `source` and `host` when the snapshot has them, and omit them otherwise.
- A 0.1.0 snapshot has origin only if its reason ends with an origin paragraph.
- A 0.1.0 reader shows a newer snapshot's origin paragraph as part of its reason. Nothing else about the snapshot changes for that reader.

Values this repository sends:

| Caller | `host` | `source` |
| --- | --- | --- |
| CLI `create` | none | `cli` |
| OpenCode plugin | `opencode` | `session-start`, `session-idle`, `tool` |
| Pi extension | `pi` | `session-start`, `agent-settled`, `tool`, `command` |

### FR-14: Snapshot diff

Selection:

- `snapshot.diff` takes `cwd`, `from`, and `to`, and optionally `offset` and `max_patch_bytes`. `from` and `to` are snapshot identifiers. Each is resolved as FR-12 resolves a lookup, inside the worktree namespace derived from `cwd` and with the same identifier syntax and fail-closed item checks. Commit IDs, refs, and revision expressions are not accepted.
- The diff shows how the tree of `to` differs from the tree of `from`. Either snapshot may be the older one.
- `from` and `to` may be the same identifier, or name snapshots with equal trees. The result then has no files.
- `from` is resolved before `to`. When both are missing, the error names `from`.

Content:

- Each changed path is one file entry. Entries are in the order `git diff-tree` reports them: byte order of the path, with a rename at its new path.
- Renames are detected at 50% similarity, with Git's rename limit set to 1000. Copies are not detected.
- A type change, such as a regular file replaced by a symbolic link, is one entry with status `type_changed`. Its hunks are the deletion of the old content followed by the addition of the new content.
- A symbolic link is compared by its target text. A submodule entry (mode `160000`) is compared by commit ID and appears as `Subproject commit <id>` hunk lines. Blink does not read submodule repositories.
- A file is binary when Git's built-in content rule finds a NUL byte in the first 8000 bytes of either side, or when a side is larger than 512 MiB. A binary entry has no hunks and zero line counts.
- Patches use 3 lines of context, an inter-hunk context of 0, the Myers algorithm, the indent heuristic, and full object IDs. Blink passes each of these options explicitly.

Isolation and determinism:

- Blink computes the diff in a private evidence repository. It is a bare repository in a scratch directory outside the worktree, placed by the same rule as the FR-3 scratch index. Its object store reads the repository's objects through `objects/info/alternates`. It has no refs, no attributes, no replace refs, and no promisor remote. Blink removes it when the request finishes, on success and on failure.
- Git runs in the evidence repository with `GIT_CONFIG_NOSYSTEM=1` and `GIT_ATTR_NOSYSTEM=1`, and with `HOME` and `XDG_CONFIG_HOME` pointing into the scratch directory. Repository, global, and system configuration cannot change the result. Neither can an attribute file in the worktree, in `$GIT_DIR/info`, in the global location, or inside either snapshot tree, nor a replace ref.
- For the same `from`, `to`, `offset`, and `max_patch_bytes`, the same Git executable returns a byte-identical result, whatever the state of the working tree, the index, the configuration, and the other refs. Results from different Git versions can differ, because Git does not promise stable diff output across versions.
- A missing object, as in a partial clone, fails the request with `-32603`. Blink never fetches objects for a diff.
- A diff MUST NOT write the index, refs, objects, `HEAD`, configuration, or working-tree files of the repository. It writes only inside its scratch directory.

Paging and limits:

- Files are numbered from 0 in diff order. A page lists the files from index `offset` (default 0), at most 200 of them.
- `max_patch_bytes` (default 65536, from 1024 through 1048576) is the page's patch budget. A hunk costs the bytes Git printed for its header line and its body lines, newline characters included, before any UTF-8 repair. Paths, other fields, and JSON escaping are not charged.
- A hunk is included whole or not at all.
- Text files receive their hunks in order. A text file is `full` when all its hunks fit in the budget that remains. The first text file that does not fit ends the patches of the page:
  - If no hunk is charged to the page yet, the file is `truncated` and carries the longest prefix of its hunks that fits, possibly none.
  - Otherwise the file is `deferred` and carries no hunks.
- Every text file listed after that file is `deferred`. A binary file costs nothing, never ends the patches, and is `binary`.
- A `truncated` file carries `patch_bytes`, the cost of its whole patch, when that cost is at most 1048576. The same page requested with `max_patch_bytes` set to `patch_bytes` returns that file `full`. When the whole patch costs more than 1048576, `patch_bytes` is absent and Blink cannot return the rest of that file's hunks.
- `next_offset` is the index of the first `deferred` file on the page. With no `deferred` file, it is `offset + files.length` when that is less than `totals.files`, and absent otherwise. When present, it is greater than `offset`.
- An `offset` at or beyond `totals.files` returns an empty `files` array and no `next_offset`.
- `totals` always covers the whole diff, not only the page.
- A result holds at most 200 file entries and at most `max_patch_bytes` of hunk text, before JSON escaping.

Text:

- Every string is UTF-8. When a path, an old path, a hunk section, or hunk text contains bytes that are not valid UTF-8, Blink replaces each run of invalid bytes with one U+FFFD and sets `lossy: true` on that file entry. Line counts, ranges, budget charges, and object IDs come from the original bytes.
- Hunk text keeps carriage returns and every other byte Git printed, apart from that UTF-8 repair.
- Host tool text prints `path` and `old_path` under the quoting rule of FR-11, so the index lines and the `== N <path>` headers show a path that holds a control character, a double quote, or a backslash as one quoted token.

Integrity:

- Blink reads each hunk body by the line counts in its header. It fails the request with `-32603` when a hunk body does not match its header, when the patch sections do not line up with the file entries, or when the hunk lines of a `full` file do not add up to its `additions` and `deletions`.

Evidence and citations:

- A citation of diff evidence is the tuple (`from`, `to`, `path`, `old_start`, `old_lines`, `new_start`, `new_lines`). Its text form is `<path> @@ -<old_start>,<old_lines> +<new_start>,<new_lines> @@`.
- When the host text shows a path in quotes, a citation uses that quoted form, and its `path` is the value the quotes decode to.
- A citation is valid when the diff of `from` and `to` has a file whose `path` or `old_path` equals `path`, with a hunk that has exactly those four numbers. Because the diff is deterministic, anyone can check a citation again by repeating the diff with the same Git executable.
- Review text that a host or agent writes MUST cite evidence this way. Review text is never part of a diff result or a diff tool result. This repository ships no review generator.

Errors:

| Condition | Code | Message |
| --- | --- | --- |
| Unknown param | `-32602` | `unknown param: <key>` |
| `cwd`, `from`, or `to` missing | `-32602` | `missing required param: <key>` |
| `cwd`, `from`, or `to` blank or not a string | `-32602` | `param must be a non-empty string: <key>` |
| `from` or `to` not a snapshot identifier | `-32602` | `param must be a snapshot id: <key>` |
| `offset` not an integer from 0 through 2147483647 | `-32602` | `param must be an integer between 0 and 2147483647: offset` |
| `max_patch_bytes` not an integer from 1024 through 1048576 | `-32602` | `param must be an integer between 1024 and 1048576: max_patch_bytes` |
| `from` or `to` has no snapshot in the namespace | `-32001` | `snapshot not found: <id>` |
| Repository not resolved, item ref refused, no scratch directory outside the worktree, missing object, Git failure, or a failed integrity check | `-32603` | The operation's message |

### FR-15: Restore plan

**Status:** Built in every layer, except the second rule of `name_collision`. The Store, protocol, TypeScript client, CLI, and host tools all ship the read-only plan. [`RESTORE.md`](RESTORE.md) records the design and the order of the pull requests that build it. Each of those pull requests adds its layer to FR-8 through FR-11 and section 7.1. Phase 5 builds the second rule of `name_collision` in the Store before any restore writes the working tree.

A restore plan shows what restoring a snapshot would change in the working tree, and what would stop the restore. It is read-only. This release cannot apply a restore.

Selection:

- `snapshot.restore_plan` takes `cwd` and `id`, and optionally `paths`, `offset`, and `current_tree`. `id` is a snapshot identifier, resolved as FR-12 resolves a lookup.
- Without `paths`, the plan covers the whole tree. With `paths`, it holds 1 to 100 repository paths. Each path is valid UTF-8, at most 4096 bytes, and made of segments separated by `/`. No segment is empty, `.`, or `..`, and no path contains a NUL byte. Paths are literal, with no globs and no pathspec magic.
- A scope path covers itself and every path below it, by whole segments. `src` covers `src` and `src/a.go`, and not `srcx`. No scope path may equal or cover another.
- A scope path that covers no path in the current tree or the snapshot tree is listed in `unmatched`.

Trees:

- The current tree is the tree that FR-2 capture would record at that moment. Blink computes it with the same steps as `snapshot.create`, so a `create` of an unchanged working tree records exactly `current_tree`.
- Without `paths`, the result tree is the snapshot tree. With `paths`, the result tree holds every current-tree path that no scope path covers, and every snapshot-tree path that a scope path covers.
- Neither tree is stored in the repository. Their objects stay in the plan's scratch directory and are removed with it.

Capture safety:

- The plan uses a scratch `GIT_INDEX_FILE`, and sets `GIT_OBJECT_DIRECTORY` to a private object directory. Both live in a scratch directory placed by the FR-3 rule. The private directory's `info/alternates` names the repository's object directory, so Git reads every existing object and writes only new objects, into the private directory.
- Git runs in the repository, so its excludes, attributes, filters, and configuration apply as they do for capture.
- A plan MUST NOT write the index, refs, `HEAD`, configuration, logs, or working-tree files, and MUST NOT add, remove, or change the content of any file in the Git directory. Git may refresh the modification time of an existing object or pack file that it would otherwise write.
- A plan runs the programs that the repository configures for capture, exactly as `snapshot.create` does. They include clean filters and process filters such as Git LFS. What those programs write is outside this guarantee. Like a create, a plan runs no repository hook and no fsmonitor program (FR-3).
- Reads of the real index run `ls-files` with `GIT_OPTIONAL_LOCKS=0`. A plan never runs a command that refreshes the index.
- Blink removes the scratch directory when the request finishes, fails, or is cancelled. A cancelled plan returns no result.

Entries:

- Each covered path that differs between the current tree and the snapshot tree is one entry. Without `paths`, every path is covered. A path differs when its mode or object ID changes, or when only one tree has it. Entries are in byte order of the path.
- The action is `create` when only the snapshot tree has the path, and `delete` when only the current tree has it. It is `type_change` when the two trees hold different kinds of entry (regular file, symbolic link, or submodule), and `modify` otherwise. Renames are not detected, so a rename is a `delete` and a `create`.
- `staged` is set when the real index's stage-0 entry and the `HEAD` entry for the path differ, or only one exists. On an unborn branch, `HEAD` is the empty tree.
- `unstaged` is set when the real index has a stage-0 entry for the path, and the current tree lacks the path or holds a different mode or object ID.
- `untracked` is set when the current tree has the path and the real index has no entry for it. `unstaged` and `untracked` are never both set.
- A path that is not valid UTF-8 is reported with each run of invalid bytes replaced by one U+FFFD, as FR-14 does.
- An entry carries at most one blocker, the first in the order of the path-blocker table that applies.

Repository blockers, listed in `blockers` in this order, each at most once:

| Blocker | Applies when |
| --- | --- |
| `operation_in_progress` | `MERGE_HEAD`, `CHERRY_PICK_HEAD`, `REVERT_HEAD`, `REBASE_HEAD`, `BISECT_LOG`, `rebase-merge`, `rebase-apply`, or `sequencer` exists at the path that `git rev-parse --git-path` gives for it. |
| `unmerged_index` | The real index has an entry above stage 0. |
| `sparse_checkout` | `core.sparseCheckout` is true for the worktree. |

Path blockers, in the order Blink applies them:

| Blocker | Applies when |
| --- | --- |
| `unaddressable_path` | The path is not valid UTF-8, so no request or result can name it exactly. |
| `unsafe_path` | A segment of the path is empty, `.`, `..`, or `.git` in any letter case, or is a segment that Git refuses to check out under its `core.protectNTFS` or `core.protectHFS` rules. It also applies when the entry is checked out as a symbolic link and a segment is a name that Git refuses for a symbolic `.gitmodules` under those rules. An entry is checked out with its target mode, or with its current mode for a `delete`. Blink applies both rules on every platform. |
| `submodule` | The current or the target mode is `160000`. This includes a nested repository that capture recorded as a submodule. |
| `parent_conflict` | In a path-scoped plan, the action writes the target, and a parent of the path that no scope path covers is a regular file, symbolic link, or submodule in the current tree. |
| `index_flag` | The real index has a stage-0 entry for the path that is marked assume-unchanged or skip-worktree. |
| `name_collision` | The working tree folds case, and the path or one of its parent directories becomes equal, under Unicode simple case folding, to a different path or parent directory in the result tree. It also applies when the working tree folds case, the action writes the target, and a parent directory of the path that the current tree does not hold as a directory exists on disk only under a name that differs from it and is equal to it under Unicode simple case folding. That on-disk directory must also still be there after the restore removed every current-tree path that the plan deletes or replaces. Blink finds it by reading the names that the grandparent directory stores. |
| `untracked_in_the_way` | The action writes the target, and something would still be in the way after the restore removed every current-tree path that the plan deletes or replaces. Anything at the path is in the way. At a parent of the path where the current tree has no directory, anything other than a real directory is in the way, such as a file or a symbolic link. |

- An action writes the target when it is `create`, `modify`, or `type_change`.
- `untracked_in_the_way` covers ignored files, ignored symbolic links, empty directories, and a directory that holds ignored files where the snapshot has a file. A path that the file system resolves to a current-tree path the plan deletes, as in a case-only rename, is not in the way. An entry at the path itself under a name that differs only by case is in the way, because the file system resolves the path to it.
- Git writes a created path into an existing parent directory under that directory's on-disk spelling. The second `name_collision` rule blocks that case, so a restore never records a tree that differs from `result_tree` by the case of a directory name.
- The working tree folds case when `core.ignorecase` is true. It also folds case when the first current-tree path, in byte order, whose last segment holds an ASCII letter names the same file after the case of those letters is swapped. A plan does not check Unicode normalization.

Applicability:

- `applicable` is true exactly when `blockers` and `unmatched` are empty and no entry has a blocker. A plan with no entries is applicable when that holds.
- `result_tree` is absent when any entry meets the `parent_conflict` condition, even when that entry reports an earlier blocker, because no tree can hold both a file and a path below it.
- `HEAD` moving since the snapshot was recorded is not a blocker.

Paging and staleness:

- Entries are numbered from 0. A page lists at most 200 entries, from index `offset` (default 0).
- `next_offset` is `offset + entries.length` when that is less than `totals.entries`, and absent otherwise. An `offset` at or beyond `totals.entries` returns no entries.
- A request with `offset` greater than 0 MUST carry `current_tree`, the value from the first page.
- When a request carries `current_tree` and the working tree no longer captures to it, the plan fails with `-32002` and lists nothing. Each page captures the working tree again.
- `totals` always covers the whole plan.

Safety backup:

- A restore's safety backup is a snapshot recorded by `snapshot.create` with reason `restore backup`, after the plan and before any confirmation.
- The backup is valid for a plan when its tree, read by FR-12 lookup, equals `current_tree`. Origin does not count, because FR-13 origin is not evidence.
- Deduplication (FR-5) applies. A reused snapshot whose tree equals `current_tree` is a valid backup.
- `snapshot.diff` from a valid backup to the snapshot shows the content of a full-tree plan. For a path-scoped plan, it also shows changes outside the scope.

Interfaces:

- CLI `restore-plan <id> [--cwd <dir>] [--path <path>]... [--offset <n>] [--current-tree <oid>]` prints one page of the restore plan object as JSON. `--path` may repeat. The command exits `0` when it prints a plan, including a plan that is not applicable, and `1` on any error.
- `SnapClient.restorePlan(cwd, id, options?, signal?)` sends `snapshot.restore_plan` and returns the validated restore plan object (section 8.5). `options` holds `paths`, `offset`, and `currentTree`. When the binary answers `-32601`, the client rejects with an error that says the installed `agent-snap` cannot plan restores.
- `SnapLike` gains `restorePlan`, and nothing else for restore.
- Both hosts expose `agent_snap_restore_plan(id, paths?, offset?, current_tree?, cwd?)` with caller cancellation support. The tool returns text that says whether the plan is applicable, lists each repository blocker and unmatched scope path, and gives the totals. It then lists every entry on the page with its action, flags, and blocker, and names the `offset` and `current_tree` to request next. It ends by saying that the plan changed nothing, and that this release cannot apply a restore.
- The plan tool result is Git evidence only. Hosts MUST NOT add generated summaries, rankings, or opinions to it.

Errors:

| Condition | Code | Message |
| --- | --- | --- |
| Unknown param | `-32602` | `unknown param: <key>` |
| `cwd` or `id` missing | `-32602` | `missing required param: <key>` |
| `cwd` or `id` blank or not a string | `-32602` | `param must be a non-empty string: <key>` |
| `id` not a snapshot identifier | `-32602` | `param must be a snapshot id: id` |
| `paths` not an array of strings | `-32602` | `param must be an array of strings: paths` |
| `paths` empty or longer than 100 | `-32602` | `param must hold 1 to 100 paths: paths` |
| A path that breaks the path rules | `-32602` | `param must hold only repository paths: paths` |
| Two paths where one equals or covers the other | `-32602` | `param must not hold overlapping paths: paths` |
| `offset` not an integer from 0 through 2147483647 | `-32602` | `param must be an integer between 0 and 2147483647: offset` |
| `current_tree` not 40 or 64 lowercase hex digits | `-32602` | `param must be an object id: current_tree` |
| `offset` greater than 0 without `current_tree` | `-32602` | `missing required param: current_tree` |
| `id` has no snapshot in the namespace | `-32001` | `snapshot not found: <id>` |
| The working tree no longer captures to `current_tree` | `-32002` | `working tree changed since the plan` |
| Repository not resolved, item ref refused, no scratch directory outside the worktree, missing object, Git failure, or a snapshot tree that names one path twice or holds a path below a file | `-32603` | The operation's message |

### FR-16: Restore

**Status:** Specified, not built. Nothing in this release dispatches `snapshot.restore`, and no command writes the working tree. [`RESTORE.md`](RESTORE.md) records the design, its evidence, and the order of the pull requests that build it. Each of those pull requests adds its layer to FR-8 through FR-11 and section 7.1.

A restore applies one restore plan to the working tree after a person confirmed it. It writes working-tree files only.

Entry:

1. `agent-snap serve --restore` is the only protocol entry that dispatches `snapshot.restore`. It answers `ping`, answers the first `snapshot.restore` request, and then exits with status `0`. It answers every other method with `-32601` and keeps reading.
2. `agent-snap serve` MUST answer `snapshot.restore` with `-32601`.
3. The CLI `restore` command runs the restore in its own process and never starts `serve --restore`.

Request:

- `snapshot.restore` takes `cwd`, `snapshot`, `current_tree`, `result_tree`, and `backup`, and optionally `paths`.
- `snapshot` and `backup` are snapshot identifiers. Each is resolved as FR-12 resolves a lookup, `snapshot` first.
- `paths` follows the FR-15 path rules. Without `paths`, the restore covers the whole tree.
- `current_tree` and `result_tree` are full object IDs, 40 or 64 lowercase hex digits.
- The server rejects an invalid parameter before any Git work, as FR-15 does, and never reads an invalid `paths` value as the whole tree.

Sequence. A restore MUST run these steps in this order:

1. Validate the parameters. A restore entry in the Store that takes a scope MUST refuse an unset scope before any Git work.
2. Resolve the repository, then look up `snapshot` and `backup`.
3. Refuse with `-32004` when the backup's tree differs from `current_tree`.
4. Take the worktree's restore lock without waiting. Refuse with `-32005` when another process holds it.
5. Capture the working tree as FR-15 does, into a scratch index and a private object directory.
6. When the capture equals `result_tree`, report `already_applied` and write nothing.
7. When the capture differs from `current_tree`, refuse with `-32002`.
8. Recompute every entry of the FR-15 plan from that capture, with the request's snapshot and scope. Refuse with `-32003` when it is not applicable, and with `-32004` when its result tree differs from `result_tree`. The checks that read the file system run last in this step.
9. For a path-scoped restore, write the result tree's tree objects into the private object directory, and check that its ID equals `result_tree`.
10. Write with `git -c core.splitIndex=false read-tree -m -u <current_tree> <result_tree>` on the scratch index from step 5.
11. Capture the working tree again into a new scratch index, decide the outcome, remove the scratch directory, and report the restore result object (section 8.6).

Safety:

- Every Git command of a restore runs in the resolved repository with inherited `GIT_*` variables removed, and sets only `GIT_INDEX_FILE` and `GIT_OBJECT_DIRECTORY`, as a restore plan does.
- A restore MUST NOT write the index, refs, `HEAD`, configuration, logs, or the repository's object store.
- A restore MUST NOT write, overwrite, or remove a working-tree path that its recomputed plan does not list, apart from removing a directory that its own deletions emptied. One case is outside this rule. Git's write can replace an ignored file or an empty directory that appears at a planned path after the checks of step 8. Git has no working-tree lock, so Blink cannot close that window.
- In the Git directory, a restore creates only its lock file. It may refresh the modification time of an existing object or pack file, as FR-15 allows.
- When a request fails before step 10, the working tree MUST be unchanged by that request. Every error response from `snapshot.restore` means that the request wrote nothing.
- Once step 10 starts, the restore MUST answer with a result and never with an error.

Outcome:

- `applied` when the capture of step 11 equals `result_tree`, whatever Git's exit status was.
- `already_applied` when the capture of step 5 equals `result_tree`.
- `stopped` when the capture of step 11 differs from `result_tree` and the write's Git process failed.
- `unknown` when the capture of step 11 differs from `result_tree` and the write's Git process succeeded, or when that capture failed.
- A planned path is `written` when the capture holds its target, or lacks it for a `delete`. It is `not_started` when the capture holds its current side, and `failed` otherwise. A path outside the plan is `unplanned` when the capture differs from `result_tree` at it.

Cancellation:

- A cancel before step 10 stops the restore. The core stops its Git children, removes its scratch directory, releases the lock, and writes nothing. In the CLI, `SIGINT` and `SIGTERM` cancel.
- From just before the write's Git process starts until the process exits, the core MUST ignore `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT`, and the write's Git process inherits them as ignored. A signal sent to the process group during step 10 or 11 does not stop the restore, and the restore reports its outcome.
- `SIGKILL` and a Windows forced termination cannot be ignored. Either one during step 10 can leave some planned paths written, with no result.

Concurrency:

- The restore lock is an exclusive lock on the file that `git rev-parse --git-path agent-snap-restore.lock` names, in the worktree's own Git directory. The operating system releases it when the process ends. The file stays after the lock is released.
- At most one restore runs in a worktree. Restores in different worktrees of one repository do not block each other.
- `snapshot.create` and `snapshot.restore_plan` take no restore lock. Either can run during a restore and see a partly written working tree.

Interfaces:

- CLI `restore <id> [--cwd <dir>] (--all | --path <path>...)`. It requires `--all` or at least one `--path`, and refuses both together. It has no option that confirms for the person and never reads the confirmation from standard input. It opens the controlling terminal and refuses when there is none, before it makes a backup. It plans, records the backup, prints the confirmation prompt to the terminal, and restores only after the person types the last six hex digits of the backup ID. It prints the restore result object as JSON to standard output.
- The CLI `restore` command exits `0` for `applied` and `already_applied`, `1` for an error before any write, `2` for a refusal that wrote nothing, and `3` for `stopped` and `unknown`. A refusal includes no terminal, a blocked plan, a stale tree, a mismatch, a busy worktree, a decline, and an interrupt before step 10.
- `confirmRestore(snap, request, ask)` in `packages/core` plans, records the backup, and checks that the backup's tree equals the plan's `current_tree`. It then shows the prompt through `ask`, and returns a confirmation only when the typed answer equals the last six hex digits of the backup ID. It makes no backup when the plan is not applicable, and it does not call `ask` when the backup's tree differs.
- The confirmation prompt names the snapshot with its creation time and reason, the scope, and the totals for each action and flag. It lists at most the first 20 entries with their action and flags. It names the backup ID and the commands that list every entry and show the diff from the backup to the snapshot. Paths and reasons print under the quoting rule of host tool text.
- `SnapClient.restore(confirmation)` refuses an object that `confirmRestore` did not return, or one already used, with a `TypeError` before it starts a process. It starts `agent-snap serve --restore`, sends `ping`, then sends one `snapshot.restore`, and waits for the response with no timeout. It takes no `AbortSignal`. It resolves with the validated result for `applied` and `already_applied`, and rejects with an error that carries the result for `stopped` and `unknown`. When the process exits before it answers the ping, the client rejects with an error that says the installed `agent-snap` cannot apply restores. When the process exits after the request and before a response, the client rejects with an error that names the backup ID and says the outcome is unknown. `close()` waits for a restore whose request was sent.
- `SnapLike` gains nothing for this requirement.
- Pi registers `/agent-snap-restore <id> --all` and `/agent-snap-restore <id> <path>...`. The command runs only when `ctx.mode` is `tui` and `ctx.hasUI` is true, and only while the agent is idle. It pauses automatic snapshots until it ends. An argument string without exactly `--all` or at least one path after the ID prints the usage and does nothing.
- OpenCode gets no restore. The restore plan tool in both hosts ends with the command that applies the plan, `agent-snap restore` in OpenCode and `/agent-snap-restore` in Pi. The command line names the scope paths only when each path holds only ASCII letters, digits, `.`, `_`, `/`, `@`, `+`, and `-` and does not start with `-`.

Errors:

| Condition | Code | Message |
| --- | --- | --- |
| Unknown param | `-32602` | `unknown param: <key>` |
| `cwd`, `snapshot`, `current_tree`, `result_tree`, or `backup` missing | `-32602` | `missing required param: <key>` |
| `cwd`, `snapshot`, or `backup` blank or not a string | `-32602` | `param must be a non-empty string: <key>` |
| `snapshot` or `backup` not a snapshot identifier | `-32602` | `param must be a snapshot id: <key>` |
| `paths` not an array of strings | `-32602` | `param must be an array of strings: paths` |
| `paths` empty or longer than 100 | `-32602` | `param must hold 1 to 100 paths: paths` |
| A path that breaks the path rules | `-32602` | `param must hold only repository paths: paths` |
| Two paths where one equals or covers the other | `-32602` | `param must not hold overlapping paths: paths` |
| `current_tree` or `result_tree` not 40 or 64 lowercase hex digits | `-32602` | `param must be an object id: <key>` |
| `snapshot` or `backup` has no snapshot in the namespace | `-32001` | `snapshot not found: <id>` |
| The working tree captures to neither `current_tree` nor `result_tree` | `-32002` | `working tree changed since the plan` |
| The recomputed plan is not applicable | `-32003` | `restore plan is not applicable` |
| The backup's tree differs from `current_tree` | `-32004` | `backup does not match current_tree` |
| The recomputed result tree differs from `result_tree` | `-32004` | `restore plan does not match result_tree` |
| Another process holds the worktree's restore lock | `-32005` | `another restore is running in this worktree` |
| Repository not resolved, item ref refused, no scratch directory outside the worktree, missing object, a cancel, or a Git failure before step 10 | `-32603` | The operation's message |

## 7. Technical Architecture

```text
OpenCode plugin ─┐
                 ├─ TypeScript SnapClient ─ JSONL/stdin-stdout ─ Go core ─ Git plumbing
Pi extension ────┘                                                │
                                                                    └─ hidden refs and Git objects
```

### 7.1 Components

| Component | Responsibility |
| --- | --- |
| `cmd/agent-snap` | User-facing CLI and process entrypoint. |
| `internal/protocol` | Strict JSONL request parsing, parameter validation, dispatch, and JSON-RPC-style errors. |
| `internal/snapshot` | Repository discovery, scratch-index snapshot construction, hidden-ref persistence, deduplication, listing, lookup, diff, and restore plans. |
| `packages/core` | Typed Node client, response validation, process lifecycle management, cancellation, and teardown. |
| `packages/opencode` | OpenCode lifecycle integration and manual tools. |
| `packages/pi` | Pi lifecycle integration, manual tools, and commands. |

### 7.2 Storage model

A Blink snapshot is a Git commit containing the snapshot tree. It is intentionally not attached to a normal branch. Its item ref preserves discoverability and its worktree-specific `latest` ref establishes the checkpoint chain.

The commit message is `agent-snap: <reason>`, followed by the origin paragraph from FR-13 when the snapshot has origin values:

```text
agent-snap: before-refactor

Agent-Snap-Source: tool
Agent-Snap-Host: pi
```

Commit author and committer identity are fixed to the local product identity (`agent-snap <agent-snap@localhost>` in the current implementation), and timestamps are in UTC.

## 8. Data Contracts

### 8.1 Snapshot object

```json
{
  "id": "20260814T120000.000000000Z-abc123def456",
  "commit": "abc123def456...",
  "reason": "before-refactor",
  "created_at": "2026-08-14T12:00:00Z",
  "source": "tool",
  "host": "pi",
  "changed_files": ["src/main.go"],
  "reused": false
}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `id` | string | Timestamp plus the first 12 characters of the commit ID; unique checkpoint reference within its worktree namespace. |
| `commit` | string | Full Git commit object ID. |
| `reason` | string | Caller-supplied or default checkpoint reason, without the origin paragraph. |
| `created_at` | RFC 3339 timestamp | Creation time of the snapshot. |
| `source` | string, optional | Stored origin `source` (FR-13). Absent when the snapshot stores none. |
| `host` | string, optional | Stored origin `host` (FR-13). Absent when the snapshot stores none. |
| `changed_files` | string array, optional | Paths changed between the new snapshot and its parent; supplied for a newly created snapshot. |
| `reused` | boolean, optional | `true` if the current tree equals the latest checkpoint and Blink reused that checkpoint. |

### 8.2 Snapshot detail object

Returned by lookup (`snapshot.get`).

```json
{
  "id": "20260814T120000.000000000Z-abc123def456",
  "commit": "abc123def456...",
  "reason": "before-refactor",
  "created_at": "2026-08-14T12:00:00Z",
  "tree": "4b825dc642cb...",
  "parent": "9f8e7d6c5b4a...",
  "parent_id": "20260814T115500.000000000Z-9f8e7d6c5b4a",
  "changed_files": ["src/main.go"]
}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `id`, `commit`, `reason`, `created_at` | | As in list results: `created_at` is the snapshot commit's UTC time, to the second. |
| `source`, `host` | string, optional | As in the snapshot object. |
| `tree` | string | Full Git tree object ID of the snapshot. |
| `parent` | string, optional | Full ID of the parent commit: the previous checkpoint, or `HEAD` when the snapshot started the chain. Absent when the snapshot has no parent. |
| `parent_id` | string, optional | Snapshot identifier of the parent when the parent is a snapshot in the same worktree namespace. |
| `changed_files` | string array | Paths changed between the snapshot and its parent, or every path when there is no parent. Always present; at most 1000 entries. |
| `changed_files_truncated` | boolean, optional | `true` when more than 1000 paths changed and `changed_files` holds only the first 1000. |

### 8.3 Request and response examples

Create:

```json
{"id":1,"method":"snapshot.create","params":{"cwd":"/path/to/repo","reason":"before refactor","source":"tool","host":"pi"}}
```

List:

```json
{"id":2,"method":"snapshot.list","params":{"cwd":"/path/to/repo","limit":20}}
```

Get:

```json
{"id":3,"method":"snapshot.get","params":{"cwd":"/path/to/repo","id":"20260814T120000.000000000Z-abc123def456"}}
```

Diff:

```json
{"id":4,"method":"snapshot.diff","params":{"cwd":"/path/to/repo","from":"20261029T101500.123456789Z-1a2b3c4d5e6f","to":"20261029T104200.987654321Z-9f8e7d6c5b4a","offset":0,"max_patch_bytes":65536}}
```

Restore plan, second page:

```json
{"id":5,"method":"snapshot.restore_plan","params":{"cwd":"/path/to/repo","id":"20261029T101500.123456789Z-1a2b3c4d5e6f","paths":["src"],"offset":200,"current_tree":"9c1e7a3f..."}}
```

Success responses contain `id` and `result`. Failure responses contain `id` and `error`:

```json
{"id":2,"error":{"code":-32602,"message":"param must be an integer between 1 and 100: limit"}}
```

### 8.4 Diff result object

Returned by `snapshot.diff`. The example shows the first page of a three-file diff whose budget ran out at the third file.

```json
{
  "from": {"id": "20261029T101500.123456789Z-1a2b3c4d5e6f", "commit": "1a2b3c4d5e6f..."},
  "to": {"id": "20261029T104200.987654321Z-9f8e7d6c5b4a", "commit": "9f8e7d6c5b4a..."},
  "offset": 0,
  "next_offset": 2,
  "totals": {"files": 3, "additions": 14, "deletions": 5},
  "files": [
    {
      "status": "added", "path": "assets/logo.png", "new_mode": "100644", "new_oid": "23c62bf5...",
      "additions": 0, "deletions": 0, "patch": "binary", "hunks": []
    },
    {
      "status": "modified", "path": "src/server.go", "old_mode": "100644", "new_mode": "100644",
      "old_oid": "83db48f8...", "new_oid": "8792505e...", "additions": 2, "deletions": 0, "patch": "full",
      "hunks": [
        {"old_start": 88, "old_lines": 6, "new_start": 88, "new_lines": 8, "section": "func dispatch(ctx context.Context, req *Request) (any, *Error) {",
         "text": " \t\treturn listSnapshots(ctx, req.Params)\n \tcase \"snapshot.get\":\n \t\treturn getSnapshot(ctx, req.Params)\n+\tcase \"snapshot.diff\":\n+\t\treturn diffSnapshots(ctx, req.Params)\n \tdefault:\n \t\treturn nil, &Error{Code: CodeMethodNotFound, Message: \"method not found\"}\n \t}\n"}
      ]
    },
    {
      "status": "renamed", "path": "src/util.go", "old_path": "src/helpers.go", "similarity": 92,
      "old_mode": "100644", "new_mode": "100644", "old_oid": "96cc5588...", "new_oid": "1c5a36f5...",
      "additions": 12, "deletions": 5, "patch": "deferred", "hunks": []
    }
  ]
}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `from`, `to` | object | `{id, commit}`: the snapshot identifier and full commit ID of each side. |
| `offset` | integer | Index of the first listed file, as requested. |
| `next_offset` | integer, optional | Index to request next (FR-14). Absent when the page finishes the diff. |
| `totals` | object | `{files, additions, deletions}` over the whole diff. Binary files add 0 lines. |
| `files` | array | At most 200 file entries, starting at `offset`. |

File entry:

| Field | Type | Meaning |
| --- | --- | --- |
| `status` | `"added"`, `"deleted"`, `"modified"`, `"renamed"`, or `"type_changed"` | Kind of change. `modified` covers content and mode changes. |
| `path` | string | Path in `to`, or in `from` for a deletion. |
| `old_path` | string, optional | Path in `from`. Present only for `renamed`. |
| `similarity` | integer, optional | Git's similarity score, 50 through 100. Present only for `renamed`. |
| `old_mode`, `old_oid` | string, optional | Mode (six octal digits) and full object ID in `from`. Absent for `added`. |
| `new_mode`, `new_oid` | string, optional | Mode and full object ID in `to`. Absent for `deleted`. For mode `160000` the ID is a submodule commit. |
| `additions`, `deletions` | integer | Added and deleted lines of the whole file, even when its hunks are not on the page. 0 for `binary`. |
| `patch` | `"full"`, `"truncated"`, `"deferred"`, or `"binary"` | Whether the page carries the file's hunks (FR-14). |
| `patch_bytes` | integer, optional | Cost of the whole patch. Present only for `truncated`, when at most 1048576. |
| `hunks` | array | The included hunks. Empty unless `patch` is `full` or `truncated`. A `full` patch is empty for a mode-only change, an exact rename, or an empty file. |
| `lossy` | `true`, optional | Some bytes of a path or a hunk were not valid UTF-8 and were replaced with U+FFFD. |

Hunk:

| Field | Type | Meaning |
| --- | --- | --- |
| `old_start`, `old_lines`, `new_start`, `new_lines` | integer, at least 0 | The ranges from Git's hunk header. A count that Git omits is 1. |
| `section` | string, optional | Text Git printed after the closing `@@` of the header. |
| `text` | string | Body lines. Each line ends with `\n` and starts with a space, `+`, `-`, or `\`. |

A client MUST reject a result that breaks any of these rules:

- `old_path` and `similarity` are present exactly when `status` is `renamed`.
- `old_mode` and `old_oid` are absent exactly when `status` is `added`. `new_mode` and `new_oid` are absent exactly when `status` is `deleted`.
- `binary` has zero line counts and no hunks. `deferred` has no hunks. `patch_bytes` appears only on `truncated`.
- `files.length` is at most 200. When `files` is not empty, `offset + files.length` is at most `totals.files`.
- `next_offset`, when present, is greater than `offset` and at most `offset + files.length`.

### 8.5 Restore plan object

Returned by `snapshot.restore_plan`. The example plans a restore of `src` that an ignored file blocks.

```json
{
  "snapshot": {"id": "20261029T101500.123456789Z-1a2b3c4d5e6f", "commit": "1a2b3c4d5e6f...", "tree": "7f3e9b2a..."},
  "paths": ["src"],
  "current_tree": "9c1e7a3f...",
  "result_tree": "d2f04b91...",
  "applicable": false,
  "blockers": [],
  "unmatched": [],
  "offset": 0,
  "totals": {"entries": 3, "create": 1, "modify": 1, "type_change": 0, "delete": 1, "staged": 0, "unstaged": 1, "untracked": 1, "blocked": 1},
  "entries": [
    {"path": "src/cache.log", "action": "create", "target": {"mode": "100644", "oid": "5716ca59..."}, "blocker": "untracked_in_the_way"},
    {"path": "src/new.go", "action": "delete", "current": {"mode": "100644", "oid": "e69de29b..."}, "untracked": true},
    {"path": "src/server.go", "action": "modify", "current": {"mode": "100644", "oid": "8792505e..."}, "target": {"mode": "100644", "oid": "83db48f8..."}, "unstaged": true}
  ]
}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `snapshot` | object | `{id, commit, tree}` of the snapshot to restore. |
| `paths` | string array, optional | The scope, as requested. Absent for a full-tree plan. |
| `current_tree` | string | Full ID of the tree that the working tree captures to. |
| `result_tree` | string, optional | Full ID of the tree that the restore would produce. Absent when any entry meets the `parent_conflict` condition, even when that entry reports an earlier blocker. |
| `applicable` | boolean | Whether a restore could apply this plan (FR-15). |
| `blockers` | string array | Repository blockers, in the FR-15 order. |
| `unmatched` | string array | Scope paths that cover nothing. Empty for a full-tree plan. |
| `offset` | integer | Index of the first listed entry, as requested. |
| `next_offset` | integer, optional | Index to request next. Absent on the last page. |
| `totals` | object | `{entries, create, modify, type_change, delete, staged, unstaged, untracked, blocked}`, counted over the whole plan. |
| `entries` | array | At most 200 entries, starting at `offset`. |

Entry:

| Field | Type | Meaning |
| --- | --- | --- |
| `path` | string | Repository path, repaired as FR-15 describes when it is not valid UTF-8. |
| `action` | `"create"`, `"modify"`, `"type_change"`, or `"delete"` | What the restore would do at the path. |
| `current` | object, optional | `{mode, oid}` in the current tree. Absent for `create`. |
| `target` | object, optional | `{mode, oid}` in the snapshot tree. Absent for `delete`. For mode `160000` the ID is a submodule commit. |
| `staged`, `unstaged`, `untracked` | `true`, optional | The FR-15 flags. Each is present only when set. |
| `blocker` | string, optional | The path blocker that stops this entry. |

A client MUST reject a result that breaks any of these rules:

- `current` is absent exactly when `action` is `create`. `target` is absent exactly when `action` is `delete`.
- `unstaged` and `untracked` are never both present.
- Every value in `blockers` is a repository blocker, and every `blocker` is a path blocker, from FR-15.
- When `applicable` is true, `blockers` and `unmatched` are empty, `totals.blocked` is 0, and `result_tree` is present.
- `totals.entries` equals the sum of `create`, `modify`, `type_change`, and `delete`. Each of `staged`, `unstaged`, `untracked`, and `blocked` is at most `totals.entries`.
- `entries.length` is at most 200. When `entries` is not empty, `offset + entries.length` is at most `totals.entries`.
- `next_offset`, when present, equals `offset + entries.length` and is less than `totals.entries`.
- Every mode is six octal digits. `current_tree`, `result_tree`, `snapshot.tree`, and every `oid` are full object IDs of one length, 40 or 64 lowercase hex digits.

### 8.6 Restore result object

Returned by `snapshot.restore` (FR-16). The example is a path-scoped restore that a read-only directory stopped part way.

```json
{
  "outcome": "stopped",
  "snapshot": {"id": "20261029T101500.123456789Z-1a2b3c4d5e6f", "commit": "1a2b3c4d5e6f...", "tree": "7f3e9b2a..."},
  "backup": {"id": "20261103T090000.000000000Z-5d6e7f8a9b0c", "commit": "5d6e7f8a9b0c...", "tree": "9c1e7a3f..."},
  "paths": ["src"],
  "current_tree": "9c1e7a3f...",
  "result_tree": "d2f04b91...",
  "after_tree": "0b4c12de...",
  "error": "error: unable to unlink old 'src/zz/f': Permission denied",
  "totals": {"planned": 3, "written": 1, "failed": 1, "not_started": 1, "unplanned": 0},
  "written": ["src/new.go"],
  "failed": ["src/zz/f"],
  "not_started": ["src/zz/g"],
  "unplanned": []
}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `outcome` | `"applied"`, `"already_applied"`, `"stopped"`, or `"unknown"` | What the restore did (FR-16). |
| `snapshot` | object | `{id, commit, tree}` of the snapshot restored. |
| `backup` | object | `{id, commit, tree}` of the safety backup. |
| `paths` | string array, optional | The scope, as requested. Absent for the whole tree. |
| `current_tree`, `result_tree` | string | The bound trees, as requested. |
| `after_tree` | string, optional | The tree that the capture after the write recorded, or the capture of step 5 for `already_applied`. Absent when that capture failed. |
| `error` | string, optional | Git's error output from the write, cut to its first 4096 bytes and repaired as FR-14 repairs text. Present only for `stopped`. |
| `totals` | object | `{planned, written, failed, not_started, unplanned}`. `planned` counts the entries of the recomputed plan, and is 0 for `already_applied`. |
| `written`, `failed`, `not_started`, `unplanned` | string array | The first 100 paths of each kind, in byte order. A path outside the plan that is not valid UTF-8 is repaired as FR-15 repairs entry paths. |

The four lists hold at most 400 paths. With the path limit of 4096 bytes and the `error` limit, a result holds at most about 1.6 MiB of text before JSON escaping. `restore-plan` toward the snapshot lists what a `stopped` restore left, and `restore-plan` toward the backup lists what it changed.

A client MUST reject a result that breaks any of these rules:

- `backup.tree` equals `current_tree`.
- `after_tree` equals `result_tree` exactly when `outcome` is `applied` or `already_applied`, and is present for both.
- `error` is present exactly when `outcome` is `stopped`.
- `after_tree` is absent only when `outcome` is `unknown`.
- For `applied`, `totals.written` equals `totals.planned`, and `failed`, `not_started`, and `unplanned` are 0.
- For `already_applied`, every count in `totals` is 0.
- When `after_tree` is present, `written + failed + not_started` equals `planned`. When it is absent, `written`, `failed`, `not_started`, and `unplanned` are 0.
- Each list's length is the smaller of its total and 100. Its paths increase in byte order, and no path appears in two lists.
- `paths`, when present, holds 1 to 100 strings.
- `current_tree`, `result_tree`, `after_tree`, and the `commit` and `tree` of `snapshot` and `backup` are full object IDs of one length, 40 or 64 lowercase hex digits.

## 9. Safety and Reliability Requirements

- All Git commands run from the resolved repository and with inherited `GIT_*` variables removed. Blink sets only its own scratch `GIT_INDEX_FILE` for capture, and for a restore plan and a restore also `GIT_OBJECT_DIRECTORY`, naming the private object directory of FR-15. The one exception is the FR-14 diff, which runs in a private evidence repository whose only link to the resolved repository is a read-only `objects/info/alternates` entry.
- A restore (FR-16) is the only operation that writes working-tree files. It writes them only with `read-tree -m -u` on the scratch index that its own capture filled, after it recomputed the plan from that capture. It writes no index, ref, `HEAD`, configuration, or object outside its scratch directory, and its only file in the Git directory is its restore lock.
- Once a restore's write starts, cancellation and teardown signals are deferred until its process exits. No caller is told that nothing changed when files may have changed.
- Ref writes must never dereference symbolic refs into normal repository refs.
- A failed scratch-directory safety check must stop snapshot creation rather than create temporary state inside the worktree.
- The Node client must terminate descendant processes on Unix and Windows according to platform-specific process-tree semantics.
- Integrations must log failures with enough context to diagnose them, while avoiding a checkpoint failure from crashing or blocking the host beyond the Pi baseline barrier semantics.
- The system must behave correctly on Git SHA-1 and SHA-256 object-format repositories.
- All public protocol results must be validated at the TypeScript boundary to prevent malformed subprocess output from reaching an agent host.

## 10. Restore Release Requirements (Future)

Restore writes are excluded from the current executable and protocol. [`RESTORE.md`](RESTORE.md) is the design, and it records the Phase 5 design review. FR-15 specifies the read-only plan and the safety backup. FR-16 specifies the confirmed write, which is not built. The write may be introduced only when all of these acceptance requirements are designed and tested:

1. **Explicit selection:** restore requires an exact snapshot identifier and, for partial restore, an explicit path allow-list, as FR-15 defines them.
2. **Preflight:** Blink computes the FR-15 plan again immediately before writing, and refuses when it is not applicable or no longer matches the plan the user saw. FR-16 steps 3 to 8 do this.
3. **Preview:** the caller can inspect the FR-15 entries, flags, and blockers, and the diff from the backup to the snapshot, before confirmation.
4. **Backup:** a valid FR-15 safety backup exists before any working-tree write. A fresh capture that equals the planned current tree counts as fresh, even when deduplication reused an existing snapshot.
5. **Conflict preservation:** staged changes are kept, because a restore never writes the index. Unstaged changes and untracked files are named in the plan, held by the backup, and never overwritten or deleted without a confirmation.
6. **Atomicity:** writes use the safest practical transaction/rollback strategy. If full atomicity is impossible, the result clearly identifies completed and failed paths and offers recovery from the safety snapshot. FR-16 refuses before the first write, and the restore result object lists written, failed, and not-started paths. Recovery is a restore of the backup.
7. **Confirmation:** destructive restore requires an explicit confirmation from a person, outside lifecycle hooks and agent tools. The guarantee covers Blink's own tools, hooks, and shared transport. An agent with a shell can run Git or the CLI itself, and Blink does not claim to stop it.
8. **Auditability:** the result identifies the source snapshot, safety backup, selected paths, and final outcome. The restore result object (section 8.6) does this.
9. **Testing:** integration tests cover clean repositories, dirty staged and unstaged repositories, untracked files, deletions, path-scoped restore, merge conflicts, linked worktrees, cancellation, and mid-operation failure.

## 11. Intelligent Review Requirements (Future)

Snapshot diff is implemented as FR-14 and is the evidence layer for review.

An intelligent review layer may prioritize risky changes, summarize scope, and flag likely regressions. It must cite every conclusion with an FR-14 citation and must never replace the source evidence. This repository ships no review generator. The agent that calls the diff tool is the reviewer.

## 12. Quality Strategy

### Unit and protocol tests

- Test CLI argument parsing, protocol strictness, response shapes, default values, and error codes.
- Test snapshot creation in repositories with and without a `HEAD` commit.
- Test ignored files, untracked files, staged and unstaged changes, deletions, deduplication, ref namespace isolation, symbolic-ref refusal, and concurrent creation.
- Test lookup with empty history, malformed and path-like identifiers, identifiers from another worktree, SHA-1 and SHA-256 repositories, tampered item refs, and changed-path truncation.
- Test diff with equal and identical snapshots, renames, type changes, symbolic links, submodules, binary files, non-UTF-8 bytes, carriage returns, and missing final newlines. Test every paging rule at its edges, isolation from hostile attributes, configuration, and replace refs, SHA-1 and SHA-256 repositories, linked worktrees, and that a diff writes nothing to the repository.
- Test restore plans in every FR-15 state and blocker, path-scope validation and matching, paging and staleness, SHA-1 and SHA-256 repositories, linked worktrees, an unborn branch, a `HEAD` with CRLF content under `core.autocrlf`, sparse checkout, and cancellation. Test that a `create` right after a plan records `current_tree`, and that a plan changes no path or content in the Git directory and nothing in the working tree.
- Test scratch-directory selection and cleanup, including a temporary directory configured inside the repository.
- Test client cancellation, timeout, crash recovery, close barriers, response validation, and platform teardown decisions.
- Test host lifecycle coalescing, busy/session-deleted cancellation, shutdown flush, and manual tools/commands.

### Integration and packaging tests

- Build the Go binary and execute JSONL smoke tests against a temporary Git repository.
- Run each TypeScript package's test suite using the built Go binary.
- Verify packed npm artifacts install into a clean directory with required runtime dependencies and extension discovery topology.

### Required validation commands

```sh
test -z "$(gofmt -l .)"
go test ./...
go vet ./...
bun run build
bun run typecheck
bun run test
bun run test:node
go build -o bin/agent-snap ./cmd/agent-snap
AGENT_SNAP_BIN="$PWD/bin/agent-snap" bun run smoke
AGENT_SNAP_BIN="$PWD/bin/agent-snap" bun run pack-check
```

## 13. Delivery Milestones

### Milestone A: Safe Capture (current)

Deliver the current Go core, protocol, Node client, OpenCode adapter, and Pi extension with the safety requirements in this specification.

### Milestone B: Blink naming and distribution

Define the package/binary migration strategy, user-facing branding, compatibility aliases, documentation migration, and upgrade path from `agent-snap` to `blink` without breaking existing integrations.

### Milestone C: Review

Add snapshot selection and diff APIs, then present evidence-backed change summaries in supported agent hosts.

### Milestone D: Restore

Implement the restore safety contract in section 10, beginning with dry-run and backup-only flows before enabling confirmed writes.

### Milestone E: Lifecycle management

Add configurable retention, garbage collection, observability, and policy controls while preserving ref isolation and non-destructive defaults.

## 14. Open Decisions

These decisions need product and engineering agreement before their related milestones begin:

- Whether `refs/agent-snapshots/` becomes `refs/blink/`, remains a compatibility namespace, or is hidden behind a migration layer.
- Which host environments are first-class after OpenCode and Pi Coding Agent.
- Whether review intelligence is local-only, host-provided, or supported by an optional service.
- Retention defaults, storage quotas, and whether checkpoints may be explicitly exported or shared.
- Versioning, package names, and executable aliases for the Blink product release.
