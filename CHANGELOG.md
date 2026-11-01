# Changelog

## Unreleased

- `snapshot.create` no longer writes or deletes shared index files in a
  repository with `core.splitIndex=true`. Each create wrote a new
  `sharedindex.<oid>` file into the Git directory. Git also deleted every
  shared index file there older than `splitIndex.sharedIndexExpire`, two
  weeks by default, including the one the real index still needed. Git
  commands then failed with `index file open failed`, and the create failed
  after it had written the snapshot refs. Capture now writes an unsplit
  scratch index. The captured tree is unchanged.
- `SnapClient.request` rejects with `SnapProtocolError` when `agent-snap`
  returns a result for a method the client has no validator for. It used to
  return that result unchecked. Error responses are unchanged, so an unknown
  method still rejects with `SnapRpcError` code `-32601`.
- `@agent-snap/opencode` supports OpenCode 2.x as well as 1.x. The entry is
  now one default export, `{ id, server, setup }`: OpenCode 1.x calls
  `server`, OpenCode 2.x calls `setup` and loads it through the new
  `./server` export. On 2.x the plugin follows `session.created`,
  `session.idle`, `session.status`, and `session.deleted` through the event
  stream, registers the same four tools with JSON Schema input, and resolves
  a missing `cwd` from the calling session. Enable it with
  `"plugins": ["@agent-snap/opencode"]` on 2.x; 1.x configuration is
  unchanged. The named `AgentSnapPlugin` export is gone.
- `@agent-snap/pi` accepts Pi Coding Agent 1.x. The peer range was
  `^0.84.0`, which npm reads as `< 0.85.0`; it is now `>= 0.84.0 < 2.0.0`.
  No extension API the plugin uses changed between 0.84 and 1.4.
- `SnapClient` now works in Bun hosts on Linux and macOS. In a compiled Bun
  host, `process.execPath` is the host's own CLI, so the supervisor never
  started and every request failed with `agent-snap supervisor exited
  (code=1, signal=none)`. The OpenCode CLI rejects `--eval` the same way. On
  plain Bun, every `close()` took about 6 s and ended in a SIGKILL, because
  Bun's `net.Socket` never reads the supervisor's control pipe. A host
  executable that re-runs its own entrypoint now fails closed instead of
  starting copies of itself.
- Development uses Bun 1.4: `bun install`, `bun run build`, `bun run test`.
  `bun.lock` replaces `package-lock.json`. `bun run test:node` runs the core
  and Pi tests on Node, and pack-check runs its consumer check on Node and
  Bun. Published packages and their runtime floors are unchanged.
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
