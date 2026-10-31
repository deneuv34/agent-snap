import { test } from "node:test"
import assert from "node:assert/strict"
import type { SnapDiff, SnapDiffPage, SnapLike, SnapOrigin, SnapSnapshot } from "@agent-snap/core"
import { setupAgentSnap, type AgentSnapHost } from "../src/v2.js"

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

interface CreateCall {
  cwd: string
  reason?: string
  signal?: AbortSignal
  origin?: SnapOrigin
}

function fakeClient() {
  const creates: CreateCall[] = []
  const lists: Array<{ cwd: string; limit?: number; signal?: AbortSignal }> = []
  const diffs: Array<{ cwd: string; from: string; to: string; page?: SnapDiffPage; signal?: AbortSignal }> = []
  const releases: Array<() => void> = []
  let closed = false
  const client: SnapLike = {
    async create(cwd, reason, signal, origin) {
      creates.push({ cwd, reason, signal, origin })
      await new Promise<void>((resolve, reject) => {
        if (signal?.aborted) return reject(new DOMException("aborted", "AbortError"))
        signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true })
        releases.push(resolve)
      })
      const snap: SnapSnapshot = { id: `snap-${creates.length}`, commit: "0123456789ab", reason: reason ?? "manual", created_at: "2026-11-01T00:00:00Z" }
      return snap
    },
    async list(cwd, limit, signal) {
      lists.push({ cwd, limit, signal })
      return []
    },
    async get() {
      throw new Error("unexpected get")
    },
    async diff(cwd, from, to, page, signal) {
      diffs.push({ cwd, from, to, page, signal })
      const empty: SnapDiff = {
        from: { id: from, commit: "a".repeat(40) },
        to: { id: to, commit: "b".repeat(40) },
        offset: 0,
        totals: { files: 0, additions: 0, deletions: 0 },
        files: [],
      }
      return empty
    },
    async close() {
      closed = true
    },
  }
  const release = () => {
    for (const resolve of releases.splice(0)) resolve()
  }
  return { client, creates, lists, diffs, release, closed: () => closed }
}

interface ToolInfo {
  name: string
  description: string
  input: { type: string; properties: Record<string, Record<string, unknown>>; required: string[]; additionalProperties: boolean }
  execute: (input: unknown, context: { sessionID: string; signal: AbortSignal }) => Promise<{ content?: unknown }>
}

function fakeHost(options: { sessionDirectory?: string; transformFails?: boolean } = {}) {
  const queue: unknown[] = []
  let wake: (() => void) | undefined
  const subscriptions: Array<{ types?: readonly string[]; signal?: AbortSignal }> = []
  const tools = new Map<string, ToolInfo>()
  const gets: Array<{ sessionID: string; signal?: AbortSignal }> = []
  let failure: Error | undefined

  async function* stream(signal?: AbortSignal) {
    while (signal?.aborted !== true) {
      if (failure !== undefined) throw failure
      const next = queue.shift()
      if (next !== undefined) {
        yield next
        continue
      }
      await new Promise<void>((resolve) => {
        wake = resolve
        signal?.addEventListener("abort", () => resolve(), { once: true })
      })
    }
  }

  const host = {
    event: {
      subscribe(subscribeOptions: { types?: readonly string[]; signal?: AbortSignal }) {
        subscriptions.push(subscribeOptions)
        return stream(subscribeOptions.signal)
      },
    },
    tool: {
      async transform(callback: (editor: { add: (tool: ToolInfo) => void }) => void) {
        if (options.transformFails === true) throw new Error("registration refused")
        callback({ add: (tool) => tools.set(tool.name, tool) })
        return { dispose: async () => {} }
      },
    },
    session: {
      async get(input: { sessionID: string }, requestOptions?: { signal?: AbortSignal }) {
        gets.push({ sessionID: input.sessionID, signal: requestOptions?.signal })
        return { location: { directory: options.sessionDirectory ?? "/session/dir" } }
      },
    },
  } as unknown as AgentSnapHost

  const emit = async (event: unknown) => {
    queue.push(event)
    wake?.()
    await sleep(5)
  }
  const fail = async (error: Error) => {
    failure = error
    wake?.()
    await sleep(5)
  }
  return { host, tools, gets, subscriptions, emit, fail }
}

const created = (sessionID: string, directory: string) => ({ type: "session.created", data: { sessionID, location: { directory } } })
const idle = (sessionID: string) => ({ type: "session.idle", data: { sessionID } })
const status = (sessionID: string, type: "busy" | "idle") => ({ type: "session.status", data: { sessionID, status: { type } } })
const deleted = (sessionID: string) => ({ type: "session.deleted", data: { sessionID } })

const toolContext = (sessionID = "ses_1", signal = new AbortController().signal) => ({ sessionID, signal })

test("registers create, list, get, and diff tools with strict object schemas", async () => {
  const { client } = fakeClient()
  const { host, tools } = fakeHost()
  const cleanup = await setupAgentSnap(host, { client })
  try {
    assert.deepEqual([...tools.keys()], ["agent_snap_create", "agent_snap_list", "agent_snap_get", "agent_snap_diff"])
    for (const tool of tools.values()) {
      assert.equal(tool.input.type, "object")
      assert.equal(tool.input.additionalProperties, false, `${tool.name} must reject unknown keys`)
      assert.ok(tool.description.length > 0)
    }
    assert.deepEqual(tools.get("agent_snap_get")?.input.required, ["id"])
    assert.deepEqual(tools.get("agent_snap_diff")?.input.required, ["from", "to"])
    assert.deepEqual(tools.get("agent_snap_list")?.input.properties.limit, {
      type: "integer",
      minimum: 1,
      maximum: 100,
      description: "Maximum number of snapshots to return",
    })
    const diff = tools.get("agent_snap_diff")?.input.properties
    assert.deepEqual([diff?.offset?.minimum, diff?.offset?.maximum], [0, 2147483647])
    assert.deepEqual([diff?.max_patch_bytes?.minimum, diff?.max_patch_bytes?.maximum], [1024, 1048576])
  } finally {
    await cleanup?.()
  }
})

test("subscribes to the four session events and snapshots the created session's directory", async () => {
  const { client, creates, release } = fakeClient()
  const { host, subscriptions, emit } = fakeHost()
  const cleanup = await setupAgentSnap(host, { client, coalesceDelayMs: 20 })
  try {
    assert.deepEqual(subscriptions[0]?.types, ["session.created", "session.idle", "session.status", "session.deleted"])
    await emit(created("ses_1", "/work/repo"))
    assert.deepEqual(
      creates.map(({ cwd, reason, origin }) => ({ cwd, reason, origin })),
      [{ cwd: "/work/repo", reason: "session-start", origin: { source: "session-start", host: "opencode" } }],
    )
    release()

    await emit(idle("ses_1"))
    await emit(idle("ses_1"))
    await sleep(60)
    assert.equal(creates.length, 2, "bursts of idle events coalesce into one snapshot")
    assert.deepEqual(creates[1]?.origin, { source: "session-idle", host: "opencode" })
    assert.equal(creates[1]?.cwd, "/work/repo")
    release()
  } finally {
    await cleanup?.()
  }
})

test("busy cancels a pending idle snapshot and deleted aborts an in-flight baseline", async () => {
  const { client, creates, release } = fakeClient()
  const { host, emit } = fakeHost()
  const cleanup = await setupAgentSnap(host, { client, coalesceDelayMs: 40 })
  try {
    await emit(created("ses_1", "/work/repo"))
    release()
    await emit(idle("ses_1"))
    await emit(status("ses_1", "busy"))
    await sleep(80)
    assert.equal(creates.length, 1, "busy must cancel the pending idle snapshot")

    await emit(created("ses_2", "/work/other"))
    assert.equal(creates[1]?.signal?.aborted, false)
    await emit(deleted("ses_2"))
    assert.equal(creates[1]?.signal?.aborted, true, "deleting the session aborts its baseline")

    await emit(idle("ses_unknown"))
    await sleep(80)
    assert.equal(creates.length, 2, "idle for an unknown session is ignored")
  } finally {
    await cleanup?.()
  }
})

test("tools default cwd to the calling session's directory and pass the tool signal", async () => {
  const { client, creates, lists, release } = fakeClient()
  const { host, tools, gets } = fakeHost({ sessionDirectory: "/session/repo" })
  const cleanup = await setupAgentSnap(host, { client })
  try {
    const signal = new AbortController().signal
    const pending = tools.get("agent_snap_create")?.execute({ reason: "checkpoint" }, toolContext("ses_9", signal))
    await sleep(5)
    release()
    const result = await pending
    assert.match(String(result?.content), /^Snapshot snap-1 created/)
    assert.deepEqual(gets, [{ sessionID: "ses_9", signal }])
    assert.deepEqual(creates.map(({ cwd, reason, signal: s, origin }) => ({ cwd, reason, s, origin })), [
      { cwd: "/session/repo", reason: "checkpoint", s: signal, origin: { source: "tool", host: "opencode" } },
    ])

    await tools.get("agent_snap_list")?.execute({ cwd: "/explicit", limit: 5 }, toolContext("ses_9", signal))
    assert.equal(gets.length, 1, "an explicit cwd never asks the host for the session")
    assert.deepEqual(lists, [{ cwd: "/explicit", limit: 5, signal }])
  } finally {
    await cleanup?.()
  }
})

test("diff passes ids and page fields only when given", async () => {
  const { client, diffs } = fakeClient()
  const { host, tools } = fakeHost()
  const cleanup = await setupAgentSnap(host, { client })
  try {
    const diff = tools.get("agent_snap_diff")
    const output = await diff?.execute({ from: "a", to: "b", cwd: "/r" }, toolContext())
    assert.match(String(output?.content), /^Git evidence: snapshot diff \(read-only\)\nfrom a\nto {3}b\nNo files changed\./)
    await diff?.execute({ from: "a", to: "b", cwd: "/r", offset: 3, max_patch_bytes: 2048 }, toolContext())
    assert.deepEqual(
      diffs.map(({ from, to, page }) => ({ from, to, page })),
      [
        { from: "a", to: "b", page: {} },
        { from: "a", to: "b", page: { offset: 3, maxPatchBytes: 2048 } },
      ],
    )
  } finally {
    await cleanup?.()
  }
})

test("tools reject malformed input before reaching the client", async () => {
  const { client, creates, diffs } = fakeClient()
  const { host, tools, gets } = fakeHost()
  const cleanup = await setupAgentSnap(host, { client })
  try {
    await assert.rejects(() => tools.get("agent_snap_create")!.execute(null, toolContext()), /input must be an object/)
    await assert.rejects(() => tools.get("agent_snap_create")!.execute({ reason: 7 }, toolContext()), /reason must be a string/)
    await assert.rejects(() => tools.get("agent_snap_list")!.execute({ cwd: "/r", limit: 1.5 }, toolContext()), /limit must be an integer/)
    await assert.rejects(() => tools.get("agent_snap_get")!.execute({ cwd: "/r" }, toolContext()), /id is required/)
    await assert.rejects(() => tools.get("agent_snap_diff")!.execute({ from: "a", cwd: "/r" }, toolContext()), /to is required/)
    await assert.rejects(() => tools.get("agent_snap_diff")!.execute({ from: "a", to: "b", cwd: 1 }, toolContext()), /cwd must be a string/)
    assert.equal(creates.length + diffs.length + gets.length, 0)
  } finally {
    await cleanup?.()
  }
})

test("cleanup ends the subscription, waits for in-flight work, and closes the client", async () => {
  const { client, creates, closed } = fakeClient()
  const { host, tools, subscriptions, emit } = fakeHost()
  const cleanup = await setupAgentSnap(host, { client })
  await emit(created("ses_1", "/work/repo"))
  // The baseline is gated and never released: cleanup can only resolve by aborting it.
  await cleanup?.()
  assert.equal(subscriptions[0]?.signal?.aborted, true)
  assert.equal(creates[0]?.signal?.aborted, true, "cleanup aborts the in-flight baseline")
  assert.equal(closed(), true)

  await emit(created("ses_2", "/work/repo"))
  assert.equal(creates.length, 1, "no snapshots after cleanup")
  await assert.rejects(() => tools.get("agent_snap_list")!.execute({ cwd: "/r" }, toolContext()), /closing/)
})

test("a refused tool registration closes the subscription and the client", async () => {
  const { client, closed } = fakeClient()
  const { host, subscriptions } = fakeHost({ transformFails: true })
  await assert.rejects(() => setupAgentSnap(host, { client }), /registration refused/)
  assert.equal(subscriptions[0]?.signal?.aborted, true)
  assert.equal(closed(), true)
})

test("an event stream failure is logged, never thrown", async () => {
  const { client } = fakeClient()
  const { host, fail } = fakeHost()
  const logs: string[] = []
  const cleanup = await setupAgentSnap(host, { client, log: (message) => logs.push(message) })
  await fail(new Error("stream reset"))
  assert.deepEqual(logs, ["session event stream failed: stream reset"])
  await cleanup?.()
})
