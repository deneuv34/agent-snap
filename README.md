# Blink

Zero-impact Git working-tree snapshots for AI agent sessions.

Agent sessions mutate your working tree relentlessly. Blink records
snapshots of the working tree at key moments — session start (baseline) and
when the agent settles to idle — without ever touching `HEAD`, the real index,
or the working tree. Snapshots live in hidden git refs
(`refs/agent-snapshots/*`), so they survive `git gc`, never appear in `git
branch`, and never collide with your real history.

Blink ships as the `agent-snap` executable and the `@agent-snap/core`,
`@agent-snap/opencode`, and `@agent-snap/pi` packages. Those names, the JSONL
methods, and the `refs/agent-snapshots/` refs stay as they are, so an existing
configuration keeps working. [docs/NAMESPACE.md](docs/NAMESPACE.md) records
that decision, with install and rollback steps.

## How it works

1. A Go core (`cmd/agent-snap`) exposes `create` / `list` / `get` / `diff` / `restore-plan` / `ping` as
   direct JSON commands and a long-running JSONL `serve` mode on stdin/stdout.
2. Thin TypeScript adapters launch and reuse the Go subprocess:
   - `@agent-snap/opencode` — an OpenCode plugin (baseline at
     `session.created`, coalesced snapshot at `session.idle`, plus
     `agent_snap_create` / `agent_snap_list` / `agent_snap_get` /
     `agent_snap_diff` / `agent_snap_restore_plan` tools).
   - `@agent-snap/pi` — a Pi Coding Agent extension (baseline at
     `session_start`, coalesced snapshot at `agent_settled`, cleanup at
     `session_shutdown`, plus `agent_snap_create` / `agent_snap_list` /
     `agent_snap_get` / `agent_snap_diff` / `agent_snap_restore_plan` tools and
     `agent-snap-create` / `agent-snap-list` / `agent-snap-get` /
     `agent-snap-diff` commands).
3. Every create builds a tree from a **temporary index**
   (`GIT_INDEX_FILE` pointing at a scratch file), so the real index is never
   written. The commit is created with `git commit-tree` against hidden refs —
   `HEAD` never moves.
4. Unchanged trees are deduplicated: a create whose tree matches the latest
   snapshot simply reuses it (`reused: true`) instead of minting a commit.
5. Snapshot refs are namespaced per worktree (derived from the resolved git
   dir), so linked worktrees never collide; `latest` moves through an atomic
   `update-ref` compare-and-swap transaction, keeping the snapshot chain
   linear under concurrent creates.

## Requirements

- Go 1.26+
- git 2.30+
- To use the adapters: Pi runs them on Node.js 22.19+ (the same floor as Pi
  Coding Agent), and OpenCode runs them on its bundled Bun.
- To develop the adapters: Bun 1.4+, plus Node.js 22.19+ for the Node test
  run and pack-check.

Supported CI matrix, release artifacts, and publication steps:
[docs/RELEASE.md](docs/RELEASE.md). Frozen CLI / JSONL / package contract:
[docs/COMPATIBILITY.md](docs/COMPATIBILITY.md). Failure diagnosis without
repository contents: [docs/DIAGNOSTICS.md](docs/DIAGNOSTICS.md).

## Install

```sh
# Go core
go install ./cmd/agent-snap

# TypeScript adapters (optional)
bun install
bun run build
```

## CLI

```sh
agent-snap create "before big refactor" --cwd /path/to/repo   # prints snapshot JSON
agent-snap list --cwd /path/to/repo --limit 10                # prints {"snapshots": [...]}
agent-snap diff <from-id> <to-id> --cwd /path/to/repo         # prints one diff page
agent-snap restore-plan <id> --cwd /path/to/repo              # prints one restore plan page, writes nothing
agent-snap ping                                               # {"status":"ok","version":...}
agent-snap version
agent-snap serve                                              # JSONL server on stdin/stdout
```

## JSONL wire protocol

`agent-snap serve` reads JSON requests, one per line, from stdin and writes
one JSON response per line to stdout.

Request:

```json
{"id":1,"method":"snapshot.create","params":{"cwd":"/path/to/repo","reason":"why","source":"tool","host":"pi"}}
```

Response (success):

```json
{"id":1,"result":{"id":"20260814T120000.000000000Z-abc123def456","commit":"abc123def456","reason":"why","created_at":"2026-08-14T12:00:00Z","source":"tool","host":"pi","changed_files":["src/main.go"],"reused":false}}
```

Response (error — processing continues with the next request):

```json
{"id":1,"error":{"code":-32601,"message":"method not found"}}
```

Methods:

| Method           | Params                        | Result                                   |
| ---------------- | ----------------------------- | ---------------------------------------- |
| `ping`           | —                             | `{"status":"ok","version":...}`          |
| `snapshot.create`| `cwd`, `reason?`, `source?`, `host?` | snapshot object (see above)       |
| `snapshot.list`  | `cwd`, `limit?`               | `{"snapshots":[...]}` (newest first)     |
| `snapshot.get`   | `cwd`, `id`                   | snapshot detail object                   |
| `snapshot.diff`  | `cwd`, `from`, `to`, `offset?`, `max_patch_bytes?` | one diff page            |
| `snapshot.restore_plan` | `cwd`, `id`, `paths?`, `offset?`, `current_tree?` | one restore plan page |

Error codes: `-32700` parse error, `-32601` method not found,
`-32602` invalid params, `-32603` internal error, `-32001` snapshot not found,
`-32002` stale restore plan.

`source` and `host` are the snapshot's origin: what asked for it, and which
integration sent the request. Each is a lowercase hyphenated token of at most
64 characters. They are stored in the snapshot commit and returned by create,
list, and get when present. The CLI records `source: cli`; the OpenCode and Pi
integrations record their own host and event. Origin is reported by the
caller, so treat it as a label, not evidence. No session identifiers are
stored. See FR-13 in [`docs/BLINK_SPEC.md`](docs/BLINK_SPEC.md).

`snapshot.diff` returns the changed files and hunks between two snapshots of
the same worktree, one page at a time: at most 200 files and 64 KiB of hunks
by default. Each file says whether its hunks are on the page (`full`,
`truncated`, `deferred`, or `binary`), and `next_offset` names the next page.
The same pair gives the same result with the same Git, because the diff runs
in a scratch repository that ignores Git attributes and configuration. See
FR-14 in [`docs/BLINK_SPEC.md`](docs/BLINK_SPEC.md).

`snapshot.restore_plan` lists what restoring a snapshot would do to the working
tree, one page at a time, and changes nothing. A request with `offset` above 0
must carry the `current_tree` from the first page. If the working tree no
longer captures to `current_tree`, the method answers `-32002`.

The CLI exposes the same plan as
`agent-snap restore-plan <id> [--cwd <dir>] [--path <path>]... [--offset <n>] [--current-tree <oid>]`.
`--path` may repeat, and with none the plan covers the whole tree. The command
exits `0` when it prints a plan, including one that is not applicable, and `1`
on any error, including a stale `--current-tree`.

`client.restorePlan(cwd, id, { paths?, offset?, currentTree? }, signal?)`
returns the validated restore plan (section 8.5). It rejects with
`SnapProtocolError` for a plan that breaks section 8.5, and with
`SnapRestorePlanUnsupportedError` (which extends `SnapRpcError`, code -32601)
when the installed `agent-snap` cannot plan restores. `SnapLike` gains
`restorePlan`.

`client.retentionReport(cwd, offset?, signal?)` returns one validated page of
the retention report (section 8.7). It rejects with `SnapProtocolError` for a
report that breaks section 8.7. It rejects with
`SnapRetentionReportUnsupportedError`, which extends `SnapRpcError` with code
-32601, when the installed `agent-snap` cannot report retention. `SnapLike`
gains `retentionReport`. See FR-17 in
[`docs/BLINK_SPEC.md`](docs/BLINK_SPEC.md).

Both hosts expose the plan as `agent_snap_restore_plan(id, paths?, offset?, current_tree?, cwd?)`.
The tool is read-only and returns one page as text. It names the `offset` and
`current_tree` to request next. No release can apply a restore. See FR-15 in
[`docs/BLINK_SPEC.md`](docs/BLINK_SPEC.md).

## Adapters

### OpenCode plugin

One package serves OpenCode 1.x (>= 1.18.18) and 2.x (>= 2.2.5). Enable it
in `opencode.json(c)` with the key your OpenCode version reads:

```jsonc
// OpenCode 2.x
{ "plugins": ["@agent-snap/opencode"] }

// OpenCode 1.x
{ "plugin": ["@agent-snap/opencode"] }
```

or copy the built package into `.opencode/plugins/`. The plugin:

- snapshots the session directory when a session is created,
- coalesces `session.idle` bursts (1.5 s window) into one settled snapshot,
- exposes `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`,
  `agent_snap_get(id, cwd?)`,
  `agent_snap_diff(from, to, offset?, max_patch_bytes?, cwd?)`, and
  `agent_snap_restore_plan(id, paths?, offset?, current_tree?, cwd?)` tools,
- records host `opencode` and source `session-start`, `session-idle`, or
  `tool` on each snapshot.

Set `AGENT_SNAP_BIN` if the binary is not on `PATH`.

### Pi Coding Agent extension

Targets **Pi Coding Agent** (pi.dev, `@earendil-works/pi-coding-agent` 0.84
through 1.x, Node >= 22.19). Install with Pi's package installer:

```sh
pi install npm:@agent-snap/pi
```

Or place it directly — Pi's discovery scans extension directories one level
deep and does **not** recurse into wrapper `node_modules`, so the manifest
must live at the top of the extension directory:

```sh
mkdir -p .pi/extensions/agent-snap
cd .pi/extensions/agent-snap
npm init -y >/dev/null
npm install @agent-snap/core
npm pack @agent-snap/pi --pack-destination . >/dev/null
tar -xzf agent-snap-pi-*.tgz
mv package/package.json package/dist ./
rm -rf package agent-snap-pi-*.tgz
```

The extension:

- takes a baseline snapshot at `session_start` (awaited as a barrier),
- takes a coalesced (1.5 s burst window) snapshot at `agent_settled`,
- flushes pending snapshots and shuts the Go subprocess down at
  `session_shutdown`,
- registers `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`,
  `agent_snap_get(id, cwd?)`,
  `agent_snap_diff(from, to, offset?, max_patch_bytes?, cwd?)`, and
  `agent_snap_restore_plan(id, paths?, offset?, current_tree?, cwd?)` tools,
- registers `agent-snap-create`, `agent-snap-list`, `agent-snap-get <id>`, and
  `agent-snap-diff <from> <to>` commands,
- records host `pi` and source `session-start`, `agent-settled`, `tool`, or
  `command` on each snapshot.

Set `AGENT_SNAP_BIN` if the binary is not on `PATH`.

## Development

These local gates match CI (`.github/workflows/ci.yml`). `gofmt` is required;
CI runs the Go core on Linux, Windows, and macOS and prints each platform's
skipped Go tests with their reasons. It also runs TypeScript tests on Linux,
Windows, and macOS on both Bun and Node, and smoke on Linux and Windows. Use
`bun run test`, not `bun test`: the script builds first and raises Bun's 5 s
per-test timeout, which teardown tests exceed.

```sh
test -z "$(gofmt -l .)"
go test ./... && go vet ./...
bun install
bun run build && bun run typecheck && bun run test
bun run test:node                                          # core and Pi tests on Node, Pi's runtime
go build -o bin/agent-snap ./cmd/agent-snap
AGENT_SNAP_BIN="$PWD/bin/agent-snap" bun run smoke         # includes a compiled Bun host
AGENT_SNAP_BIN="$PWD/bin/agent-snap" bun run pack-check    # bun pm pack, npm install, check on Node and Bun
```

## Safety model

- Never moves `HEAD`, never writes the real index (a private temp dir holds
  the scratch `GIT_INDEX_FILE`), never writes to the working tree.
- Inherited git-repository environment (`GIT_DIR`, `GIT_WORK_TREE`,
  `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, …) is stripped from every git
  invocation, so commands are pinned to the resolved repository. A diff runs
  in a scratch bare repository outside the worktree that only reads the
  repository's objects, so attributes and configuration cannot change it.
- Commits are written to hidden refs only; no history rewrite, no branch
  updates.
- `git add -A` honors `.gitignore`; the snapshot tree is what the agent would
  commit.
- The TS client runtime-validates every child response, tears down the child
  process group on timeout or close (so a timed-out create can never produce
  a late snapshot), and respawns the transport lazily.

## Roadmap

- **Restore (`snapshot.restore`)** — deliberately out of scope for this MVP.
  Restoring means writing files into the working tree, which is a destructive
  operation that needs careful safety work (backup of current tree, staged
  vs. unstaged preservation, conflict handling, dry-run). Planned as a
  separate, fully safety-tested release.
- Snapshot garbage collection / retention policy.

## License

MIT
