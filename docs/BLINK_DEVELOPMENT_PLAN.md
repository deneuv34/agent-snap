# Blink Phase-by-Phase Development Plan

**Status:** Proposed execution plan — tracked on GitHub
**Product:** Blink — Craft confidently. Review intelligently. Restore instantly.
**Current codebase name:** `agent-snap`

## GitHub tracker

This document is the source of truth for **scope**. GitHub is the source of truth for **status**.

Each phase has one milestone and one tracking issue. Work items and exit criteria live as checklists on that issue. Close the issue only when the phase exit criteria pass. Change scope here first, then update the matching issue so the two stay aligned.

| Phase | Tracking issue | Milestone |
| --- | --- | --- |
| 0 — Baseline and Release Discipline | [#3](https://github.com/deneuv34/agent-snap/issues/3) | [Phase 0](https://github.com/deneuv34/agent-snap/milestone/1) |
| 1 — Blink Identity and Backward-Compatible Distribution | [#1](https://github.com/deneuv34/agent-snap/issues/1) | [Phase 1](https://github.com/deneuv34/agent-snap/milestone/2) |
| 2 — Snapshot Discovery and Evidence APIs | [#6](https://github.com/deneuv34/agent-snap/issues/6) | [Phase 2](https://github.com/deneuv34/agent-snap/milestone/3) |
| 3 — Snapshot Diff and Intelligent Review | [#2](https://github.com/deneuv34/agent-snap/issues/2) | [Phase 3](https://github.com/deneuv34/agent-snap/milestone/4) |
| 4 — Restore Design, Dry Run, and Safety Backup | [#4](https://github.com/deneuv34/agent-snap/issues/4) | [Phase 4](https://github.com/deneuv34/agent-snap/milestone/5) |
| 5 — Confirmed Restore | [#5](https://github.com/deneuv34/agent-snap/issues/5) | [Phase 5](https://github.com/deneuv34/agent-snap/milestone/6) |
| 6 — Retention, Observability, and Operational Maturity | [#7](https://github.com/deneuv34/agent-snap/issues/7) | [Phase 6](https://github.com/deneuv34/agent-snap/milestone/7) |

Pull requests that implement a phase should mention that phase's tracking issue. Use `Fixes #N` only when the PR completes the whole phase. Filter issues with `label:type:phase` or `label:phase/N`.

## Planning Rules

- A phase ships only after its explicit exit criteria pass.
- Each phase should be delivered through focused, independently reviewable commits or pull requests; do not mix product renaming, restore writes, and process-safety changes in one change set.
- Preserve the current safety invariant throughout: capture and inspection never modify `HEAD`, the real index, normal refs, or the working tree.
- Changes that write working-tree files require a dedicated design review, adversarial tests, and explicit user confirmation UX.
- Keep `agent-snap` compatibility until a published Blink migration policy says otherwise.

## Phase 0 — Baseline and Release Discipline

**GitHub:** [#3](https://github.com/deneuv34/agent-snap/issues/3) · [milestone](https://github.com/deneuv34/agent-snap/milestone/1)

**Objective:** Establish a reproducible, trustworthy baseline before expanding product scope.

### Deliverables

- Green local verification for Go core, TypeScript packages, smoke test, and package-install test.
- A release checklist covering versioning, changelog, supported platforms, and binary/package publication ([docs/RELEASE.md](RELEASE.md), [CHANGELOG.md](../CHANGELOG.md)).
- CI that runs the same verification gates on supported platforms.
- A documented compatibility contract for the current CLI, JSONL methods, TypeScript client, OpenCode plugin, and Pi extension ([docs/COMPATIBILITY.md](COMPATIBILITY.md)).
- Operational diagnostics that do not expose repository contents ([docs/DIAGNOSTICS.md](DIAGNOSTICS.md)).

### Work items

1. Keep the required local gates in the root README and CI configuration aligned.
2. Add or verify platform coverage for POSIX process-group teardown and Windows process-tree teardown.
3. Define release artifacts: Go binary checksums, npm package contents, and a minimum supported Git/Go/Node matrix.
4. Document operational diagnostics for failed snapshots and teardown failures without exposing repository content unnecessarily.

### Exit criteria

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

All commands pass on the supported platform matrix, and package artifacts install cleanly in a fresh directory.

## Phase 1 — Blink Identity and Backward-Compatible Distribution

**GitHub:** [#1](https://github.com/deneuv34/agent-snap/issues/1) · [milestone](https://github.com/deneuv34/agent-snap/milestone/2)

**Objective:** Introduce the Blink product identity without breaking existing users of `agent-snap`.

### Deliverables

- User-facing documentation and package metadata branded as Blink.
- A written namespace migration decision for the executable, npm packages, JSONL methods, and hidden Git refs.
- Compatibility aliases or a clearly versioned migration path for `agent-snap` consumers.

### Work items

1. Decide and record whether Git refs remain `refs/agent-snapshots/` permanently, are dual-written, or migrate to a Blink namespace.
2. Decide whether `agent-snap` remains the core executable, becomes a Blink-compatible alias, or is replaced with a new `blink` executable.
3. Decide package publication names and the deprecation/upgrade policy for `@agent-snap/*` packages.
4. Update root and package READMEs after the compatibility decision; do not rename protocol methods until compatibility tests exist.
5. Add migration tests that exercise old configuration and the documented new configuration against the same snapshot store.

### Exit criteria

- Existing OpenCode and Pi configuration continues to work unchanged.
- Migration documentation has copy-pasteable install and rollback paths.
- No migration risks orphaning or silently hiding existing snapshot refs.

## Phase 2 — Snapshot Discovery and Evidence APIs

**GitHub:** [#6](https://github.com/deneuv34/agent-snap/issues/6) · [milestone](https://github.com/deneuv34/agent-snap/milestone/3)

**Objective:** Make the checkpoint timeline easy to inspect programmatically and interactively, while remaining read-only.

### Deliverables

- Stable snapshot metadata contract including source event, host/session context where safe, and schema versioning if needed.
- Read-only APIs to resolve a snapshot by ID and inspect its associated Git commit/tree.
- Agent-host UX that can list, select, and describe checkpoints succinctly.

### Work items

1. Extend `internal/snapshot` with explicit snapshot lookup APIs, keeping repository and namespace validation centralized.
2. Extend `internal/protocol` with strict, bounded read-only lookup methods and exact error contracts.
3. Add typed client methods and runtime validation in `packages/core`.
4. Add host tools/commands in `packages/opencode` and `packages/pi` only after core contract tests pass.
5. Test empty history, invalid IDs, a snapshot from another worktree, SHA-1/SHA-256 repositories, and malformed protocol responses.

### Exit criteria

- A caller can identify and inspect a checkpoint without manually constructing hidden ref paths.
- Read-only operations preserve the current no-impact safety invariant.
- Public result schemas are documented and runtime-validated.

## Phase 3 — Snapshot Diff and Intelligent Review

**GitHub:** [#2](https://github.com/deneuv34/agent-snap/issues/2) · [milestone](https://github.com/deneuv34/agent-snap/milestone/4)

**Objective:** Let developers review evidence-backed changes between checkpoints before considering restoration.

### Deliverables

- `snapshot.diff` API for two explicit checkpoints in the same repository/worktree namespace.
- Structured and human-readable diff summaries.
- Agent-host review tools that present changed paths, additions, deletions, binary-file handling, and content patches or references.

### Work items

1. Define the diff request/result schema, output size limits, pagination/truncation semantics, and binary-file representation in `docs/BLINK_SPEC.md` before implementation.
2. Implement read-only diff execution in the Go core with no working-tree writes.
3. Add protocol validation, TypeScript types, client response validation, and fixture-based tests.
4. Build host rendering that separates the deterministic Git evidence from optional generated review summaries.
5. If adding AI review, require every finding to cite a path and hunk/range from the underlying snapshot diff; never present an inference as a confirmed fact.

### Exit criteria

- Diffs are deterministic for the same snapshot pair.
- Every review conclusion is traceable to a source diff.
- Diff operations do not change repository state and are safe to invoke from an agent tool.

## Phase 4 — Restore Design, Dry Run, and Safety Backup

**GitHub:** [#4](https://github.com/deneuv34/agent-snap/issues/4) · [milestone](https://github.com/deneuv34/agent-snap/milestone/5)

**Objective:** Design and validate restore behaviour before Blink is allowed to write user files.

### Deliverables

- A reviewed restore design covering full-tree and path-scoped operations.
- A `snapshot.restore_plan` dry-run API (FR-15) that computes writes, deletions, conflicts, and preservation requirements without changing the repository.
- A mandatory safety-snapshot design that captures the current state before every confirmed restore.

### Work items

1. Write an ADR for restore semantics: staged versus unstaged preservation, untracked file treatment, ignored-file handling, conflict policy, rollback, and exit status ([docs/RESTORE.md](RESTORE.md)).
2. Add a dry-run protocol method with exact target selection and explicit path allow-list semantics.
3. Define an explicit confirmation token/capability model that lifecycle hooks and unattended agent prompts cannot manufacture.
4. Implement backup-only flow and verify it uses the same scratch-index safety properties as normal capture.
5. Add adversarial integration tests for dirty repositories, deleted paths, renames, nested worktrees, cancellation, and failures between backup and write.

### Exit criteria

- Dry run provides enough evidence for a user to understand every planned write.
- A restore safety backup is created and returned before the restore confirmation is accepted.
- The design has an approved response for every ambiguous or conflicting repository state; unsupported states fail safely.

## Phase 5 — Confirmed Restore

**GitHub:** [#5](https://github.com/deneuv34/agent-snap/issues/5) · [milestone](https://github.com/deneuv34/agent-snap/milestone/6)

**Objective:** Restore selected snapshot content only after an explicit, informed confirmation.

### Deliverables

- Confirmed full-tree and path-scoped restore operations.
- Clear final result describing source snapshot, safety backup, modified paths, conflicts, rollback outcome, and any manual follow-up.
- Host-specific confirmation UX that cannot be triggered by normal automatic lifecycle events.

### Work items

1. Implement restore writes through a narrowly scoped core API; do not expose raw Git commands directly through host plugins.
2. Enforce the validated dry-run plan at the protocol boundary, and the person's confirmation in the hosts and the CLI ([docs/RESTORE.md](RESTORE.md)).
3. Apply the documented preservation and conflict rules; refuse rather than overwrite anything outside the approved plan.
4. Provide an abort and recovery path that references the newly created safety snapshot.
5. Add end-to-end tests that prove no unrelated files, index entries, refs, or worktrees are modified.

### Exit criteria

- No restore path can execute without an explicit confirmation and a verified safety backup.
- Integration tests cover success, cancellation, conflict, partial failure, and recovery.
- The operation has a documented, tested rollback/recovery story.

## Phase 6 — Retention, Observability, and Operational Maturity

**GitHub:** [#7](https://github.com/deneuv34/agent-snap/issues/7) · [milestone](https://github.com/deneuv34/agent-snap/milestone/7)

**Objective:** Make Blink maintainable for long-lived repositories and teams.

### Deliverables

- Configurable retention and garbage collection for hidden snapshot refs.
- Structured diagnostics/metrics for snapshot latency, deduplication, failures, restore outcomes, and cleanup failures.
- Documented storage and privacy behaviour.

### Work items

1. Define retention policy inputs: age, count, storage size, per-worktree handling, and protected/manual checkpoints.
2. Implement a dry-run garbage-collection report before any ref deletion capability.
3. Add a confirmed cleanup command/API with explicit deletion reporting.
4. Add privacy-preserving observability that records operation metadata, not source contents or secrets.
5. Define support runbooks for stuck child processes, corrupt refs, failed cleanup, and large repositories.

### Exit criteria

- Retention cannot delete a protected checkpoint or a ref outside Blink's namespace.
- Operators can diagnose failures without accessing developer source code.
- Storage behaviour and cleanup safety are documented and tested.

## Backlog Triage

Prioritize work in this order:

1. Safety or data-loss risk.
2. Reliability and cross-platform correctness.
3. Evidence and review quality.
4. Explicit restore capability.
5. Branding, packaging, and operational enhancements.

Every proposed feature must name the affected phase, safety impact, compatibility impact, API contract, test strategy, and release/rollback plan before implementation begins.
