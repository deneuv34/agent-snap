import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { SnapClient, SnapProtocolError, SnapRpcError } from "../src/index.js"
import { GROUP_WAIT_MS, WINDOWS_TREE_WAIT_MS, groupEmptyDecision, windowsTreeCompletionDecision } from "../src/teardown.js"

const fixture = fileURLToPath(new URL("./fixtures/fake-agent-snap.mjs", import.meta.url))

function client(extraEnv: Record<string, string> = {}, options: { timeoutMs?: number } = {}) {
  return new SnapClient({
    bin: process.execPath,
    serveArgs: [fixture, "serve"],
    env: extraEnv,
    timeoutMs: options.timeoutMs,
  })
}

test("ping round-trips", async () => {
  const c = client()
  try {
    const result = await c.ping()
    assert.equal(result.status, "ok")
    assert.ok(result.version.length > 0)
  } finally {
    await c.close()
  }
})

test("create returns a typed snapshot", async () => {
  const c = client()
  try {
    const snap = await c.create("/some/repo", "unit-test")
    assert.ok(snap.id.length > 0)
    assert.ok(snap.commit.length > 0)
    assert.equal(snap.reason, "unit-test")
    assert.equal(snap.reused, false)
  } finally {
    await c.close()
  }
})

test("list returns an array", async () => {
  const c = client()
  try {
    const snapshots = await c.list("/some/repo", 5)
    assert.deepEqual(snapshots, [])
  } finally {
    await c.close()
  }
})

const snapshotID = "20260101T000000.000000000Z-0123456789ab"

test("get sends exactly cwd and id and returns a typed detail", async () => {
  const log = join(mkdtempSync(join(tmpdir(), "agent-snap-get-")), "log.jsonl")
  const c = client({ FAKE_AGENT_SNAP_LOG: log })
  try {
    const detail = await c.get("/some/repo", snapshotID)
    assert.deepEqual(detail, {
      id: snapshotID,
      commit: "0123456789ab0000000000000000000000000000",
      reason: "manual",
      created_at: "2026-01-01T00:00:00Z",
      tree: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
      parent: "fedcba9876540000000000000000000000000000",
      parent_id: "20251231T000000.000000000Z-fedcba987654",
      changed_files: ["src/a.ts"],
      changed_files_truncated: undefined,
    })
  } finally {
    await c.close()
  }
  const requests = readFileSync(log, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line) as { event: string; method: string; params: unknown })
    .filter((entry) => entry.event === "request")
    .map(({ method, params }) => ({ method, params }))
  assert.deepEqual(requests, [{ method: "snapshot.get", params: { cwd: "/some/repo", id: snapshotID } }])
})

test("a missing snapshot surfaces SnapRpcError -32001 with the server message", async () => {
  const c = client({ FAKE_AGENT_SNAP_MODE: "detail-not-found" })
  try {
    await assert.rejects(
      () => c.get("/repo", snapshotID),
      (err: unknown) => err instanceof SnapRpcError && err.code === -32001 && err.message === `snapshot not found: ${snapshotID}`,
    )
  } finally {
    await c.close()
  }
})

test("malformed snapshot.get results surface SnapProtocolError", async () => {
  for (const mode of [
    "detail-scalar",
    "detail-missing-tree",
    "detail-missing-changed-files",
    "detail-bad-changed-files",
    "detail-bad-parent",
    "detail-bad-parent-id",
    "detail-bad-truncated",
  ]) {
    const c = client({ FAKE_AGENT_SNAP_MODE: mode })
    try {
      await assert.rejects(
        () => c.get("/repo", snapshotID),
        (err: unknown) => err instanceof SnapProtocolError,
        `mode ${mode} must produce a SnapProtocolError`,
      )
    } finally {
      await c.close()
    }
  }
})

test("unknown method surfaces a SnapRpcError with the wire code", async () => {
  const c = client()
  try {
    await assert.rejects(
      () => c.request("bogus.method"),
      (err: unknown) => err instanceof SnapRpcError && err.code === -32601,
    )
  } finally {
    await c.close()
  }
})

test("client respawns lazily after close", async () => {
  const c = client()
  try {
    const first = await c.create("/repo")
    await c.close()
    const second = await c.create("/repo")
    assert.equal(second.reused, false)
    assert.notEqual(first.id, second.id)
  } finally {
    await c.close()
  }
})

test("in-flight request rejects when the child exits", async () => {
  const c = client({ FAKE_AGENT_SNAP_MODE: "exit-on-list" })
  const pending = c.list("/repo")
  await assert.rejects(() => pending, /exited unexpectedly/)
  // POSIX: the dead child's process group is empty, so close() succeeds.
  // Windows: the tree of a root that exited on its own cannot be terminated,
  // so close() fails closed instead of reporting success.
  if (process.platform === "win32") {
    await assert.rejects(() => c.close(), /process tree/)
  } else {
    await c.close()
  }
})

test("close fails closed on Windows when the root has already exited", async () => {
  const c = client({ FAKE_AGENT_SNAP_MODE: "exit-on-ping" })
  await assert.rejects(() => c.ping(), /exited unexpectedly/)
  if (process.platform === "win32") {
    await assert.rejects(() => c.close(), /process tree/)
  } else {
    await c.close()
  }
})

test(
  "close awaits the teardown a timeout started, instead of racing it",
  { skip: process.platform === "win32" ? "process-group semantics are POSIX-only" : false },
  async () => {
    const dir = mkdtempSync(join(tmpdir(), "agent-snap-timeout-close-"))
    const log = join(dir, "fake.log")
    const c = client({ FAKE_AGENT_SNAP_MODE: "slow-create-grandchild", FAKE_AGENT_SNAP_LOG: log }, { timeoutMs: 300 })
    await assert.rejects(() => c.create("/repo", "never-finishes"), /timed out/)
    const start = Date.now()
    await c.close()
    const elapsed = Date.now() - start
    const raw = readFileSync(log, "utf8")
    assert.ok(
      raw.includes("grandchild-exited"),
      `close() must wait for the teardown the timeout started; grandchild was still alive: ${JSON.stringify(raw)}`,
    )
    assert.ok(elapsed >= 15, `close() returned after ${elapsed}ms — it must await the in-flight teardown`)
  },
)

test("group emptiness decision: empty completes, errors fail closed, deadline fails", () => {
  assert.equal(groupEmptyDecision({ state: "empty", elapsedMs: 0 }), "done")
  assert.equal(groupEmptyDecision({ state: "empty", elapsedMs: GROUP_WAIT_MS + 1 }), "done")
  assert.equal(groupEmptyDecision({ state: "error", elapsedMs: 0 }), "fail")
  assert.equal(groupEmptyDecision({ state: "live", elapsedMs: 0 }), "wait")
  assert.equal(groupEmptyDecision({ state: "live", elapsedMs: GROUP_WAIT_MS - 1 }), "wait")
  assert.equal(groupEmptyDecision({ state: "live", elapsedMs: GROUP_WAIT_MS }), "fail")
  assert.equal(groupEmptyDecision({ state: "live", elapsedMs: GROUP_WAIT_MS + 1 }), "fail")
})

test("windows tree decision: needs BOTH taskkill success and root exit", () => {
  // nothing yet → wait
  assert.equal(windowsTreeCompletionDecision({ taskkillDone: false, taskkillFailed: false, rootExited: false, elapsedMs: 0 }), "wait")
  // taskkill done but root not exited → wait
  assert.equal(windowsTreeCompletionDecision({ taskkillDone: true, taskkillFailed: false, rootExited: false, elapsedMs: 100 }), "wait")
  // root exited but taskkill not done → wait
  assert.equal(windowsTreeCompletionDecision({ taskkillDone: false, taskkillFailed: false, rootExited: true, elapsedMs: 100 }), "wait")
  // both → done
  assert.equal(windowsTreeCompletionDecision({ taskkillDone: true, taskkillFailed: false, rootExited: true, elapsedMs: 100 }), "done")
  // taskkill failure rejects even when the root has exited
  assert.equal(windowsTreeCompletionDecision({ taskkillDone: false, taskkillFailed: true, rootExited: true, elapsedMs: 100 }), "fail")
  assert.equal(windowsTreeCompletionDecision({ taskkillDone: false, taskkillFailed: true, rootExited: false, elapsedMs: 100 }), "fail")
  // window expiry without both conditions → fail, never a silent success
  assert.equal(windowsTreeCompletionDecision({ taskkillDone: true, taskkillFailed: false, rootExited: false, elapsedMs: WINDOWS_TREE_WAIT_MS }), "fail")
  assert.equal(windowsTreeCompletionDecision({ taskkillDone: false, taskkillFailed: false, rootExited: true, elapsedMs: WINDOWS_TREE_WAIT_MS }), "fail")
})

test("requests during close reject instead of spawning outside the barrier", async () => {
  const c = client({ FAKE_AGENT_SNAP_MODE: "slow-create" })
  const pendingCreate = c.create("/repo", "hangs")
  await new Promise((resolve) => setTimeout(resolve, 50))
  const closing = c.close()
  await assert.rejects(() => c.ping(), /client is closing/)
  await assert.rejects(() => pendingCreate, /client closed/)
  await closing
  const ping = await c.ping()
  assert.equal(ping.status, "ok")
  await c.close()
})

test(
  "teardowns of different children run concurrently, not serialized behind grace periods",
  { skip: process.platform === "win32" ? "process-group semantics are POSIX-only" : false },
  async () => {
    const dir = mkdtempSync(join(tmpdir(), "agent-snap-concurrent-"))
    const log = join(dir, "fake.log")
    const c = client({ FAKE_AGENT_SNAP_MODE: "stubborn-create-grandchild", FAKE_AGENT_SNAP_LOG: log }, { timeoutMs: 300 })
    // First child times out and its teardown starts immediately (guardian
    // grace timer running). The second request spawns a replacement child,
    // which also times out. close() must drain both without delaying the
    // second teardown behind the first one's grace window.
    await assert.rejects(() => c.create("/repo", "first"), /timed out/)
    await assert.rejects(() => c.create("/repo", "second"), /timed out/)
    const start = Date.now()
    await c.close()
    const elapsed = Date.now() - start
    // Concurrent: the second guardian's grace (~2s) dominates. Serialized
    // behind the first teardown it would be ~4s+.
    assert.ok(elapsed < 3_200, `teardowns appear serialized: close() took ${elapsed}ms`)
    const raw = readFileSync(log, "utf8")
    const sawTerm = (raw.match(/grandchild-saw-term/g) ?? []).length
    assert.equal(sawTerm, 2, `both stubborn grandchildren must receive SIGTERM, log: ${JSON.stringify(raw)}`)
  },
)

test(
  "cleanup failures latch until verified: repeated close never fakes success",
  { skip: process.platform === "win32" ? "guardian semantics are POSIX-only" : false },
  async () => {
    const dir = mkdtempSync(join(tmpdir(), "agent-snap-latch-"))
    const log = join(dir, "fake.log")
    const c = client({ FAKE_AGENT_SNAP_MODE: "survive-eof", FAKE_AGENT_SNAP_LOG: log })
    await c.ping()
    const guardianPid = c.pid
    const firstLine = readFileSync(log, "utf8").trim().split("\n")[0]
    const fakePid = JSON.parse(firstLine).pid as number
    assert.ok(guardianPid !== undefined && fakePid > 0)
    // Kill the supervisor out from under the client: the tree (the fake)
    // survives, so cleanup cannot be verified and must fail closed.
    process.kill(guardianPid, "SIGKILL")
    await assert.rejects(() => c.close(), /did not terminate|supervisor/)
    // The failure latches: a second close must not report success.
    await assert.rejects(() => c.close(), /did not terminate|supervisor/)
    // Once the surviving tree is actually gone, a retry verifies emptiness
    // and the latch clears.
    process.kill(fakePid, "SIGKILL")
    await new Promise((resolve) => setTimeout(resolve, 500))
    await c.close()
  },
)

test("aborted request tears down the transport and rejects with AbortError", async () => {
  const dir = mkdtempSync(join(tmpdir(), "agent-snap-abort-test-"))
  const log = join(dir, "fake.log")
  const c = client({ FAKE_AGENT_SNAP_MODE: "slow-create", FAKE_AGENT_SNAP_LOG: log })
  const controller = new AbortController()
  try {
    const pending = c.create("/repo", "aborted", controller.signal)
    setTimeout(() => controller.abort(), 150)
    await assert.rejects(() => pending, (err: unknown) => (err as Error).name === "AbortError")
  } finally {
    await c.close()
  }
  const entries = readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line) as { event: string })
  assert.ok(!entries.some((entry) => entry.event === "completed"), "aborted create must never complete")
})

test(
  "close waits for the whole process group, including grandchildren",
  { skip: process.platform === "win32" ? "process-group semantics are POSIX-only" : false },
  async () => {
    const dir = mkdtempSync(join(tmpdir(), "agent-snap-group-test-"))
    const log = join(dir, "fake.log")
    const c = client({ FAKE_AGENT_SNAP_MODE: "grandchild", FAKE_AGENT_SNAP_LOG: log })
    await c.ping()
    await c.close()
    const raw = readFileSync(log, "utf8")
    assert.ok(
      raw.includes("grandchild-exited"),
      `close() resolved before the grandchild was terminated; raw log: ${JSON.stringify(raw)}`,
    )
  },
)

test("missing binary rejects with an actionable message", async () => {
  const c = new SnapClient({ bin: "/definitely/not/a/real/agent-snap-binary" })
  try {
    await assert.rejects(() => c.ping(), /binary not found/)
  } finally {
    await c.close()
  }
})

test("timed-out request kills the transport and cannot produce a late snapshot", async () => {
  const dir = mkdtempSync(join(tmpdir(), "agent-snap-client-test-"))
  const log = join(dir, "fake.log")
  const state = join(dir, "fake.state")
  const c = client(
    { FAKE_AGENT_SNAP_MODE: "slow-first-then-fast", FAKE_AGENT_SNAP_LOG: log, FAKE_AGENT_SNAP_STATE: state },
    { timeoutMs: 300 },
  )
  try {
    await assert.rejects(() => c.create("/repo", "slow"), /timed out/)
    const snap = await c.create("/repo", "fast-after-restart")
    assert.ok(snap.id.length > 0)
  } finally {
    await c.close()
  }

  const entries = readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line) as { pid: number; event: string; method?: string })
  const pids = new Set(entries.map((entry) => entry.pid))
  assert.ok(pids.size >= 2, `expected transport restart (new pid), got pids: ${[...pids].join(",")}`)
  const slowPid = [...pids][0] ?? 0
  assert.ok(
    entries.some((entry) => entry.pid === slowPid && entry.event === "request" && entry.method === "snapshot.create"),
    "the slow request must have reached the first child",
  )
  assert.ok(
    !entries.some((entry) => entry.pid === slowPid && entry.event === "completed"),
    "the timed-out request must never complete (no late snapshot)",
  )
})

test("close terminates a child that ignores SIGTERM within the grace period", async () => {
  const dir = mkdtempSync(join(tmpdir(), "agent-snap-close-test-"))
  const log = join(dir, "fake.log")
  const c = client({ FAKE_AGENT_SNAP_MODE: "trap-term", FAKE_AGENT_SNAP_LOG: log })
  await c.ping()
  const start = Date.now()
  await c.close()
  const elapsed = Date.now() - start
  const entries = readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line) as { event: string })

  if (process.platform === "win32") {
    // Windows has no SIGTERM escalation: taskkill /T /F force-kills the
    // tree immediately. The safety contract is that close() resolves
    // promptly and the child is gone — there is no grace period to wait.
    assert.ok(elapsed < 1500, `windows force-kill should be immediate, took ${elapsed}ms`)
    assert.ok(!entries.some((entry) => entry.event === "sigterm"), "SIGTERM is not deliverable on Windows")
  } else {
    // Unix: SIGTERM first (the child traps it and delays exit), then
    // SIGKILL after the grace period — close must wait for the escalation.
    assert.ok(elapsed >= 1500, `close returned too early: ${elapsed}ms`)
    assert.ok(elapsed < 4500, `close did not force-kill: ${elapsed}ms`)
    assert.ok(entries.some((entry) => entry.event === "sigterm"), "SIGTERM must be delivered")
  }
})

test("non-object or garbage child output is rejected or ignored safely", async () => {
  const c = client({ FAKE_AGENT_SNAP_MODE: "garbage-line" })
  try {
    const result = await c.ping()
    assert.equal(result.status, "ok")
  } finally {
    await c.close()
  }
})

test("null, array, and scalar results surface SnapProtocolError", async () => {
  for (const mode of ["null-result", "array-result", "scalar-result", "malformed-snapshot"]) {
    const c = client({ FAKE_AGENT_SNAP_MODE: mode })
    try {
      await assert.rejects(
        () => c.create("/repo"),
        (err: unknown) => err instanceof SnapProtocolError,
        `mode ${mode} must produce a SnapProtocolError`,
      )
    } finally {
      await c.close()
    }
  }
})

test("malformed error objects surface SnapProtocolError", async () => {
  const c = client({ FAKE_AGENT_SNAP_MODE: "bad-error" })
  try {
    await assert.rejects(
      () => c.request("bogus"),
      (err: unknown) => err instanceof SnapProtocolError,
    )
  } finally {
    await c.close()
  }
})
