# Compatibility contract

This is the published contract for `agent-snap` **0.1.0**. Hosts and scripts may
depend on everything listed here. Changing any frozen identifier requires a
Phase 1 migration policy and compatibility tests.

Identity remains `agent-snap` until that policy exists. Do not rename the
executable, npm packages, JSONL methods, or hidden refs in this release.

## Version

| Surface | Value |
| --- | --- |
| Protocol / CLI (`agent-snap version`, `ping.version`) | `0.1.0` |
| npm `@agent-snap/core`, `@agent-snap/opencode`, `@agent-snap/pi` | `0.1.0` |
| Release builds may override the Go version with `-ldflags "-X github.com/deneuv34/agent-snap/internal/protocol.Version=x.y.z"` | must match the npm version of the same release |

## CLI

Executable name: `agent-snap`.

| Command | Behaviour |
| --- | --- |
| `create [reason] [--cwd <dir>]` | Create one snapshot and print a snapshot JSON object. `--reason` and a positional reason are mutually exclusive. |
| `list [--cwd <dir>] [--limit <n>]` | Print `{"snapshots":[...]}` newest first. |
| `ping` | Print `{"status":"ok","version":"<version>"}`. |
| `version` | Print the version string and a newline. |
| `serve` | JSONL server on stdin/stdout. Takes no arguments. |

Missing `--cwd` defaults to the process working directory. CLI failures print
`agent-snap: <error>` on stderr and exit `1`. They are not JSON-RPC errors.

## JSONL protocol

`agent-snap serve` reads one JSON request per line and writes one JSON response
per line. Empty lines are ignored. The server continues after a request-level
error. Maximum request line size: **4 MiB**.

Request:

```json
{"id":1,"method":"snapshot.create","params":{"cwd":"/path/to/repo","reason":"why"}}
```

Unknown keys on the request or in `params` are rejected. `cwd` is required and
must be a non-empty string for `snapshot.create` and `snapshot.list`.

| Method | Params | Result |
| --- | --- | --- |
| `ping` | none | `{"status":"ok","version":"<version>"}` |
| `snapshot.create` | `cwd`, `reason?` | snapshot object |
| `snapshot.list` | `cwd`, `limit?` | `{"snapshots":[...]}` |

`limit` is an integer from **1 through 100**. Omitted `limit` uses the store
default of **20**, newest first.

### Error codes

| Code | Meaning |
| --- | --- |
| `-32700` | Parse error (malformed JSON or oversized line) |
| `-32601` | Method not found |
| `-32602` | Invalid params (unknown key, missing/blank `cwd`, bad types, bad `limit`) |
| `-32603` | Internal error (repository resolution, Git, scratch index, ref update) |

Error messages name the operation. They must not include working-tree file
contents.

### Snapshot object

| Field | Type | When present |
| --- | --- | --- |
| `id` | string | always |
| `commit` | string | always |
| `reason` | string | always |
| `created_at` | RFC 3339 string | always |
| `changed_files` | string array | create of a new snapshot |
| `reused` | boolean | create; `true` when the latest checkpoint was reused |

`id` is `<UTC timestamp>-<first 12 characters of commit>`. List results include
`id`, `commit`, `reason`, and `created_at`. `changed_files` and `reused` are
create metadata and are not guaranteed on list.

## Hidden Git refs

Namespace: `refs/agent-snapshots/`. Callers never construct these paths; only
`internal/snapshot.Store` owns them. Refs are per worktree. Capture and list
never move `HEAD`, write the real index, or write the working tree.

## TypeScript client (`@agent-snap/core`)

Public surface:

- `SnapClient` / `SnapClient.fromEnv()` / `SnapClient.defaultBin()`
- `SnapLike` (`create`, `list`, `close`)
- `SnapSnapshot`, `SnapListResult`, `PingResult`
- `SnapRpcError` (JSON-RPC `code` + `message`)
- `SnapProtocolError` (malformed child output)

`AGENT_SNAP_BIN` overrides the executable. Default timeout is **30 seconds**.
`AbortSignal` and timeout start process-tree teardown immediately. Every
success and error payload is runtime-validated before it reaches a host.

Hosts depend on `SnapLike` only. They do not spawn `agent-snap` themselves
and do not invoke Git.

## OpenCode plugin (`@agent-snap/opencode`)

Install: `{ "plugin": ["@agent-snap/opencode"] }` in `opencode.json`.

| Hook / tool | Behaviour |
| --- | --- |
| `session.created` | Baseline snapshot. Failures are logged, not thrown. |
| `session.idle` | Coalesced snapshot, 1.5 s window. |
| `session.busy` | Cancels pending idle work. |
| `session.deleted` | Cancels that session's work. |
| `agent_snap_create(reason?, cwd?)` | Manual create. |
| `agent_snap_list(cwd?, limit?)` | Manual list. |

## Pi extension (`@agent-snap/pi`)

Requires Pi Coding Agent `>= 0.84` and Node `>= 22.19`. Manifest key: `pi.extensions`.

| Hook / tool / command | Behaviour |
| --- | --- |
| `session_start` | Awaited baseline (`session-start`). Failure is logged, not propagated. |
| `agent_settled` | Coalesced snapshot, 1.5 s window. |
| `session_shutdown` | Flush, then close the client. |
| `agent_snap_create` / `agent_snap_list` | Tools. |
| `agent-snap-create` / `agent-snap-list` | Commands. |

The packaged extension must expose `package.json` and `dist/` at the extension
directory root. Pi does not recurse into wrapper `node_modules`.

## Compatibility promises

Until a migration policy is published:

- Method names, error codes, and snapshot field names stay as shipped.
- `refs/agent-snapshots/` stays the only snapshot namespace.
- Adding a method or optional result field is allowed only after spec → store
  → protocol → client validation → CLI → hosts.
- Removing or renaming any frozen name is not allowed in a patch or minor
  release without a documented dual-read/dual-write period.

See [RELEASE.md](RELEASE.md) for the platform matrix and publication steps.
