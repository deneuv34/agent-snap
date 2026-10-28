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
- Expose a CLI and a long-running JSONL protocol server.
- Provide a type-safe TypeScript client for the Go core.
- Integrate automatic and manual snapshot creation with OpenCode and Pi Coding Agent.
- Deduplicate snapshots when the resulting working-tree tree is unchanged.
- Isolate refs per Git worktree and update the latest pointer atomically.

### 4.2 Planned capabilities (not implemented)

- Compare any two snapshots and review their file-level and content-level differences.
- Restore all or selected paths from a snapshot.
- Preview, confirm, and back up a restore operation.
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
4. The caller can use the returned commit and tree IDs with normal read-only Git inspection commands where appropriate.

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
- New snapshot commits MUST use the latest Blink snapshot as their parent when one exists; otherwise, they MAY use the current `HEAD` as parent.
- The operation MUST return the snapshot object defined in section 8.

### FR-3: No-impact capture safety

- Snapshot creation MUST use a private temporary `GIT_INDEX_FILE`.
- The temporary directory MUST be outside the target repository, including after symlink resolution, so Blink's own scratch files cannot be captured.
- Scratch files MUST be removed when the operation completes or fails.
- Snapshot creation MUST NOT write the real Git index, modify `HEAD`, update user branches, or modify working-tree files.

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
- The resulting snapshot-parent chain MUST remain linear per worktree.

### FR-7: Snapshot listing

- Listing MUST return an empty list when no snapshots exist.
- The default list limit is 20.
- JSONL callers may request an integer limit from 1 through 100; the CLI requires a positive integer.
- Results MUST be sorted newest first by snapshot identifier.
- Listed entries include identifier, commit, reason, and creation time. `changed_files` and `reused` are creation-result metadata and are not guaranteed on list results.

### FR-8: Core CLI

The current CLI name is `agent-snap`; Blink branding is a future packaging decision.

| Command | Required behaviour |
| --- | --- |
| `create [reason] [--cwd <dir>]` | Create one snapshot and print JSON. `--reason` and positional reason are mutually exclusive. |
| `list [--cwd <dir>] [--limit <n>]` | List snapshots and print JSON. |
| `get <id> [--cwd <dir>]` | Print the snapshot detail object for one snapshot as JSON. |
| `ping` | Print protocol status and version JSON. |
| `version` | Print the version string. |
| `serve` | Process newline-delimited JSON requests on standard input and emit one JSON response per request on standard output. |

### FR-9: JSONL protocol

- Each request and response is one JSON object per line.
- Empty input lines are ignored.
- The server MUST continue to process later requests after a request-level parse, validation, or operation error.
- The maximum input line size is 4 MiB.
- Supported methods are `ping`, `snapshot.create`, `snapshot.list`, and `snapshot.get`.
- Unknown methods return code `-32601`.
- Malformed JSON returns `-32700`; invalid parameters return `-32602`; internal operation failures return `-32603`; a well-formed snapshot identifier with no snapshot in the worktree namespace returns `-32001`.
- Parameter validation MUST reject unknown keys, missing `cwd`, blank `cwd`, malformed types, invalid list limits, and malformed snapshot identifiers.

### FR-10: TypeScript client

- `SnapClient` launches `agent-snap serve` lazily and reuses one transport while healthy.
- `AGENT_SNAP_BIN` overrides the executable path; the fallback is `agent-snap` on `PATH`.
- The client MUST runtime-validate successful and error responses before exposing them to integrations.
- Requests support `AbortSignal` and a default 30-second timeout.
- Timeout or cancellation MUST start teardown of the spawned process tree, preventing a timed-out request from later producing an unobserved checkpoint.
- After a crash or close, the client MAY spawn a fresh transport lazily for a subsequent request.
- `close()` MUST prevent a replacement transport from being created until close completes.

### FR-11: Agent-host integrations

#### OpenCode

- Create a baseline snapshot for `session.created`; failures are logged and must not become unhandled rejections.
- Create a coalesced snapshot after `session.idle` using a 1.5-second window.
- A transition to busy cancels pending idle work and aborts idle work already in flight.
- `session.deleted` cancels work for that session and removes session state.
- During disposal, reject new events, flush pending snapshots, abort remaining work, and close the core client.
- Expose `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`, and `agent_snap_get(id, cwd?)` with caller cancellation support.

#### Pi Coding Agent

- At `session_start`, create a `session-start` baseline and await it as a session-start barrier. Failure is logged rather than propagated.
- At `agent_settled`, create an `agent-settled` snapshot through the same 1.5-second coalescing policy.
- At `session_shutdown`, flush queued work, dispose the scheduler, await tracked baselines, and close the client.
- Expose `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`, `agent_snap_get(id, cwd?)`, `agent-snap-create`, `agent-snap-list`, and `agent-snap-get <id>`.

### FR-12: Snapshot lookup

- Lookup takes `cwd` and a snapshot identifier, and resolves the identifier only inside the worktree namespace derived from `cwd`. Identifiers from another worktree are not found.
- An identifier MUST match `<YYYYMMDD>T<HHMMSS>.<nanoseconds>Z-<12 lowercase hex digits>` before any ref path is built from it.
- Lookup MUST fail closed when the item ref is symbolic, does not point at a commit, points at a commit with more than one parent, points at a commit whose ID does not begin with the identifier's 12-digit suffix, or points at a commit without the `agent-snap: ` message prefix and the fixed product author and committer identity. The same rules decide whether a parent is reported as a snapshot (`parent_id`).
- Lookup MUST read objects with Git replace objects disabled, and MUST parse only Git's standard output, never its diagnostics. Unparseable Git output is an internal error, not "not found".
- Lookup MUST return the snapshot detail object defined in section 8.2. `changed_files` lists paths changed between the snapshot and its parent (or every path when there is no parent), is capped at 1000 entries, and sets `changed_files_truncated: true` when more than 1000 paths changed.
- Lookup MUST NOT write the index, refs, objects, `HEAD`, or working-tree files.

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
| `internal/snapshot` | Repository discovery, scratch-index snapshot construction, hidden-ref persistence, deduplication, listing, and lookup. |
| `packages/core` | Typed Node client, response validation, process lifecycle management, cancellation, and teardown. |
| `packages/opencode` | OpenCode lifecycle integration and manual tools. |
| `packages/pi` | Pi lifecycle integration, manual tools, and commands. |

### 7.2 Storage model

A Blink snapshot is a Git commit containing the snapshot tree. It is intentionally not attached to a normal branch. Its item ref preserves discoverability and its worktree-specific `latest` ref establishes the checkpoint chain.

The commit message is `agent-snap: <reason>`. Commit author and committer identity are fixed to the local product identity (`agent-snap <agent-snap@localhost>` in the current implementation), and timestamps are in UTC.

## 8. Data Contracts

### 8.1 Snapshot object

```json
{
  "id": "20260814T120000.000000000Z-abc123def456",
  "commit": "abc123def456...",
  "reason": "before-refactor",
  "created_at": "2026-08-14T12:00:00Z",
  "changed_files": ["src/main.go"],
  "reused": false
}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `id` | string | Timestamp plus the first 12 characters of the commit ID; unique checkpoint reference within its worktree namespace. |
| `commit` | string | Full Git commit object ID. |
| `reason` | string | Caller-supplied or default checkpoint reason. |
| `created_at` | RFC 3339 timestamp | Creation time of the snapshot. |
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
| `tree` | string | Full Git tree object ID of the snapshot. |
| `parent` | string, optional | Full ID of the parent commit: the previous checkpoint, or `HEAD` when the snapshot started the chain. Absent when the snapshot has no parent. |
| `parent_id` | string, optional | Snapshot identifier of the parent when the parent is a snapshot in the same worktree namespace. |
| `changed_files` | string array | Paths changed between the snapshot and its parent, or every path when there is no parent. Always present; at most 1000 entries. |
| `changed_files_truncated` | boolean, optional | `true` when more than 1000 paths changed and `changed_files` holds only the first 1000. |

### 8.3 Request and response examples

Create:

```json
{"id":1,"method":"snapshot.create","params":{"cwd":"/path/to/repo","reason":"before refactor"}}
```

List:

```json
{"id":2,"method":"snapshot.list","params":{"cwd":"/path/to/repo","limit":20}}
```

Get:

```json
{"id":3,"method":"snapshot.get","params":{"cwd":"/path/to/repo","id":"20260814T120000.000000000Z-abc123def456"}}
```

Success responses contain `id` and `result`. Failure responses contain `id` and `error`:

```json
{"id":2,"error":{"code":-32602,"message":"param must be an integer between 1 and 100: limit"}}
```

## 9. Safety and Reliability Requirements

- All Git commands run from the resolved repository and with inherited `GIT_*` variables removed.
- Ref writes must never dereference symbolic refs into normal repository refs.
- A failed scratch-directory safety check must stop snapshot creation rather than create temporary state inside the worktree.
- The Node client must terminate descendant processes on Unix and Windows according to platform-specific process-tree semantics.
- Integrations must log failures with enough context to diagnose them, while avoiding a checkpoint failure from crashing or blocking the host beyond the Pi baseline barrier semantics.
- The system must behave correctly on Git SHA-1 and SHA-256 object-format repositories.
- All public protocol results must be validated at the TypeScript boundary to prevent malformed subprocess output from reaching an agent host.

## 10. Restore Release Requirements (Future)

Restore is intentionally excluded from the current executable and protocol. It may be introduced only when all of these acceptance requirements are designed and tested:

1. **Explicit selection:** restore requires an exact snapshot ID or commit and, for partial restore, an explicit path set.
2. **Preflight:** Blink determines the target repository, validates the snapshot ref/commit, checks repository state, and computes a dry-run plan before writing.
3. **Preview:** the caller can inspect changed, added, deleted, and conflicting paths before confirmation.
4. **Backup:** Blink captures the pre-restore state in a fresh safety snapshot before any working-tree write.
5. **Conflict preservation:** staged and unstaged changes are detected and preserved or the operation refuses with actionable guidance; it must never silently overwrite them.
6. **Atomicity:** writes use the safest practical transaction/rollback strategy. If full atomicity is impossible, the result clearly identifies completed and failed paths and offers recovery from the safety snapshot.
7. **Confirmation:** destructive restore requires an explicit confirmation outside unattended lifecycle hooks.
8. **Auditability:** the result identifies the source snapshot, safety backup, selected paths, and final outcome.
9. **Testing:** integration tests cover clean repositories, dirty staged and unstaged repositories, untracked files, deletions, path-scoped restore, merge conflicts, linked worktrees, cancellation, and mid-operation failure.

## 11. Diff and Intelligent Review Requirements (Future)

A future `snapshot.diff` capability should:

- accept two explicit snapshot identifiers or commits within the same resolved worktree namespace;
- produce a deterministic file-level summary and patch/content view;
- distinguish additions, modifications, deletions, renames where Git detects them, and binary files;
- support structured output for agent consumers and concise human-readable output for interactive clients;
- never write repository state;
- form the evidence layer for Blink's future intelligent-review experience.

An intelligent review layer may prioritize risky changes, summarize scope, and flag likely regressions, but it must always link conclusions to the underlying snapshot diff rather than replace source evidence.

## 12. Quality Strategy

### Unit and protocol tests

- Test CLI argument parsing, protocol strictness, response shapes, default values, and error codes.
- Test snapshot creation in repositories with and without a `HEAD` commit.
- Test ignored files, untracked files, staged and unstaged changes, deletions, deduplication, ref namespace isolation, symbolic-ref refusal, and concurrent creation.
- Test lookup with empty history, malformed and path-like identifiers, identifiers from another worktree, SHA-1 and SHA-256 repositories, tampered item refs, and changed-path truncation.
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
npm run build
npm run typecheck
npm test
go build -o bin/agent-snap ./cmd/agent-snap
AGENT_SNAP_BIN="$PWD/bin/agent-snap" npm run smoke
AGENT_SNAP_BIN="$PWD/bin/agent-snap" npm run pack-check
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
- How a restore confirmation is represented in each agent host and CLI without enabling prompt-driven destructive writes.
- Whether review intelligence is local-only, host-provided, or supported by an optional service.
- Retention defaults, storage quotas, and whether checkpoints may be explicitly exported or shared.
- Versioning, package names, and executable aliases for the Blink product release.
