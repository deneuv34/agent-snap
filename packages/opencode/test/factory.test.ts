import { test } from "node:test"
import assert from "node:assert/strict"
import type { SnapLike, SnapSnapshot, SnapSnapshotDetail } from "@agent-snap/core"
import { createAgentSnapPlugin } from "../src/factory.js"

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

interface Deferred {
  promise: Promise<void>
  resolve: () => void
}

function deferred(): Deferred {
  let resolve!: () => void
  const promise = new Promise<void>((r) => {
    resolve = r
  })
  return { promise, resolve }
}

interface CreateCall {
  cwd: string
  reason?: string
  signal?: AbortSignal
}

function fakeClient() {
  const creates: CreateCall[] = []
  const gates: Deferred[] = []
  let closed = false
  const client: SnapLike = {
    async create(cwd, reason, signal) {
      creates.push({ cwd, reason, signal })
      const gate = deferred()
      gates.push(gate)
      await new Promise<void>((resolve, reject) => {
        if (signal?.aborted) {
          reject(new DOMException("aborted", "AbortError"))
          return
        }
        const onAbort = () => reject(new DOMException("aborted", "AbortError"))
        signal?.addEventListener("abort", onAbort, { once: true })
        gate.promise.then(() => {
          signal?.removeEventListener("abort", onAbort)
          resolve()
        })
      })
      const snap: SnapSnapshot = {
        id: `snap-${creates.length}`,
        commit: "0123456789ab",
        reason: reason ?? "manual",
        created_at: new Date().toISOString(),
        reused: false,
      }
      return snap
    },
    async list() {
      return []
    },
    async get() {
      throw new Error("unexpected get")
    },
    async close() {
      closed = true
    },
  }
  return { client, creates, gates, closed: () => closed }
}

function detailClient(detail: SnapSnapshotDetail) {
  const gets: Array<{ cwd: string; id: string; signal?: AbortSignal }> = []
  const client: SnapLike = {
    async create() {
      throw new Error("unexpected create")
    },
    async list() {
      return []
    },
    async get(cwd, id, signal) {
      gets.push({ cwd, id, signal })
      return detail
    },
    async close() {},
  }
  return { client, gets }
}

const detail: SnapSnapshotDetail = {
  id: "20261028T120000.000000000Z-0123456789ab",
  commit: "0123456789ab",
  reason: "edit",
  created_at: "2026-10-28T12:00:00Z",
  tree: "4b825dc642cb",
  parent: "fedcba987654",
  parent_id: "20261028T115500.000000000Z-fedcba987654",
  changed_files: ["src/a.ts", "src/b.ts"],
}

function releaseAll(gates: Deferred[]) {
  for (const gate of gates) gate.resolve()
  gates.length = 0
}

function createdEvent(id: string, directory: string) {
  return { type: "session.created", properties: { info: { id, directory } } } as never
}

function idleEvent(sessionID: string) {
  return { type: "session.idle", properties: { sessionID } } as never
}

function statusEvent(sessionID: string, status: "busy" | "idle") {
  return { type: "session.status", properties: { sessionID, status: { type: status } } } as never
}

function deletedEvent(id: string) {
  return { type: "session.deleted", properties: { info: { id } } } as never
}

test("exposes create, list, and get tools", async () => {
  const { client } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  assert.ok(plugin.tool?.agent_snap_create)
  assert.ok(plugin.tool?.agent_snap_list)
  assert.ok(plugin.tool?.agent_snap_get)
  await plugin.dispose?.()
})

test("agent_snap_get describes a snapshot and passes cwd, id, and the abort signal", async () => {
  const { client, gets } = detailClient(detail)
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback" })
  const abort = new AbortController().signal
  try {
    const text = await plugin.tool?.agent_snap_get.execute({ id: detail.id }, { directory: "/repo", abort } as never)
    assert.equal(
      text,
      "Snapshot 20261028T120000.000000000Z-0123456789ab\ncommit: 0123456789ab\ntree: 4b825dc642cb\nreason: edit\n" +
        "created: 2026-10-28T12:00:00Z\nparent: fedcba987654 (snapshot 20261028T115500.000000000Z-fedcba987654)\n" +
        "changed files:\n  src/a.ts\n  src/b.ts",
    )
    await plugin.tool?.agent_snap_get.execute({ id: detail.id, cwd: "/other" }, { directory: "/repo", abort } as never)
    assert.deepEqual(gets, [
      { cwd: "/repo", id: detail.id, signal: abort },
      { cwd: "/other", id: detail.id, signal: abort },
    ])
  } finally {
    await plugin.dispose?.()
  }
})

test("agent_snap_get labels commit parents, root snapshots, empty and truncated file lists", async () => {
  const cases: Array<[Partial<SnapSnapshotDetail>, string]> = [
    [{ parent_id: undefined }, "parent: fedcba987654 (commit)\nchanged files:\n  src/a.ts\n  src/b.ts"],
    [{ parent: undefined, parent_id: undefined, changed_files: [] }, "parent: none\nchanged files: none"],
    [{ changed_files: ["a"], changed_files_truncated: true }, "changed files (first 1):\n  a"],
  ]
  for (const [overrides, tail] of cases) {
    const { client } = detailClient({ ...detail, ...overrides })
    const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
    try {
      const text = await plugin.tool?.agent_snap_get.execute({ id: detail.id }, { directory: "/repo", abort: new AbortController().signal } as never)
      assert.ok(String(text).endsWith(tail), `expected ${JSON.stringify(text)} to end with ${JSON.stringify(tail)}`)
    } finally {
      await plugin.dispose?.()
    }
  }
})

test("takes a baseline snapshot when a session is created", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback" })
  try {
    await plugin.event?.({ event: createdEvent("s1", "/repo") })
    await sleep(10)
    assert.deepEqual(
      { cwd: creates[0]?.cwd, reason: creates[0]?.reason },
      { cwd: "/repo", reason: "session-start" },
    )
  } finally {
    releaseAll(gates)
    await plugin.dispose?.()
  }
})

test("coalesces idle events into one snapshot of the settled tree", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback", coalesceDelayMs: 30 })
  try {
    await plugin.event?.({ event: createdEvent("s1", "/repo") })
    await plugin.event?.({ event: idleEvent("s1") })
    await plugin.event?.({ event: idleEvent("s1") })
    assert.equal(creates.length, 1, "only the baseline so far")
    await sleep(100)
    assert.equal(creates.length, 2, "exactly one idle snapshot")
    assert.deepEqual(
      { cwd: creates[1]?.cwd, reason: creates[1]?.reason },
      { cwd: "/repo", reason: "session-idle" },
    )
  } finally {
    releaseAll(gates)
    await plugin.dispose?.()
  }
})

test("a busy status cancels a pending idle snapshot", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback", coalesceDelayMs: 1000 })
  try {
    await plugin.event?.({ event: createdEvent("s1", "/repo") })
    await plugin.event?.({ event: idleEvent("s1") })
    await plugin.event?.({ event: statusEvent("s1", "busy") })
    await sleep(50)
    assert.equal(creates.length, 1, "pending idle snapshot must be cancelled")
  } finally {
    releaseAll(gates)
    await plugin.dispose?.()
  }
})

test("busy after the idle snapshot fired aborts the in-flight request", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback", coalesceDelayMs: 10 })
  try {
    await plugin.event?.({ event: createdEvent("s1", "/repo") })
    await plugin.event?.({ event: idleEvent("s1") })
    await sleep(40)
    assert.equal(creates.length, 2, "idle snapshot must be in flight")
    assert.equal(gates.length, 2, "both creates must still be gated (in flight)")
    await plugin.event?.({ event: statusEvent("s1", "busy") })
    assert.equal(creates[1]?.signal?.aborted, true, "in-flight idle request must be aborted")
  } finally {
    releaseAll(gates)
    await plugin.dispose?.()
  }
})

test("session.deleted aborts a baseline that is still in flight", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback" })
  try {
    await plugin.event?.({ event: createdEvent("s1", "/repo") })
    await sleep(10)
    assert.equal(creates.length, 1, "baseline must be in flight")
    await plugin.event?.({ event: deletedEvent("s1") })
    assert.equal(creates[0]?.signal?.aborted, true, "baseline must be aborted on delete")
  } finally {
    releaseAll(gates)
    await plugin.dispose?.()
  }
})

test("events are rejected once the plugin is closing", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback", coalesceDelayMs: 10 })
  try {
    await plugin.event?.({ event: createdEvent("s1", "/repo") })
    await plugin.event?.({ event: idleEvent("s1") })
    const disposing = plugin.dispose?.()
    await plugin.event?.({ event: createdEvent("s2", "/other") })
    await plugin.event?.({ event: idleEvent("s1") })
    releaseAll(gates)
    await disposing
    assert.equal(creates.length, 2, "no new work may start during dispose")
    assert.equal(creates[1]?.reason, "session-idle", "the flushed idle snapshot is the last one")
  } finally {
    releaseAll(gates)
    await plugin.dispose?.()
  }
})

test("idle for an unknown session is ignored, never the plugin root", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback", coalesceDelayMs: 30 })
  try {
    await plugin.event?.({ event: idleEvent("ghost-session") })
    await sleep(100)
    assert.equal(creates.length, 0, "unknown sessions must not produce snapshots")
  } finally {
    releaseAll(gates)
    await plugin.dispose?.()
  }
})

test("a rejected baseline is logged and never unhandled", async () => {
  const logs: string[] = []
  const client: SnapLike = {
    async create() {
      throw new Error("baseline boom")
    },
    async list() {
      return []
    },
    async get() {
      throw new Error("unexpected get")
    },
    async close() {},
  }
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback", log: (m) => logs.push(m) })
  try {
    await plugin.event?.({ event: createdEvent("s1", "/repo") })
    await sleep(20)
    assert.ok(logs.some((m) => m.includes("baseline boom")), "failure must be logged")
  } finally {
    await plugin.dispose?.()
  }
})

test("dispose is a shutdown barrier: flushes idle and waits for in-flight work", async () => {
  const { client, creates, gates, closed } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback", coalesceDelayMs: 30 })
  await plugin.event?.({ event: createdEvent("s1", "/repo") })
  await plugin.event?.({ event: idleEvent("s1") })

  let disposed = false
  const disposePromise = plugin.dispose?.().then(() => {
    disposed = true
  })
  await sleep(50)
  assert.equal(disposed, false, "dispose must wait for the in-flight idle snapshot")
  for (const gate of gates) gate.resolve()
  await disposePromise
  assert.equal(disposed, true)
  assert.equal(closed(), true, "dispose must close the client")
  assert.equal(creates.length, 2)
})

test("tools pass the abort signal and reject while closing", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback" })
  const aborted = new AbortController()
  aborted.abort()
  await assert.rejects(
    () => plugin.tool?.agent_snap_create.execute({ reason: "x" }, { directory: "/repo", abort: aborted.signal } as never),
    (err: unknown) => (err as Error).name === "AbortError",
  )
  assert.equal(creates.length, 1, "aborted tool call must reach the client once")
  assert.equal(creates[0]?.signal?.aborted, true)

  await plugin.dispose?.()
  await assert.rejects(
    () => plugin.tool?.agent_snap_list.execute({}, { directory: "/repo", abort: new AbortController().signal } as never),
    /closing/,
  )
  await assert.rejects(
    () => plugin.tool?.agent_snap_get.execute({ id: detail.id }, { directory: "/repo", abort: new AbortController().signal } as never),
    /closing/,
  )
})
