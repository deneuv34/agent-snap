# Changelog

## Unreleased

- `SnapClient` now works in Bun hosts on Linux and macOS. In a compiled Bun
  host, `process.execPath` is the host's own CLI, so the supervisor never
  started and every request failed with `agent-snap supervisor exited
  (code=1, signal=none)`. The OpenCode CLI rejects `--eval` the same way. On
  plain Bun, every `close()` took about 6 s and ended in a SIGKILL, because
  Bun's `net.Socket` never reads the supervisor's control pipe. A host
  executable that re-runs its own entrypoint now fails closed instead of
  starting copies of itself.
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
