import { appendFileSync, existsSync, readFileSync, writeFileSync } from "node:fs"
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"

const mode = process.env.FAKE_AGENT_SNAP_MODE ?? ""
const logFile = process.env.FAKE_AGENT_SNAP_LOG
const stateFile = process.env.FAKE_AGENT_SNAP_STATE

const record = (obj) => {
  if (logFile) appendFileSync(logFile, JSON.stringify({ pid: process.pid, ...obj }) + "\n")
}

if (mode === "trap-term") {
  process.on("SIGTERM", () => {
    record({ event: "sigterm" })
    setTimeout(() => process.exit(0), 5000)
  })
}

if (mode === "survive-eof") {
  // Keep running even if stdin reaches EOF (e.g. the guardian supervisor is
  // killed), so tests can exercise cleanup of a tree that survives its
  // supervisor.
  setInterval(() => {}, 10_000)
}

const rl = createInterface({ input: process.stdin })
let created = 0
let grandchildSpawned = false

rl.on("line", (line) => {
  let req
  try {
    req = JSON.parse(line)
  } catch {
    respond(null, null, { code: -32700, message: "parse error" })
    return
  }
  record({ event: "request", method: req.method, params: req.params ?? null })
  if (mode === "grandchild" && req.method === "ping" && !grandchildSpawned) {
    grandchildSpawned = true
    // The grandchild reports its own termination: the fake process itself is
    // dead by the time the group signal is handled, so it cannot record it.
    // The grandchild first installs its TERM trap, then signals readiness;
    // the fake only answers once ready, so a close() racing the spawn can
    // never TERM the shell before its trap exists.
    spawn(
      "sh",
      ["-c", `trap 'echo grandchild-exited >> "$FAKE_LOG"' TERM; echo grandchild-ready >> "$FAKE_LOG"; sleep 30`],
      { stdio: "ignore", env: { ...process.env, FAKE_LOG: logFile ?? "" } },
    )
    const respondWhenReady = () => {
      try {
        if (readFileSync(logFile ?? "", "utf8").includes("grandchild-ready")) {
          handle(req)
          return
        }
      } catch {
        // log not written yet
      }
      setTimeout(respondWhenReady, 10)
    }
    respondWhenReady()
    return
  }
  if (mode === "exit-on-list" && req.method === "snapshot.list") {
    process.exit(0)
  }
  if (mode === "exit-on-ping" && req.method === "ping") {
    process.exit(0)
  }
  if (mode === "slow-create-grandchild" && req.method === "snapshot.create" && !grandchildSpawned) {
    grandchildSpawned = true
    // Hang forever with a grandchild so the client times out and must tear
    // down the whole process group. The grandchild reports its own
    // termination; the fake never responds to the create.
    spawn(
      "sh",
      ["-c", `trap 'echo grandchild-exited >> "$FAKE_LOG"' TERM; sleep 30`],
      { stdio: "ignore", env: { ...process.env, FAKE_LOG: logFile ?? "" } },
    )
    return
  }
  if (mode === "stubborn-create-grandchild" && req.method === "snapshot.create" && !grandchildSpawned) {
    grandchildSpawned = true
    // Like slow-create-grandchild, but the grandchild traps SIGTERM and
    // keeps sleeping, so the tree only dies when the guardian escalates to
    // SIGKILL after the grace period — this makes teardown duration
    // observable for concurrency tests.
    spawn(
      "sh",
      ["-c", `trap 'echo grandchild-saw-term >> "$FAKE_LOG"' TERM; sleep 30`],
      { stdio: "ignore", env: { ...process.env, FAKE_LOG: logFile ?? "" } },
    )
    return
  }
  if (mode === "v010" && req.method === "snapshot.create") {
    // A 0.1.0 binary rejects keys it does not know before any other work.
    const unknown = Object.keys(req.params ?? {}).find((key) => key !== "cwd" && key !== "reason")
    if (unknown !== undefined) {
      respond(req, null, { code: -32602, message: `unknown param: ${unknown}` })
      return
    }
  }
  if (mode === "invalid-origin" && req.method === "snapshot.create") {
    respond(req, null, { code: -32602, message: "param must be an origin value: source" })
    return
  }
  if (mode === "internal-unknown-param" && req.method === "snapshot.create") {
    respond(req, null, { code: -32603, message: "unknown param: source" })
    return
  }
  if (mode === "garbage-line" && req.method === "ping") {
    console.log("this is not valid json at all")
  }
  const slow =
    (mode === "slow-create" && req.method === "snapshot.create") ||
    (mode === "slow-first-then-fast" && req.method === "snapshot.create" && !existsSync(stateFile ?? ""))
  if (slow) {
    if (stateFile) writeFileSync(stateFile, "done")
    setTimeout(() => {
      record({ event: "completed", method: req.method })
      handle(req)
    }, 5000)
    return
  }
  handle(req)
})

function handle(req) {
  switch (req.method) {
    case "ping":
      respond(req, { status: "ok", version: "0.0.0-fake" })
      break
    case "snapshot.create":
      created += 1
      if (mode === "null-result") {
        respond(req, null)
      } else if (mode === "array-result") {
        respond(req, [])
      } else if (mode === "scalar-result") {
        respond(req, "just a string")
      } else if (mode === "malformed-snapshot") {
        respond(req, { id: 42, commit: "0123456789ab" })
      } else {
        respond(req, {
          id: `20260101T000000.000000000Z-fake${process.pid}-${created}`,
          commit: "0123456789ab",
          reason: req.params?.reason ?? "manual",
          created_at: new Date().toISOString(),
          source: mode === "malformed-origin" ? 7 : req.params?.source,
          host: mode === "malformed-origin-host" ? 7 : req.params?.host,
          changed_files: [],
          reused: false,
        })
      }
      break
    case "snapshot.list":
      respond(req, { snapshots: [] })
      break
    case "snapshot.get":
      if (mode === "detail-not-found") {
        respond(req, null, { code: -32001, message: `snapshot not found: ${req.params?.id}` })
      } else if (mode === "detail-scalar") {
        respond(req, "just a string")
      } else {
        respond(req, { ...detail(req.params?.id), ...(malformedDetails[mode] ?? {}) })
      }
      break
    case "snapshot.diff":
      if (mode === "diff-not-found") {
        respond(req, null, { code: -32001, message: `snapshot not found: ${req.params?.from}` })
      } else if (mode === "diff-method-not-found") {
        respond(req, null, { code: -32601, message: "method not found" })
      } else if (mode === "diff-slow") {
        setTimeout(() => {
          record({ event: "completed", method: req.method })
          respond(req, diffResult(req.params))
        }, 5000)
      } else if (mode === "diff-scalar") {
        respond(req, "just a string")
      } else if (mode in malformedDiffs) {
        respond(req, malformedDiffs[mode](diffResult(req.params)))
      } else {
        respond(req, diffResult(req.params))
      }
      break
    default:
      if (mode === "bad-error") {
        respond(req, null, "boom")
      } else {
        respond(req, null, { code: -32601, message: "method not found" })
      }
  }
}

const malformedDetails = {
  "detail-missing-tree": { tree: undefined },
  "detail-missing-changed-files": { changed_files: undefined },
  "detail-bad-changed-files": { changed_files: [1] },
  "detail-bad-parent": { parent: 7 },
  "detail-bad-parent-id": { parent_id: null },
  "detail-bad-truncated": { changed_files_truncated: "yes" },
  "detail-bad-source": { source: 7 },
  "detail-bad-host": { host: null },
}

function detail(id) {
  return {
    id,
    commit: "0123456789ab0000000000000000000000000000",
    reason: "manual",
    created_at: "2026-01-01T00:00:00Z",
    tree: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
    parent: "fedcba9876540000000000000000000000000000",
    parent_id: "20251231T000000.000000000Z-fedcba987654",
    changed_files: ["src/a.ts"],
  }
}

const diffHunk = {
  old_start: 88,
  old_lines: 6,
  new_start: 88,
  new_lines: 7,
  section: "func dispatch() {",
  text: " \tcase \"snapshot.get\":\n+\tcase \"snapshot.diff\":\n \tdefault:\n\\ No newline at end of file\n",
}

function diffResult(params) {
  return {
    from: { id: params?.from, commit: "1a2b3c4d5e6f0000000000000000000000000000" },
    to: { id: params?.to, commit: "9f8e7d6c5b4a0000000000000000000000000000" },
    offset: params?.offset ?? 0,
    next_offset: (params?.offset ?? 0) + 2,
    totals: { files: 9, additions: 14, deletions: 5 },
    unexpected: "dropped",
    files: [
      {
        status: "added", path: "assets/logo.png", new_mode: "100644", new_oid: "23c62bf50000",
        additions: 0, deletions: 0, patch: "binary", hunks: [], unexpected: "dropped",
      },
      {
        status: "modified", path: "src/server.go", old_mode: "100644", new_mode: "100644",
        old_oid: "83db48f80000", new_oid: "8792505e0000", additions: 1, deletions: 0, patch: "full",
        hunks: [{ ...diffHunk, unexpected: "dropped" }],
      },
      {
        status: "renamed", path: "src/util.go", old_path: "src/helpers.go", similarity: 92,
        old_mode: "100644", new_mode: "100644", old_oid: "96cc55880000", new_oid: "1c5a36f50000",
        additions: 12, deletions: 5, patch: "deferred", hunks: [],
      },
    ],
  }
}

const withFile = (index, change) => (result) => ({
  ...result,
  files: result.files.map((file, i) => (i === index ? { ...file, ...change } : file)),
})

const withHunk = (change) => withFile(1, { hunks: [{ ...diffHunk, ...change }] })

const malformedDiffs = {
  "diff-bad-from": (r) => ({ ...r, from: { id: "x" } }),
  "diff-bad-to": (r) => ({ ...r, to: { id: 1, commit: "x" } }),
  "diff-missing-offset": (r) => ({ ...r, offset: undefined }),
  "diff-fractional-offset": (r) => ({ ...r, offset: 0.5 }),
  "diff-negative-offset": (r) => ({ ...r, offset: -1 }),
  "diff-unsafe-offset": (r) => ({ ...r, offset: 2 ** 53 }),
  "diff-null-next-offset": (r) => ({ ...r, next_offset: null }),
  "diff-next-offset-not-after-offset": (r) => ({ ...r, next_offset: r.offset }),
  "diff-next-offset-past-page": (r) => ({ ...r, next_offset: r.offset + 4 }),
  "diff-bad-totals": (r) => ({ ...r, totals: { files: 9, additions: "14", deletions: 5 } }),
  "diff-missing-totals": (r) => ({ ...r, totals: undefined }),
  "diff-page-past-totals": (r) => ({ ...r, totals: { ...r.totals, files: 2 } }),
  "diff-files-not-array": (r) => ({ ...r, files: {} }),
  "diff-too-many-files": (r) => ({
    ...r,
    totals: { ...r.totals, files: 1000 },
    next_offset: undefined,
    files: Array.from({ length: 201 }, () => r.files[0]),
  }),
  "diff-file-scalar": (r) => ({ ...r, next_offset: undefined, files: [7] }),
  "diff-bad-status": withFile(1, { status: "copied" }),
  "diff-bad-patch": withFile(1, { patch: "partial" }),
  "diff-bad-path": withFile(1, { path: null }),
  "diff-bad-mode": withFile(1, { new_mode: 100644 }),
  "diff-null-oid": withFile(1, { old_oid: null }),
  "diff-bad-additions": withFile(1, { additions: -1 }),
  "diff-bad-deletions": withFile(1, { deletions: 1.5 }),
  "diff-bad-lossy": withFile(1, { lossy: false }),
  "diff-null-lossy": withFile(1, { lossy: null }),
  "diff-renamed-without-old-path": withFile(2, { old_path: undefined }),
  "diff-renamed-without-similarity": withFile(2, { similarity: undefined }),
  "diff-similarity-too-low": withFile(2, { similarity: 49 }),
  "diff-similarity-too-high": withFile(2, { similarity: 101 }),
  "diff-similarity-fractional": withFile(2, { similarity: 92.5 }),
  "diff-old-path-on-modified": withFile(1, { old_path: "src/old.go" }),
  "diff-similarity-on-modified": withFile(1, { similarity: 90 }),
  "diff-added-with-old-mode": withFile(0, { old_mode: "100644" }),
  "diff-added-with-old-oid": withFile(0, { old_oid: "83db48f80000" }),
  "diff-added-without-new-mode": withFile(0, { new_mode: undefined }),
  "diff-modified-without-old-oid": withFile(1, { old_oid: undefined }),
  "diff-modified-without-old-mode": withFile(1, { old_mode: undefined }),
  "diff-deleted-with-new-oid": withFile(1, { status: "deleted" }),
  "diff-modified-without-new-oid": withFile(1, { new_oid: undefined }),
  "diff-binary-with-additions": withFile(0, { additions: 1 }),
  "diff-binary-with-deletions": withFile(0, { deletions: 1 }),
  "diff-binary-with-hunks": withFile(0, { hunks: [diffHunk] }),
  "diff-deferred-with-hunks": withFile(2, { hunks: [diffHunk] }),
  "diff-patch-bytes-on-full": withFile(1, { patch_bytes: 2048 }),
  "diff-patch-bytes-on-deferred": withFile(2, { patch_bytes: 2048 }),
  "diff-patch-bytes-fractional": withFile(1, { patch: "truncated", patch_bytes: 2048.5 }),
  "diff-patch-bytes-over-limit": withFile(1, { patch: "truncated", patch_bytes: 1048577 }),
  "diff-hunks-not-array": withFile(1, { hunks: "none" }),
  "diff-hunk-scalar": withFile(1, { hunks: ["@@"] }),
  "diff-hunk-bad-range": withHunk({ old_start: "88" }),
  "diff-hunk-negative-lines": withHunk({ new_lines: -1 }),
  "diff-hunk-bad-section": withHunk({ section: null }),
  "diff-hunk-bad-text": withHunk({ text: 7 }),
  "diff-hunk-text-without-newline": withHunk({ text: " tail" }),
  "diff-hunk-text-bad-prefix": withHunk({ text: " ok\nbad\n" }),
  "diff-hunk-text-empty-line": withHunk({ text: " ok\n\n" }),
}

function respond(req, result, error) {
  console.log(JSON.stringify({ id: req?.id ?? null, result, error }))
}
