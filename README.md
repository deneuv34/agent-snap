# agent-snap

Zero-impact Git working-tree snapshots for AI agent sessions.

Agent sessions mutate your working tree relentlessly. `agent-snap` records
snapshots of the working tree at key moments — session start (baseline) and
when the agent settles to idle — without ever touching `HEAD`, the real index,
or the working tree. Snapshots live in hidden git refs
(`refs/agent-snapshots/*`), so they survive `git gc`, never appear in `git
branch`, and never collide with your real history.

## How it works

1. A Go core (`cmd/agent-snap`) exposes `create` / `list` / `get` / `ping` as
   direct JSON commands and a long-running JSONL `serve` mode on stdin/stdout.
2. Thin TypeScript adapters launch and reuse the Go subprocess:
   - `@agent-snap/opencode` — an OpenCode plugin (baseline at
     `session.created`, coalesced snapshot at `session.idle`, plus
     `agent_snap_create` / `agent_snap_list` / `agent_snap_get` tools).
   - `@agent-snap/pi` — a Pi Coding Agent extension (baseline at
     `session_start`, coalesced snapshot at `agent_settled`, cleanup at
     `session_shutdown`, plus `agent_snap_create` / `agent_snap_list` /
     `agent_snap_get` tools and `agent-snap-create` / `agent-snap-list` /
     `agent-snap-get` commands).
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
- Node.js 22.19+ (adapters only; the Pi extension requires the same floor as
  Pi Coding Agent)
- git 2.30+

Supported CI matrix, release artifacts, and publication steps:
[docs/RELEASE.md](docs/RELEASE.md). Frozen CLI / JSONL / package contract:
[docs/COMPATIBILITY.md](docs/COMPATIBILITY.md). Failure diagnosis without
repository contents: [docs/DIAGNOSTICS.md](docs/DIAGNOSTICS.md).

## Install

```sh
# Go core
go install ./cmd/agent-snap

# TypeScript adapters (optional)
npm install
npm run build
```

## CLI

```sh
agent-snap create "before big refactor" --cwd /path/to/repo   # prints snapshot JSON
agent-snap list --cwd /path/to/repo --limit 10                # prints {"snapshots": [...]}
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

Error codes: `-32700` parse error, `-32601` method not found,
`-32602` invalid params, `-32603` internal error, `-32001` snapshot not found.

`source` and `host` are the snapshot's origin: what asked for it, and which
integration sent the request. Each is a lowercase hyphenated token of at most
64 characters. They are stored in the snapshot commit and returned by create,
list, and get when present. The CLI records `source: cli`; the OpenCode and Pi
integrations record their own host and event. Origin is reported by the
caller, so treat it as a label, not evidence. No session identifiers are
stored. See FR-13 in [`docs/BLINK_SPEC.md`](docs/BLINK_SPEC.md).

## Adapters

### OpenCode plugin

```jsonc
// opencode.json
{ "plugin": ["@agent-snap/opencode"] }
```

or copy the built package into `.opencode/plugins/`. The plugin:

- snapshots the session directory when a session is created,
- coalesces `session.idle` bursts (1.5 s window) into one settled snapshot,
- exposes `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`,
  and `agent_snap_get(id, cwd?)` tools,
- records host `opencode` and source `session-start`, `session-idle`, or
  `tool` on each snapshot.

Set `AGENT_SNAP_BIN` if the binary is not on `PATH`.

### Pi Coding Agent extension

Targets **Pi Coding Agent** (pi.dev, `@earendil-works/pi-coding-agent` >=
0.84, Node >= 22.19). Install with Pi's package installer:

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
rmdir package && rm agent-snap-pi-*.tgz
```

The extension:

- takes a baseline snapshot at `session_start` (awaited as a barrier),
- takes a coalesced (1.5 s burst window) snapshot at `agent_settled`,
- flushes pending snapshots and shuts the Go subprocess down at
  `session_shutdown`,
- registers `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`,
  and `agent_snap_get(id, cwd?)` tools,
- registers `agent-snap-create`, `agent-snap-list`, and `agent-snap-get <id>`
  commands,
- records host `pi` and source `session-start`, `agent-settled`, `tool`, or
  `command` on each snapshot.

Set `AGENT_SNAP_BIN` if the binary is not on `PATH`.

## Development

These local gates match CI (`.github/workflows/ci.yml`). `gofmt` is required;
CI also runs TypeScript tests on Linux, Windows, and macOS, and smoke on
Linux and Windows.

```sh
test -z "$(gofmt -l .)"
go test ./... && go vet ./...
npm install
npm run build && npm run typecheck && npm test
go build -o bin/agent-snap ./cmd/agent-snap
AGENT_SNAP_BIN="$PWD/bin/agent-snap" npm run smoke
AGENT_SNAP_BIN="$PWD/bin/agent-snap" npm run pack-check   # npm pack + clean tarball install
```

## Safety model

- Never moves `HEAD`, never writes the real index (a private temp dir holds
  the scratch `GIT_INDEX_FILE`), never writes to the working tree.
- Inherited git-repository environment (`GIT_DIR`, `GIT_WORK_TREE`,
  `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, …) is stripped from every git
  invocation, so commands are pinned to the resolved repository.
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
- `snapshot.diff` between two snapshots.

## License

MIT
