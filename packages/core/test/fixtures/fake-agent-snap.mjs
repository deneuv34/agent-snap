import { appendFileSync, existsSync, readFileSync, writeFileSync } from "node:fs"
import { spawn } from "node:child_process"

const mode = process.env.FAKE_AGENT_SNAP_MODE ?? ""
const logFile = process.env.FAKE_AGENT_SNAP_LOG
const stateFile = process.env.FAKE_AGENT_SNAP_STATE
const snapshotID = process.env.FAKE_AGENT_SNAP_ID

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

if (mode === "record-env") {
  record({ event: "env", guardian: process.env.AGENT_SNAP_GUARDIAN ?? null, bunBeBun: process.env.BUN_BE_BUN ?? null })
}

let created = 0
let grandchildSpawned = false

let unread = ""
process.stdin.setEncoding("utf8")
process.stdin.on("data", (chunk) => {
  unread += chunk
  for (let end = unread.indexOf("\n"); end !== -1; end = unread.indexOf("\n")) {
    const line = unread.slice(0, end)
    unread = unread.slice(end + 1)
    onLine(line)
  }
})

function onLine(line) {
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
}

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
          id: snapshotID ?? `20260101T000000.${String(process.pid).padStart(6, "0").slice(-6)}${String(created).padStart(3, "0").slice(-3)}Z-0123456789ab`,
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
      respond(req, { snapshots: snapshotID === undefined ? [] : listed(snapshotID) })
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
    case "snapshot.restore_plan":
      if (mode === "plan-not-found") {
        respond(req, null, { code: -32001, message: `snapshot not found: ${req.params?.id}` })
      } else if (mode === "plan-stale") {
        respond(req, null, { code: -32002, message: "working tree changed since the plan" })
      } else if (mode === "plan-invalid-params") {
        respond(req, null, { code: -32602, message: "param must be an object id: current_tree" })
      } else if (mode === "plan-method-not-found") {
        respond(req, null, { code: -32601, message: "method not found" })
      } else if (mode === "plan-slow") {
        setTimeout(() => {
          record({ event: "completed", method: req.method })
          respond(req, planResult(req.params))
        }, 5000)
      } else if (mode === "plan-scalar") {
        respond(req, "just a string")
      } else if (mode in validPlans) {
        respond(req, validPlans[mode](planResult(req.params)))
      } else if (mode in malformedPlans) {
        respond(req, malformedPlans[mode](planResult(req.params)))
      } else {
        respond(req, planResult(req.params))
      }
      break
    default:
      if (mode === "bad-error") {
        respond(req, null, "boom")
      } else if (mode === "unknown-method-result") {
        respond(req, { status: "ok" })
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
    parent_id: snapshotID ?? "20251231T000000.000000000Z-fedcba987654",
    changed_files: ["src/a.ts"],
  }
}

function listed(id) {
  return [
    { id: "20260102T000000.000000000Z-0123456789ab", commit: "0123456789ab".padEnd(40, "0"), reason: "sha1", created_at: "2026-01-02T00:00:00Z" },
    { id, commit: "fedcba987654".padEnd(64, "0"), reason: "sha256", created_at: "2026-01-01T00:00:00Z" },
  ]
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

const hex = (digit, length = 40) => digit.repeat(length)
const side = (mode, digit, length = 40) => ({ mode, oid: hex(digit, length) })

function planResult(params) {
  return {
    snapshot: { id: params?.id, commit: hex("1"), tree: hex("2") },
    ...(params?.paths === undefined ? {} : { paths: params.paths }),
    current_tree: params?.current_tree ?? hex("3"),
    result_tree: hex("4"),
    applicable: false,
    blockers: [],
    unmatched: [],
    offset: params?.offset ?? 0,
    totals: { entries: 4, create: 1, modify: 1, type_change: 1, delete: 1, staged: 1, unstaged: 1, untracked: 1, blocked: 1 },
    unexpected: "dropped",
    entries: (params?.offset ?? 0) > 0 ? [] : [
      { path: "src/cache.log", action: "create", target: side("100644", "5"), blocker: "untracked_in_the_way", unexpected: "dropped" },
      { path: "src/link", action: "type_change", current: side("100644", "6"), target: side("120000", "7"), staged: true },
      { path: "src/new.go", action: "delete", current: side("100644", "8"), untracked: true },
      { path: "src/server.go", action: "modify", current: side("100644", "9"), target: side("100755", "a"), unstaged: true },
    ],
  }
}

const withEntry = (index, change) => (plan) => ({
  ...plan,
  entries: plan.entries.map((entry, i) => (i === index ? { ...entry, ...change } : entry)),
})

const widen = (value) =>
  typeof value === "string" && /^[0-9a-f]{40}$/.test(value)
    ? value + value.slice(0, 24)
    : Array.isArray(value)
      ? value.map(widen)
      : value !== null && typeof value === "object"
        ? Object.fromEntries(Object.entries(value).map(([key, inner]) => [key, key === "commit" ? inner : widen(inner)]))
        : value

const applicableEmpty = (plan) => ({
  ...plan,
  applicable: true,
  totals: { entries: 0, create: 0, modify: 0, type_change: 0, delete: 0, staged: 0, unstaged: 0, untracked: 0, blocked: 0 },
  entries: [],
})

const pagedTotals = { entries: 9, create: 4, modify: 3, type_change: 1, delete: 1, staged: 1, unstaged: 1, untracked: 1, blocked: 1 }

const twoCreates = (first, second) => (p) => ({
  ...p,
  applicable: true,
  totals: { entries: 2, create: 2, modify: 0, type_change: 0, delete: 0, staged: 0, unstaged: 0, untracked: 0, blocked: 0 },
  entries: [
    { path: first, action: "create", target: side("100644", "5") },
    { path: second, action: "create", target: side("100644", "5") },
  ],
})

const validPlans = {
  "plan-sha256": widen,
  "plan-paged": (p) => ({ ...p, totals: pagedTotals, next_offset: 4 }),
  "plan-utf8-byte-order": twoCreates("\uFF5E", "\u{1F600}"),
  "plan-empty-applicable": applicableEmpty,
  "plan-unmatched": (p) => ({ ...p, paths: ["src", "zzz"], unmatched: ["zzz"] }),
  "plan-repo-blockers": (p) => ({ ...p, blockers: ["operation_in_progress", "sparse_checkout"] }),
  "plan-no-result-tree": (p) => ({ ...p, result_tree: undefined }),
  "plan-repaired-paths": (p) => ({
    ...p,
    totals: { ...p.totals, entries: 4, create: 3, modify: 0, type_change: 0, delete: 1, staged: 0, unstaged: 0, untracked: 1, blocked: 3 },
    entries: [
      { path: "a\uFFFD", action: "create", target: side("100644", "5"), blocker: "unaddressable_path" },
      { path: "a", action: "create", target: side("100644", "5"), blocker: "untracked_in_the_way" },
      { path: "a\uFFFD", action: "create", target: side("100644", "5"), blocker: "unaddressable_path" },
      { path: "b", action: "delete", current: side("100644", "8"), untracked: true },
    ],
  }),
}

const malformedPlans = {
  "plan-bad-snapshot": (p) => ({ ...p, snapshot: { id: p.snapshot.id, tree: p.snapshot.tree } }),
  "plan-snapshot-short-tree": (p) => ({ ...p, snapshot: { ...p.snapshot, tree: "2222" } }),
  "plan-missing-current-tree": (p) => ({ ...p, current_tree: undefined }),
  "plan-uppercase-current-tree": (p) => ({ ...p, current_tree: hex("A") }),
  "plan-short-current-tree": (p) => ({ ...p, current_tree: hex("3", 39) }),
  "plan-mixed-oid-length": (p) => ({ ...p, result_tree: hex("4", 64) }),
  "plan-bad-result-tree": (p) => ({ ...p, result_tree: "nothex" }),
  "plan-null-result-tree": (p) => ({ ...p, result_tree: null }),
  "plan-side-short-oid": withEntry(0, { target: { mode: "100644", oid: "5555" } }),
  "plan-side-mixed-oid-length": withEntry(0, { target: side("100644", "5", 64) }),
  "plan-side-short-mode": withEntry(0, { target: side("644", "5") }),
  "plan-side-non-octal-mode": withEntry(0, { target: side("100648", "5") }),
  "plan-side-numeric-mode": withEntry(0, { target: { mode: 100644, oid: hex("5") } }),
  "plan-side-scalar": withEntry(0, { target: "100644" }),
  "plan-paths-empty": (p) => ({ ...p, paths: [] }),
  "plan-paths-not-strings": (p) => ({ ...p, paths: [1] }),
  "plan-paths-null": (p) => ({ ...p, paths: null }),
  "plan-paths-too-many": (p) => ({ ...p, paths: Array.from({ length: 101 }, (_, i) => `p${i}`) }),
  "plan-paths-empty-segment": (p) => ({ ...p, paths: ["src//a"] }),
  "plan-paths-dot-segment": (p) => ({ ...p, paths: ["src/./a"] }),
  "plan-paths-dotdot-segment": (p) => ({ ...p, paths: ["../a"] }),
  "plan-paths-nul": (p) => ({ ...p, paths: ["a\u0000b"] }),
  "plan-paths-too-long": (p) => ({ ...p, paths: ["a".repeat(4097)] }),
  "plan-paths-duplicate": (p) => ({ ...p, paths: ["src", "src"] }),
  "plan-paths-overlap": (p) => ({ ...p, paths: ["src", "src/a"] }),
  "plan-applicable-not-boolean": (p) => ({ ...p, applicable: "false" }),
  "plan-applicable-with-entry-blockers": (p) => ({ ...p, applicable: true }),
  "plan-applicable-with-repo-blockers": (p) => ({ ...applicableEmpty(p), blockers: ["unmerged_index"] }),
  "plan-applicable-with-unmatched": (p) => ({ ...applicableEmpty(p), paths: ["zzz"], unmatched: ["zzz"] }),
  "plan-applicable-without-result-tree": (p) => ({ ...applicableEmpty(p), result_tree: undefined }),
  "plan-not-applicable-when-clean": (p) => ({ ...applicableEmpty(p), applicable: false }),
  "plan-blockers-not-array": (p) => ({ ...p, blockers: "none" }),
  "plan-blockers-null": (p) => ({ ...p, blockers: null }),
  "plan-unknown-repo-blocker": (p) => ({ ...p, blockers: ["detached_head"] }),
  "plan-path-blocker-as-repo-blocker": (p) => ({ ...p, blockers: ["submodule"] }),
  "plan-duplicate-repo-blocker": (p) => ({ ...p, blockers: ["unmerged_index", "unmerged_index"] }),
  "plan-repo-blockers-out-of-order": (p) => ({ ...p, blockers: ["sparse_checkout", "operation_in_progress"] }),
  "plan-unmatched-not-array": (p) => ({ ...p, unmatched: "none" }),
  "plan-unmatched-not-strings": (p) => ({ ...p, paths: ["src"], unmatched: [1] }),
  "plan-unmatched-without-paths": (p) => ({ ...p, unmatched: ["src"] }),
  "plan-unmatched-outside-paths": (p) => ({ ...p, paths: ["src"], unmatched: ["zzz"] }),
  "plan-missing-offset": (p) => ({ ...p, offset: undefined }),
  "plan-negative-offset": (p) => ({ ...p, offset: -1 }),
  "plan-fractional-offset": (p) => ({ ...p, offset: 0.5 }),
  "plan-null-next-offset": (p) => ({ ...p, next_offset: null }),
  "plan-next-offset-missing": (p) => ({ ...p, totals: pagedTotals }),
  "plan-next-offset-short": (p) => ({ ...p, totals: pagedTotals, next_offset: 3 }),
  "plan-next-offset-long": (p) => ({ ...p, totals: pagedTotals, next_offset: 5 }),
  "plan-next-offset-on-last-page": (p) => ({ ...p, next_offset: 4 }),
  "plan-next-offset-on-past-end-page": (p) => ({ ...p, offset: 9, entries: [], next_offset: 9 }),
  "plan-page-past-totals": (p) => ({ ...p, offset: 1 }),
  "plan-too-many-entries": (p) => ({
    ...p,
    totals: { ...p.totals, entries: 1000, create: 1000, modify: 0, type_change: 0, delete: 0 },
    next_offset: 201,
    entries: Array.from({ length: 201 }, (_, i) => ({ ...p.entries[0], path: `f${String(i).padStart(4, "0")}` })),
  }),
  "plan-entries-not-array": (p) => ({ ...p, entries: {} }),
  "plan-totals-missing": (p) => ({ ...p, totals: undefined }),
  "plan-totals-string": (p) => ({ ...p, totals: { ...p.totals, create: "1" } }),
  "plan-totals-negative": (p) => ({ ...p, totals: { ...p.totals, blocked: -1 } }),
  "plan-totals-missing-key": (p) => ({ ...p, totals: { ...p.totals, untracked: undefined } }),
  "plan-totals-sum-mismatch": (p) => ({ ...p, totals: { ...p.totals, delete: 2 } }),
  "plan-totals-staged-over-entries": (p) => ({ ...p, totals: { ...p.totals, staged: 5 } }),
  "plan-totals-unstaged-over-entries": (p) => ({ ...p, totals: { ...p.totals, unstaged: 5 } }),
  "plan-totals-untracked-over-entries": (p) => ({ ...p, totals: { ...p.totals, untracked: 5 } }),
  "plan-totals-blocked-over-entries": (p) => ({ ...p, totals: { ...p.totals, blocked: 5 } }),
  "plan-entry-scalar": (p) => ({ ...p, entries: [7] }),
  "plan-entry-bad-action": withEntry(1, { action: "rename" }),
  "plan-entry-empty-path": withEntry(0, { path: "" }),
  "plan-entry-numeric-path": withEntry(0, { path: 7 }),
  "plan-create-with-current": withEntry(0, { current: side("100644", "6") }),
  "plan-create-without-target": withEntry(0, { target: undefined }),
  "plan-delete-with-target": withEntry(2, { target: side("100644", "5") }),
  "plan-delete-without-current": withEntry(2, { current: undefined }),
  "plan-modify-without-current": withEntry(3, { current: undefined }),
  "plan-modify-without-target": withEntry(3, { target: undefined }),
  "plan-null-current": withEntry(1, { current: null }),
  "plan-unstaged-and-untracked": withEntry(2, { unstaged: true }),
  "plan-flag-false": withEntry(1, { staged: false }),
  "plan-flag-null": withEntry(1, { staged: null }),
  "plan-unknown-path-blocker": withEntry(0, { blocker: "ignored" }),
  "plan-repo-blocker-on-entry": withEntry(0, { blocker: "sparse_checkout" }),
  "plan-null-path-blocker": withEntry(0, { blocker: null }),
  "plan-unaddressable-without-repair": withEntry(0, { blocker: "unaddressable_path" }),
  "plan-entries-out-of-order": (p) => ({ ...p, entries: [p.entries[1], p.entries[0], p.entries[2], p.entries[3]] }),
  "plan-entries-duplicate-path": withEntry(1, { path: "src/cache.log" }),
  "plan-utf8-byte-order-reversed": twoCreates("\u{1F600}", "\uFF5E"),
}

function respond(req, result, error) {
  console.log(JSON.stringify({ id: req?.id ?? null, result, error }).replace(/[\u2028\u2029]/g, (c) => `\\u${c.charCodeAt(0).toString(16)}`))
}
