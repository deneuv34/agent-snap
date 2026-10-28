# Changelog

## Unreleased

- Read-only snapshot lookup (Phase 2): JSONL `snapshot.get`, CLI
  `agent-snap get <id>`, `SnapClient.get`, the OpenCode and Pi
  `agent_snap_get` tools, and the Pi `agent-snap-get` command. Lookup
  returns the snapshot detail object (commit, tree, parent, parent
  snapshot id, changed files capped at 1000) and adds error code
  `-32001` for a snapshot that does not exist in the worktree.
- `SnapLike` gains a required `get` method. Custom `SnapLike`
  implementations must add it.

## 0.1.0

Initial baseline.

- Go core: `create`, `list`, `ping`, `version`, JSONL `serve`
- Hidden refs under `refs/agent-snapshots/`
- TypeScript client `@agent-snap/core` with runtime validation and
  fail-closed POSIX / Windows teardown
- OpenCode plugin and Pi Coding Agent extension
- Compatibility contract, release checklist, and diagnostics guide
