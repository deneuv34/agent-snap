# Changelog

## Unreleased

- The OpenCode 1.x, OpenCode 2.x, and Pi hosts now quote file names in tool
  text. A path, `old_path`, scope path, unmatched path, changed file, snapshot
  reason, or origin value that holds a control character, a double quote, or a
  backslash prints in double quotes with C escapes, as `git diff` does with
  `core.quotePath=false`. Before, a file named `evil\napplicable: yes` printed
  as two lines in `agent_snap_restore_plan` and `agent_snap_diff` text, and an
  agent could read the second line as a plan or diff fact. Every other name
  prints unchanged, including spaces and non-ASCII text. The JSON results and
  the `details` payloads are unchanged.
- `agent-snap` now runs every Git command with `LANGUAGE=C`, so Git prints
  the messages that the store matches in English. Before, under a locale such
  as `de_DE.UTF-8` and a Git built with translations, a held ref lock failed
  with Git's translated text instead of the named lock error. The store did
  not retry a failed object write. In a reftable repository, a lost
  compare-and-swap failed the create instead of retrying it. `LC_ALL`,
  `LC_CTYPE`, and `LANG` stay the caller's, so a filter sees the same
  character encoding as before.
- `snapshot.list` skips an item ref whose name is not a snapshot identifier.
  A create that reuses the latest snapshot reports only a well-formed
  identifier. Before, a ref planted under `refs/agent-snapshots/` with U+2028
  or NEL in its name listed under that name, and the hosts printed it raw in
  list text. A skipped ref does not count toward `limit`. The TypeScript
  client now rejects a create, list, get, diff, or restore plan result whose
  snapshot identifier breaks the FR-12 syntax. The `SnapProtocolError` names
  the field. A list validation error now starts with `snapshot.list result`
  instead of `snapshot.create result`. Listing and a reused create now parse
  only Git's standard output. Before, an item ref whose name held a tab made
  Git print a warning, and the warning text became the start of a real
  snapshot's identifier in list and reused create results.
- The OpenCode 1.x, OpenCode 2.x, and Pi hosts add the
  `agent_snap_restore_plan(id, paths?, offset?, current_tree?, cwd?)` tool. It
  returns one page of the restore plan as text. It says whether the plan is
  applicable, lists the blockers and the entries, and names the `offset` and
  `current_tree` to request next. It is read-only, and this release cannot
  apply a restore. Pi has no command for it. Against a binary that predates
  `snapshot.restore_plan`, the tool fails with
  `SnapRestorePlanUnsupportedError`.
- `snapshot.PlanRestore` refuses a `PlanQuery` whose `Scope` is the zero value
  with `snapshot.ErrNoScope`. A caller that wants every path passes
  `snapshot.WholeTree()`. The JSONL behavior of `snapshot.restore_plan` is
  unchanged.
- The `agent-snap` binary answers `snapshot.restore_plan` with a read-only
  restore plan (FR-15). It lists what restoring a snapshot would change and
  what would block it, and it writes nothing to the repository. A request
  that carries a `current_tree` the working tree no longer captures to fails
  with the new error code `-32002`.
- `SnapClient.restorePlan(cwd, id, options?, signal?)` sends
  `snapshot.restore_plan` and returns the validated restore plan. It rejects a
  plan that breaks the section 8.5 rules with `SnapProtocolError`. When the
  installed `agent-snap` answers `-32601`, it rejects with the new
  `SnapRestorePlanUnsupportedError`, which extends `SnapRpcError`. `SnapLike`
  gains `restorePlan`.
- `agent-snap restore-plan <id> [--cwd <dir>] [--path <path>]... [--offset <n>]
  [--current-tree <oid>]` prints one page of the restore plan as JSON. It
  exits 0 when it prints a plan, including one that is not applicable, and 1 on
  any error, including a stale `--current-tree`. It writes nothing.
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
- `agent-snap` removes the scratch directory of a request that is cancelled
  by SIGTERM or SIGINT. Before, the binary died at once on the teardown
  signal that `SnapClient` sends on timeout, abort, and `close()`. Every
  aborted create left `agent-snap-index-*/index` in the temporary directory.
  Every aborted restore plan also left `evidence.git`, whose private object
  directory can hold blobs of working-tree files. The binary now cancels the
  request in flight, stops its Git children, removes the directory, and
  exits. SIGKILL and Windows `taskkill /F` still run no cleanup. See
  docs/DIAGNOSTICS.md.
- `snapshot.create` fails within about a second when a snapshot ref lock
  stays held. Before, a `latest.lock` or item lock that no process held made
  every later create retry until the client timed out. Git leaves such a lock
  when it dies inside its ref transaction, for example after `kill -9`. The
  `-32603` error names the lock file by its path in the Git directory and says
  to remove it only when no `agent-snap` or Git process is running. Blink
  never removes the lock itself. Creates that contend with each other still
  succeed. See docs/DIAGNOSTICS.md.
- `snapshot.create` retries a snapshot ref directory that a concurrent
  `git pack-refs --all --prune` removed. `git gc` and `git maintenance` run
  that command. Since the stale-lock fix above, such a create failed with
  `update refs: … unable to create directory for`. Blink now retries that one
  failure, for at most 5 attempts with 150 ms of waits in all. A permission
  failure prints the same message and still fails the create after the
  fifth attempt. Every other ref lock failure apart from a held lock still fails at once.
- `snapshot.create` and `snapshot.restore_plan` run no repository hook and
  no fsmonitor program. Before, a create ran the repository's
  `post-index-change` and `reference-transaction` hooks, each hook that
  configuration defines with `hook.<name>.command`, and the `core.fsmonitor`
  program. Each hook received Blink's scratch index. A `post-index-change`
  hook that printed text made the create fail, because the text became part
  of the tree ID. `snapshot.get` also ran the fsmonitor program. Every Git
  command now passes `core.fsmonitor=`. Every command of a create or a plan
  also passes `core.hooksPath` set to an empty directory in its scratch
  directory, and `hook.<name>.enabled=false` for each configured hook. A
  hook whose name contains `=` cannot be turned off that way, so a create
  or a plan in that repository fails with `-32603` and names the hook. The
  captured tree is unchanged.
- `agent-snap` now reads every object ID, ref record, and path from Git's
  standard output alone. Before, the store read the tree ID, the commit ID,
  and the `HEAD` and latest snapshot IDs from Git's standard output and
  standard error together. A clean filter that printed to standard error on
  success failed the create with
  `git commit-tree "<filter text>\n<oid>" ...: not a valid object name` when
  a file was modified in the same second as the scratch index or had a
  modification time in the future. A Git failure message still carries
  Git's standard error. A configured hook listing that is not
  `hook.<name>.<key>` now fails the create or the plan with `-32603`.
  Before, it ended the process with a Go panic. The captured tree is unchanged.

## 0.1.0

Initial baseline.

- Go core: `create`, `list`, `ping`, `version`, JSONL `serve`
- Hidden refs under `refs/agent-snapshots/`
- TypeScript client `@agent-snap/core` with runtime validation and
  fail-closed POSIX / Windows teardown
- OpenCode plugin and Pi Coding Agent extension
- Compatibility contract, release checklist, and diagnostics guide
