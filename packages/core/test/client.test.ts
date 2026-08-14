import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { SnapClient, SnapProtocolError, SnapRpcError } from "../src/index.js"

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
  try {
    await assert.rejects(() => c.list("/repo"), /exited unexpectedly/)
  } finally {
    await c.close()
  }
})

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
  assert.ok(elapsed >= 1500, `close returned too early: ${elapsed}ms`)
  assert.ok(elapsed < 4500, `close did not force-kill: ${elapsed}ms`)
  const entries = readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line) as { event: string })
  assert.ok(entries.some((entry) => entry.event === "sigterm"), "SIGTERM must be delivered")
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
