# Operational diagnostics

Use this when a snapshot operation or client teardown fails. Collect enough to
see *which* operation failed. Do **not** log working-tree file contents, diffs,
secrets, or full file listings.

## What is safe to record

- Operation (`create`, `list`, `diff`, `restore-plan`, `ping`, `close`, teardown)
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
blob or working-tree payload to host logs. A Git built with GNU gettext
prints its own messages in English whatever the locale, because the store
runs Git with `LANGUAGE=C`. The stderr also holds what a clean or process
filter printed, in the filter's own language. The store reads every value
from Git's standard output and discards what a filter prints during a
command that succeeds. The store keeps the last 8 KiB of a failed command's
stderr, because Git prints its own message after any filter output. When
the stderr was longer, it starts with
`[<n> earlier bytes of standard error omitted]`.

## Snapshot failures

| Symptom | Likely cause | What to check |
| --- | --- | --- |
| `find git repository` | `cwd` is not inside a Git work tree | `cwd`, that Git is on `PATH` |
| `no scratch directory found outside repository` | `TMPDIR` / temp dir resolves inside the repo | Point temp at a directory *outside* the worktree. Capture and diff refuse to write scratch files inside the repo. |
| `snapshot ref … is a symbolic ref; refusing to update it` / `refusing to read it` | A hidden ref is a symlink into another ref | Do not retarget `refs/agent-snapshots/*`. Inspect the ref name only. |
| `snapshot ref … points at a …, not a commit` / `does not match its id` / `has more than one parent` / `which agent-snap did not write` | An item ref was edited by hand or repointed at a commit agent-snap did not create | Lookup refuses tampered item refs. Delete the bad ref with `git update-ref -d`; do not repoint it. A create never reuses a refused or deleted item ref. When that ref was the one `latest` names, the next create of an unchanged tree records a new snapshot of the same tree. |
| `snapshot ref not found` on every create of an unchanged tree | A binary built before this fix, such as 0.1.0, and the item ref that `latest` names was deleted | Upgrade the binary. The next create then records a new snapshot. Changing any file has the same effect on an older binary. |
| `update refs` / CAS failure | Concurrent create lost the compare-and-swap | Retry is the client's job on the next create. Confirm the ref namespace is the worktree's. |
| `update refs: … cannot lock ref '<ref>': unable to create directory for <path>` | Git could not create the snapshot ref's directory in 5 attempts. Blink retries a directory that a concurrent `git pack-refs --prune` removed, so a failure after 5 attempts points to a permission problem or a full disk | Check that the user who runs `agent-snap` can write `refs/agent-snapshots/` and its parents under the directory that `git rev-parse --git-common-dir` prints. Do not delete or rename a ref directory. |
| `snapshot ref lock refs/agent-snapshots/<namespace>/latest.lock in the Git directory stayed held for 1s` (or an `items/<id>.lock`) | A Git process died inside its ref transaction and left the lock, or another process held it for over a second | Check that no `agent-snap` or `git` process runs for this repository. Then remove that one file, under the directory that `git rev-parse --git-common-dir` prints. Remove no other file. Blink never removes the lock itself. |
| `list configured hooks: hook name "<name>" contains '=', so it cannot be disabled on the Git command line` | The configuration defines a hook through `hook.<name>.command` or `hook.<name>.event` whose name contains `=`. Blink turns off every configured hook during a create or a restore plan with `-c hook.<name>.enabled=false`, and Git cannot parse such a name there. | Run `git config --show-origin --get-regexp '^hook\.'` and rename that hook. The repository is unchanged. |
| `git …` with object-format / git-dir errors | Broken Git dir or unsupported repo | Confirm SHA-1 or SHA-256 repo; inherited `GIT_*` must not redirect the repo. |
| `create evidence repository: …` | Git could not create the private bare repository that `snapshot.diff` runs in | The temp directory outside the worktree is writable, and Git is 2.30 or newer. The repository is unchanged. |
| `object directory … cannot be written to an alternates file` | The repository's object directory path contains a line break | Move the repository to a path without line breaks. |
| `git diff-tree: …` naming a bad or missing object | A snapshot tree names an object the repository no longer has, for example in a partial clone | Diff never fetches. Check the object with `GIT_NO_LAZY_FETCH=1 git cat-file -e <oid>`. |
| `diff summary …`, `diff patch …`, `diff hunk …`, `malformed raw diff record`, `malformed numstat record`, or `… but numstat reports …` | Git's diff output failed the integrity check | Record the Git version and the message. Do not attach the patch. |
| JSON-RPC `-32602` | Unknown key in `params`, a request key other than `id`, `method`, and `params` (`unknown request key: <key>`), missing `cwd`, bad `limit`, malformed snapshot `id`, `from`, or `to`, `offset` outside 0 through 2147483647, bad restore plan `paths` or `current_tree`, `offset` above 0 without `current_tree`, `max_patch_bytes` outside 1024 through 1048576, invalid `source`/`host`, a reason with a NUL byte or ending in `Agent-Snap-` lines | Fix the request. The server does not coerce types. |
| Log: `agent-snap binary does not accept snapshot origin; creating snapshots without source and host` | The binary is 0.1.0 and the plugin is newer | Snapshots still work. Upgrade the binary to record origin. |
| `agent-snap binary does not accept snapshot origin; refusing to send it a reason that ends like an origin paragraph` | 0.1.0 binary, and a reason containing `\n\nAgent-Snap-` | Upgrade the binary, or use a different reason. |
| JSON-RPC `-32001` | The `snapshot.get` or `snapshot.restore_plan` id, or either `snapshot.diff` side, does not exist in this worktree. Diff checks `from` first. | Take ids from `snapshot.list` for the same `cwd`. Ids from another worktree do not resolve. |
| JSON-RPC `-32002` | A `snapshot.restore_plan` request carried a `current_tree` that the working tree no longer captures to | The working tree changed between pages. Request the first page again without `current_tree` and use the new value. The method only plans and changes nothing. |
| JSON-RPC `-32601` | Unknown method, or `snapshot.diff` or `snapshot.restore_plan` sent to a binary that predates it | Only `ping`, `snapshot.create`, `snapshot.list`, `snapshot.get`, `snapshot.diff`, `snapshot.restore_plan` exist. Upgrade the binary if the diff tool gets `-32601`. `SnapClient`, the CLI, and `agent_snap_restore_plan` call `snapshot.restore_plan`. |
| `SnapProtocolError` | Child wrote a non-conforming line | Treat as a transport bug. Discard the payload; do not forward it to the host. |
| `SnapProtocolError` beginning `snapshot.restore_plan result` | The binary and the client disagree about the restore plan | Report it as a bug with the Git and `agent-snap` versions. Do not attach the plan. |
| `SnapProtocolError` beginning `snapshot.retention_report result` | The binary and the client disagree about the retention report | Report it as a bug with the Git and `agent-snap` versions. |
| `SnapProtocolError` ending `has a malformed id`, `has a malformed parent_id`, `has a malformed from.id`, `has a malformed to.id`, `has a malformed snapshot.id`, `has a malformed oldest.id`, or `has a malformed newest.id` | The binary returned a snapshot id that does not match the FR-12 format. An older binary can list a planted or corrupt item ref's name, or a Git warning about it, as an id. | Upgrade the binary. Inspect `git for-each-ref refs/agent-snapshots` for the bad ref name. |
| `SnapRestorePlanUnsupportedError` | `restorePlan`, or the `agent_snap_restore_plan` tool, got `-32601` from a binary that predates `snapshot.restore_plan` | Upgrade the binary or set `AGENT_SNAP_BIN`. |
| `SnapRetentionReportUnsupportedError` | `retentionReport` got `-32601` from a binary that predates `snapshot.retention_report` | Upgrade the binary or set `AGENT_SNAP_BIN`. |
| `agent_snap_restore_plan` fails with `-32002` | The working tree changed since the page's `current_tree` | Call the tool again from offset 0 without `current_tree`, then use the new `current_tree` for later pages. The tool only plans and changes nothing. |
| `agent_snap_restore_plan` fails with `-32602` and `missing required param: current_tree` | The call set `offset` above 0 without `current_tree` | Pass the `current_tree` that the first page named, or start again from offset 0. |
| `agent-snap: working tree changed since the plan` from `restore-plan` | `--current-tree` no longer matches what the working tree captures to | Run `restore-plan` again without `--current-tree` and use the new `current_tree` for later pages. The command only plans and changes nothing. |
| `agent-snap: invalid --path: …` or `--offset above 0 requires --current-tree` from `restore-plan` | The flags break the FR-15 scope or paging rules | Fix the flags. The command rejects them before it resolves the repository. |

A failed create must not leave an unobserved checkpoint. If the client
timed out or aborted, assume teardown started and do not trust a late
stdout snapshot.

## Teardown failures

Timeout, abort, crash, and `close()` all start teardown immediately and
fail closed.

### POSIX (Linux, macOS)

The client spawns a guardian supervisor as the process-group leader. It
never signals a bare PGID from the host process (the guardian is in that
group and a recycled PGID must not be signalled).

- `agent-snap supervisor exited (code=1, signal=none)` right after start, in
  a compiled host — the host executable did not run the supervisor script.
  OpenCode needs to be built with Bun 1.2.16 or later.
- `agent-snap supervisor cannot start: this host's executable ran its own
  entrypoint …` — the host re-ran itself instead of the supervisor script and
  refused to start another copy. Fail closed.

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

### Scratch directories left after a hard kill

Each create, diff, and restore plan works in one private directory named
`agent-snap-index-*`. It lives in the system temporary directory (`TMPDIR`
on POSIX, `TMP` or `TEMP` on Windows). When that directory resolves inside
the repository, Blink uses the user cache directory or `/tmp` instead. A
create directory holds a scratch `index` and an empty `hooks` directory,
which a restore plan directory also holds. A diff or restore plan directory
also holds `evidence.git`. A restore plan's `evidence.git/objects` can hold
blobs of working-tree files, so treat a leftover directory as file contents.
Do not attach it to a report.

On POSIX the guardian sends SIGTERM to the process group. `agent-snap`
cancels the request in flight and sends SIGTERM to its Git child, so Git
removes its own ref lock files. When its Git child still runs 500 ms later,
`agent-snap` kills that child. It kills no filter that Git started.
`agent-snap` then removes the directory and exits. The guardian sends SIGKILL
to the group `KILL_GRACE_MS` (2 seconds) later, which ends a filter that
ignored SIGTERM. A directory that is still being removed
then stays behind, in part or in full. A restore plan over a working tree
with tens of thousands of new or changed files can need that long.
A SIGKILL sent straight to `agent-snap` also leaves its directory.

On Windows, `taskkill /T /F` ends the process tree with no cleanup. Every
request that times out or is aborted on Windows leaves its directory.

Blink does not sweep these directories, because it cannot prove that a
directory is not in use by another running `agent-snap`. Delete
`agent-snap-index-*` directories by hand when no `agent-snap` process is
running.

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
3. Go / Git versions, and the host runtime (Node or Bun) with its version
4. The error string and JSON-RPC code
5. Whether the failure was timeout, abort, or Git
6. Whether `HEAD`, the real index, and the worktree were unchanged (`git status`
   *summary line only*)

Omit repository file contents.
