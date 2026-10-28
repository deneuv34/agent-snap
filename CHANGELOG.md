# Changelog

## Unreleased

- Read-only snapshot diff (Phase 3): JSONL `snapshot.diff`, CLI
  `agent-snap diff <from> <to>`, `SnapClient.diff`, the OpenCode and Pi
  `agent_snap_diff` tools, and the Pi `agent-snap-diff` command. A diff pages
  through the changed files and hunks between two snapshots of one worktree,
  at most 200 files and 64 KiB of hunks per page by default (`offset`,
  `max_patch_bytes` from 1 KiB to 1 MiB). It runs in a private scratch
  repository that reads the real objects, so Git attributes and configuration
  cannot hide or reshape a change, and the same pair gives the same result
  with the same Git.
- `SnapLike` gains a required `diff` method. Custom `SnapLike`
  implementations must add it.
- Read-only snapshot lookup (Phase 2): JSONL `snapshot.get`, CLI
  `agent-snap get <id>`, `SnapClient.get`, the OpenCode and Pi
  `agent_snap_get` tools, and the Pi `agent-snap-get` command. Lookup
  returns the snapshot detail object (commit, tree, parent, parent
  snapshot id, changed files capped at 1000) and adds error code
  `-32001` for a snapshot that does not exist in the worktree.
- `SnapLike` gains a required `get` method. Custom `SnapLike`
  implementations must add it.
- Snapshot origin (Phase 2): `snapshot.create` takes optional `source` and
  `host`, stored as `Agent-Snap-Source` / `Agent-Snap-Host` lines at the end of
  the snapshot commit message and returned by create, list, and get. The CLI
  records `source: cli`. OpenCode and Pi record their host and lifecycle event,
  `tool`, or `command`. No session identifiers are stored.
- `SnapLike.create` and `SnapClient.create` take an optional fourth `origin`
  argument. `SnapClient` retries without origin when a 0.1.0 binary rejects
  it, so upgrading a plugin before its binary does not stop snapshots.
- `snapshot.create` now rejects, with `-32602`, a reason that contains a NUL
  byte or ends with a paragraph of `Agent-Snap-` lines. Git always refused NUL
  bytes, so 0.1.0 failed such a create with `-32603`, or silently reused the
  latest snapshot when the tree was unchanged. 0.1.0 stored a reason ending in
  `Agent-Snap-` lines as given; this release reads such a snapshot back with
  an origin and a shorter reason.

## 0.1.0

Initial baseline.

- Go core: `create`, `list`, `ping`, `version`, JSONL `serve`
- Hidden refs under `refs/agent-snapshots/`
- TypeScript client `@agent-snap/core` with runtime validation and
  fail-closed POSIX / Windows teardown
- OpenCode plugin and Pi Coding Agent extension
- Compatibility contract, release checklist, and diagnostics guide
