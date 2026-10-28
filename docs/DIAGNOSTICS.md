# Operational diagnostics

Use this when a snapshot operation or client teardown fails. Collect enough to
see *which* operation failed. Do **not** log working-tree file contents, diffs,
secrets, or full file listings.

## What is safe to record

- Operation (`create`, `list`, `diff`, `ping`, `close`, teardown)
- JSON-RPC error code and the error message returned by the core
- Repository *existence* / "not a git repository", never file bodies
- Snapshot id, commit id, reason string, `reused`, origin `source` and `host`
- Process id, process-group id, `taskkill` exit status, timeout duration
- Host event name (`session.created`, `agent_settled`, …)
- Binary path (`AGENT_SNAP_BIN`) and protocol version

## What must not be recorded

- File contents, patches, `snapshot.diff` hunks, or `git show` / `git diff` output
- Secrets, tokens, `.env` values, or credential helpers
- Entire `git status --porcelain` dumps in shared logs (path names in a
  short error are acceptable; do not attach the tree)
- Scratch-index file bodies

The Go store wraps Git failures as `git <args>: <error>: <stderr>`. Treat
that stderr as a Git *status* string (paths, ref names). Do not add the
blob or working-tree payload to host logs.

## Snapshot failures

| Symptom | Likely cause | What to check |
| --- | --- | --- |
| `find git repository` | `cwd` is not inside a Git work tree | `cwd`, that Git is on `PATH` |
| `no scratch directory found outside repository` | `TMPDIR` / temp dir resolves inside the repo | Point temp at a directory *outside* the worktree. Capture and diff refuse to write scratch files inside the repo. |
| `snapshot ref … is a symbolic ref; refusing to update it` / `refusing to read it` | A hidden ref is a symlink into another ref | Do not retarget `refs/agent-snapshots/*`. Inspect the ref name only. |
| `snapshot ref … points at a …, not a commit` / `does not match its id` / `has more than one parent` / `which agent-snap did not write` | An item ref was edited by hand or repointed at a commit agent-snap did not create | Lookup refuses tampered item refs. Delete the bad ref with `git update-ref -d`; do not repoint it. |
| `update refs` / CAS failure | Concurrent create lost the compare-and-swap | Retry is the client's job on the next create. Confirm the ref namespace is the worktree's. |
| `git …` with object-format / git-dir errors | Broken Git dir or unsupported repo | Confirm SHA-1 or SHA-256 repo; inherited `GIT_*` must not redirect the repo. |
| `create evidence repository: …` | Git could not create the private bare repository that `snapshot.diff` runs in | The temp directory outside the worktree is writable, and Git is 2.30 or newer. The repository is unchanged. |
| `object directory … cannot be written to an alternates file` | The repository's object directory path contains a line break | Move the repository to a path without line breaks. |
| `git diff-tree: …` naming a bad or missing object | A snapshot tree names an object the repository no longer has, for example in a partial clone | Diff never fetches. Check the object with `GIT_NO_LAZY_FETCH=1 git cat-file -e <oid>`. |
| `diff summary …`, `diff patch …`, `diff hunk …`, `malformed raw diff record`, `malformed numstat record`, or `… but numstat reports …` | Git's diff output failed the integrity check | Record the Git version and the message. Do not attach the patch. |
| JSON-RPC `-32602` | Unknown key, missing `cwd`, bad `limit`, malformed snapshot `id`, `from`, or `to`, `offset` outside 0 through 2147483647, `max_patch_bytes` outside 1024 through 1048576, invalid `source`/`host`, a reason with a NUL byte or ending in `Agent-Snap-` lines | Fix the request. The server does not coerce types. |
| Log: `agent-snap binary does not accept snapshot origin; creating snapshots without source and host` | The binary is 0.1.0 and the plugin is newer | Snapshots still work. Upgrade the binary to record origin. |
| `agent-snap binary does not accept snapshot origin; refusing to send it a reason that ends like an origin paragraph` | 0.1.0 binary, and a reason containing `\n\nAgent-Snap-` | Upgrade the binary, or use a different reason. |
| JSON-RPC `-32001` | The `snapshot.get` id, or either `snapshot.diff` side, does not exist in this worktree. Diff checks `from` first. | Take ids from `snapshot.list` for the same `cwd`. Ids from another worktree do not resolve. |
| JSON-RPC `-32601` | Unknown method, or `snapshot.diff` sent to a binary that predates it | Only `ping`, `snapshot.create`, `snapshot.list`, `snapshot.get`, `snapshot.diff` exist. Upgrade the binary if the diff tool gets `-32601`. |
| `SnapProtocolError` | Child wrote a non-conforming line | Treat as a transport bug. Discard the payload; do not forward it to the host. |

A failed create must not leave an unobserved checkpoint. If the client
timed out or aborted, assume teardown started and do not trust a late
stdout snapshot.

## Teardown failures

Timeout, abort, crash, and `close()` all start teardown immediately and
fail closed.

### POSIX (Linux, macOS)

The client spawns a guardian supervisor as the process-group leader. It
never signals a bare PGID from the Node process (the guardian is in that
group and a recycled PGID must not be signalled).

Failure messages:

- `failed to inspect agent-snap process group <pid>: …` — `kill(-pgid, 0)`
  returned something other than ESRCH (for example EPERM). Fail closed.
- `agent-snap process group <pid> did not terminate within the verification window` —
  members remained after the grace + wait window.

Check: the binary still running, leftover `git` children, whether the
guardian reported `child-exit` or `spawn-error`. Do not dump the child's
JSONL transcript (it can include `cwd` and reasons only; still keep it
off shared logs if it may contain path lists).

### Windows

Cleanup succeeds only when **both** are true: `taskkill /T /F` reported
success **and** the root process exited.

Failure messages:

- `taskkill failed for agent-snap (pid <pid>): the process tree may still be running`
- `agent-snap (pid <pid>) did not terminate within the Windows tree-cleanup window`
- `agent-snap (pid <pid>) exited before its process tree could be terminated on Windows; descendants may still be running`

A root that exits by itself is **not** success; descendants can survive it.
Check Task Manager / `tasklist` for leftover `agent-snap.exe` or `git.exe`
started by that tree. Record pid and `taskkill` status only.

## Binary not found

```
agent-snap binary not found at "<path>" — install it (go install ./cmd/agent-snap) or set AGENT_SNAP_BIN
```

Hosts should log the resolved path, not spawn Git as a fallback.

## Host logging

OpenCode and Pi log lifecycle failures so they do not crash the host (except
Pi's session-start barrier, which still logs rather than throwing). Keep
those messages to operation + error string. Do not attach snapshot
`changed_files` arrays or diff `files` to telemetry.

## Support bundle (safe)

If you need to file a bug, include:

1. `agent-snap version` / `ping` JSON
2. OS and architecture
3. Go / Node / Git versions
4. The error string and JSON-RPC code
5. Whether the failure was timeout, abort, or Git
6. Whether `HEAD`, the real index, and the worktree were unchanged (`git status`
   *summary line only*)

Omit repository file contents.
