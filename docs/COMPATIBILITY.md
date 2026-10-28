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
| `create [reason] [--cwd <dir>]` | Create one snapshot with source `cli` and no host, and print a snapshot JSON object. `--reason` and a positional reason are mutually exclusive. |
| `list [--cwd <dir>] [--limit <n>]` | Print `{"snapshots":[...]}` newest first. |
| `get <id> [--cwd <dir>]` | Print the snapshot detail object. A missing snapshot fails with `snapshot not found: <id>`. |
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
must be a non-empty string for `snapshot.create`, `snapshot.list`, and
`snapshot.get`.

| Method | Params | Result |
| --- | --- | --- |
| `ping` | none | `{"status":"ok","version":"<version>"}` |
| `snapshot.create` | `cwd`, `reason?`, `source?`, `host?` | snapshot object |
| `snapshot.list` | `cwd`, `limit?` | `{"snapshots":[...]}` |
| `snapshot.get` | `cwd`, `id` | snapshot detail object |

`limit` is an integer from **1 through 100**. Omitted `limit` uses the store
default of **20**, newest first.

`id` must match `<YYYYMMDD>T<HHMMSS>.<9 digits>Z-<12 lowercase hex>`. It is
checked before the repository is resolved, and it resolves only inside the
`cwd` worktree's own namespace.

### Error codes

| Code | Meaning |
| --- | --- |
| `-32700` | Parse error (malformed JSON or oversized line) |
| `-32601` | Method not found |
| `-32602` | Invalid params (unknown key, missing/blank `cwd`, bad types, bad `limit`, malformed snapshot `id`, invalid origin value, a reason that contains a NUL byte or ends with an origin paragraph) |
| `-32603` | Internal error (repository resolution, Git, scratch index, ref update, tampered snapshot ref) |
| `-32001` | Snapshot not found: a well-formed `id` with no snapshot in this worktree |

Error messages name the operation. They must not include working-tree file
contents.

### Snapshot object

| Field | Type | When present |
| --- | --- | --- |
| `id` | string | always |
| `commit` | string | always |
| `reason` | string | always |
| `created_at` | RFC 3339 string | always |
| `source` | string | when the snapshot stores an origin source |
| `host` | string | when the snapshot stores an origin host |
| `changed_files` | string array | create of a new snapshot |
| `reused` | boolean | create; `true` when the latest checkpoint was reused |

`id` is `<UTC timestamp>-<first 12 characters of commit>`. List results include
`id`, `commit`, `reason`, and `created_at`, plus `source` and `host` when stored.
`changed_files` and `reused` are create metadata and are not guaranteed on list.
A reused snapshot reports its own stored origin, not the origin of the request.

### Snapshot detail object

Returned by `snapshot.get` and `agent-snap get`.

| Field | Type | When present |
| --- | --- | --- |
| `id`, `commit`, `reason`, `created_at` | as in the snapshot object | always |
| `source`, `host` | as in the snapshot object | when stored |
| `tree` | string | always |
| `parent` | string | when the snapshot commit has a parent |
| `parent_id` | string | when the parent is a snapshot in the same worktree |
| `changed_files` | string array, at most 1000 entries | always (may be empty) |
| `changed_files_truncated` | boolean | `true` when more than 1000 paths changed |

### Snapshot origin

`source` names what asked for a snapshot and `host` names the integration that
sent the request. Both are optional on `snapshot.create` and must match
`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, at most 64 characters. Origin is reported by
the caller and is never evidence. Callers must not put session, user, machine,
or path identifiers in it.

A snapshot with origin stores it at the end of its commit message:

```text
agent-snap: <reason>

Agent-Snap-Source: <source>
Agent-Snap-Host: <host>
```

Without origin, the message is the 0.1.0 form, `agent-snap: <reason>`. The
exact parse rule, and the rule that refuses a reason which would read back as
origin, are FR-13 in [`BLINK_SPEC.md`](BLINK_SPEC.md).

Invalid input fails with `-32602` and one of these messages:

- `param must be an origin value: source` / `: host`
- `param must not contain a NUL byte or end with an origin paragraph: reason`
- `param must be a string: <key>` for a non-string value, including `null`

Mixed versions:

- A 0.1.0 binary rejects `source` and `host` with `unknown param: <key>`.
  `SnapClient` then retries the create once without them, logs the drop once,
  and refuses to retry a reason containing `\n\nAgent-Snap-`. A plugin
  upgraded ahead of its binary keeps taking snapshots, without origin.
- A 0.1.0 client ignores `source` and `host` in results.
- A 0.1.0 reader shows a newer snapshot's origin lines as part of its reason.

## Hidden Git refs

Namespace: `refs/agent-snapshots/`. Callers never construct these paths; only
`internal/snapshot.Store` owns them. Refs are per worktree. Capture, list, and
get never move `HEAD`, write the real index, or write the working tree; get
also writes no refs or objects.

## TypeScript client (`@agent-snap/core`)

Public surface:

- `SnapClient` / `SnapClient.fromEnv()` / `SnapClient.defaultBin()`
- `SnapLike` (`create(cwd, reason?, signal?, origin?)`, `list`, `get`, `close`)
- `SnapOrigin`, `SnapSnapshot`, `SnapSnapshotDetail`, `SnapListResult`, `PingResult`
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
| Origin | Host `opencode`; source `session-start`, `session-idle`, or `tool`. |
| `agent_snap_list(cwd?, limit?)` | Manual list. |
| `agent_snap_get(id, cwd?)` | Describe one snapshot. Read-only. |

## Pi extension (`@agent-snap/pi`)

Requires Pi Coding Agent `>= 0.84` and Node `>= 22.19`. Manifest key: `pi.extensions`.

| Hook / tool / command | Behaviour |
| --- | --- |
| `session_start` | Awaited baseline (`session-start`). Failure is logged, not propagated. |
| `agent_settled` | Coalesced snapshot, 1.5 s window. |
| `session_shutdown` | Flush, then close the client. |
| `agent_snap_create` / `agent_snap_list` / `agent_snap_get` | Tools. |
| `agent-snap-create` / `agent-snap-list` / `agent-snap-get <id>` | Commands. |
| Origin | Host `pi`; source `session-start`, `agent-settled`, `tool`, or `command`. |

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
