import { test } from "node:test"
import assert from "node:assert/strict"
import type {
  SnapDiff,
  SnapDiffFile,
  SnapDiffPage,
  SnapLike,
  SnapOrigin,
  SnapRestorePlan,
  SnapRestorePlanOptions,
  SnapSnapshot,
  SnapSnapshotDetail,
} from "@agent-snap/core"
import { createAgentSnapPlugin } from "../src/v1.js"

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
  origin?: SnapOrigin
}

function fakeClient() {
  const creates: CreateCall[] = []
  const gates: Deferred[] = []
  let closed = false
  const client: SnapLike = {
    async create(cwd, reason, signal, origin) {
      creates.push({ cwd, reason, signal, origin })
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
    async diff() {
      throw new Error("unexpected diff")
    },
    async restorePlan() {
      throw new Error("unexpected restorePlan")
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
    async diff() {
      throw new Error("unexpected diff")
    },
    async restorePlan() {
      throw new Error("unexpected restorePlan")
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

test("exposes create, list, get, and diff tools", async () => {
  const { client } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  assert.ok(plugin.tool?.agent_snap_create)
  assert.ok(plugin.tool?.agent_snap_list)
  assert.ok(plugin.tool?.agent_snap_get)
  assert.ok(plugin.tool?.agent_snap_diff)
  assert.ok(plugin.tool?.agent_snap_restore_plan)
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
      { cwd: creates[0]?.cwd, reason: creates[0]?.reason, origin: creates[0]?.origin },
      { cwd: "/repo", reason: "session-start", origin: { source: "session-start", host: "opencode" } },
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
      { cwd: creates[1]?.cwd, reason: creates[1]?.reason, origin: creates[1]?.origin },
      { cwd: "/repo", reason: "session-idle", origin: { source: "session-idle", host: "opencode" } },
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
    async diff() {
      throw new Error("unexpected diff")
    },
    async restorePlan() {
      throw new Error("unexpected restorePlan")
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

test("agent_snap_create sends the tool source and the opencode host", async () => {
  const { client, creates, gates } = fakeClient()
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback" })
  try {
    const pending = plugin.tool?.agent_snap_create.execute({ reason: "before" }, { directory: "/repo", abort: new AbortController().signal } as never)
    await sleep(10)
    releaseAll(gates)
    await pending
    assert.deepEqual(
      { cwd: creates[0]?.cwd, reason: creates[0]?.reason, origin: creates[0]?.origin },
      { cwd: "/repo", reason: "before", origin: { source: "tool", host: "opencode" } },
    )
  } finally {
    releaseAll(gates)
    await plugin.dispose?.()
  }
})

test("list and get show a stored origin and omit an absent one", async () => {
  const items: SnapSnapshot[] = [
    { id: "b", commit: "c2", reason: "edit", created_at: "t2", source: "tool", host: "pi" },
    { id: "a", commit: "c1", reason: "legacy", created_at: "t1" },
    { id: "z", commit: "c0", reason: "cli", created_at: "t0", source: "cli" },
  ]
  const client: SnapLike = {
    async create() {
      throw new Error("unexpected create")
    },
    async list() {
      return items
    },
    async get() {
      return { ...detail, source: "session-idle", host: "opencode" }
    },
    async diff() {
      throw new Error("unexpected diff")
    },
    async restorePlan() {
      throw new Error("unexpected restorePlan")
    },
    async close() {},
  }
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  const ctx = { directory: "/repo", abort: new AbortController().signal } as never
  try {
    assert.equal(
      await plugin.tool?.agent_snap_list.execute({}, ctx),
      "1. b — edit — t2 — tool via pi\n2. a — legacy — t1\n3. z — cli — t0 — cli",
    )
    const text = String(await plugin.tool?.agent_snap_get.execute({ id: detail.id }, ctx))
    assert.ok(text.includes("\nreason: edit\norigin: session-idle via opencode\ncreated: "), text)
  } finally {
    await plugin.dispose?.()
  }
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

const fromID = "20261029T101500.123456789Z-1a2b3c4d5e6f"
const toID = "20261029T104200.987654321Z-9f8e7d6c5b4a"

const sides = {
  from: { id: fromID, commit: "1a2b3c4d5e6f0000000000000000000000000000" },
  to: { id: toID, commit: "9f8e7d6c5b4a0000000000000000000000000000" },
}

const file = (change: Partial<SnapDiffFile>): SnapDiffFile => ({
  status: "modified",
  path: "x",
  old_mode: "100644",
  new_mode: "100644",
  old_oid: "83db48f80000",
  new_oid: "8792505e0000",
  additions: 0,
  deletions: 0,
  patch: "deferred",
  hunks: [],
  ...change,
})

const diffPage: SnapDiff = {
  ...sides,
  offset: 10,
  next_offset: 15,
  totals: { files: 30, additions: 40, deletions: 9 },
  files: [
    file({
      path: "src/server.go",
      additions: 2,
      patch: "full",
      hunks: [
        {
          old_start: 88,
          old_lines: 2,
          new_start: 88,
          new_lines: 4,
          section: "func dispatch() {",
          text: ' \tcase "snapshot.get":\n+\tcase "snapshot.diff":\n+\t\treturn diffSnapshots()\n \tdefault:\n',
        },
      ],
    }),
    file({
      status: "renamed",
      path: "src/util.go",
      old_path: "src/helpers.go",
      similarity: 92,
      additions: 1,
      deletions: 1,
      patch: "full",
      hunks: [{ old_start: 3, old_lines: 1, new_start: 3, new_lines: 1, text: "-old\n+new\n" }],
    }),
    file({ status: "added", path: "assets/logo.png", old_mode: undefined, old_oid: undefined, patch: "binary" }),
    file({
      status: "added",
      path: "docs/caf�.md",
      old_mode: undefined,
      old_oid: undefined,
      additions: 2,
      patch: "full",
      lossy: true,
      hunks: [{ old_start: 0, old_lines: 0, new_start: 1, new_lines: 2, text: "+hello\n\\ No newline at end of file\n" }],
    }),
    file({
      path: "src/big.go",
      additions: 120,
      deletions: 30,
      patch: "truncated",
      patch_bytes: 4096,
      hunks: [{ old_start: 1, old_lines: 1, new_start: 1, new_lines: 2, section: "package big", text: " package big\n+// generated\n" }],
    }),
    file({ path: "src/later.go", additions: 3, deletions: 1 }),
    file({ status: "deleted", path: "old.txt", new_mode: undefined, new_oid: undefined, deletions: 4 }),
  ],
}

const citation = "Cite a finding as `<path> @@ -a,b +c,d @@` from this output. Lines not shown here are not evidence."

const diffPageLines = [
  "Git evidence: snapshot diff (read-only)",
  `from ${fromID}`,
  `to   ${toID}`,
  "30 files changed, +40 -9. Files 11-17 of 30.",
  "",
  "11  modified  src/server.go  +2 -0",
  "12  renamed  src/util.go <- src/helpers.go (92%)  +1 -1",
  "13  added  assets/logo.png  binary",
  "14  added  docs/caf�.md  +2 -0",
  "15  modified  src/big.go  +120 -30",
  "16  modified  src/later.go  +3 -1",
  "17  deleted  old.txt  +0 -4",
  "",
  "== 11 src/server.go",
  "@@ -88,2 +88,4 @@ func dispatch() {",
  ' \tcase "snapshot.get":',
  '+\tcase "snapshot.diff":',
  "+\t\treturn diffSnapshots()",
  " \tdefault:",
  "== 12 src/util.go",
  "@@ -3,1 +3,1 @@",
  "-old",
  "+new",
  "== 14 docs/caf�.md (non-UTF-8 bytes shown as U+FFFD)",
  "@@ -0,0 +1,2 @@",
  "+hello",
  "\\ No newline at end of file",
  "== 15 src/big.go",
  "@@ -1,1 +1,2 @@ package big",
  " package big",
  "+// generated",
  "Patch truncated. Call agent_snap_diff with offset 14 and max_patch_bytes 4096 to see this file whole.",
  "",
  "Hunks from file 16 on are not shown. Call agent_snap_diff with offset 15.",
  "",
  citation,
]

const oversizePage: SnapDiff = {
  ...sides,
  offset: 0,
  totals: { files: 1, additions: 5000, deletions: 0 },
  files: [file({ status: "added", path: "data.json", old_mode: undefined, old_oid: undefined, additions: 5000, patch: "truncated" })],
}

const oversizeText = [
  "Git evidence: snapshot diff (read-only)",
  `from ${fromID}`,
  `to   ${toID}`,
  "1 files changed, +5000 -0. Files 1-1 of 1.",
  "",
  "1  added  data.json  +5000 -0",
  "",
  "== 1 data.json",
  "Patch truncated. The rest of this file is over 1048576 bytes and cannot be shown.",
  "",
  citation,
].join("\n")

const emptyPage: SnapDiff = { ...sides, offset: 0, totals: { files: 0, additions: 0, deletions: 0 }, files: [] }

const emptyText = [
  "Git evidence: snapshot diff (read-only)",
  `from ${fromID}`,
  `to   ${toID}`,
  "No files changed.",
  "",
  citation,
].join("\n")

const pastEndPage: SnapDiff = { ...sides, offset: 4, totals: { files: 3, additions: 2, deletions: 1 }, files: [] }

const pastEndText = [
  "Git evidence: snapshot diff (read-only)",
  `from ${fromID}`,
  `to   ${toID}`,
  "3 files changed, +2 -1. No files at offset 4.",
  "",
  citation,
].join("\n")

interface DiffCall {
  cwd: string
  from: string
  to: string
  page?: SnapDiffPage
  signal?: AbortSignal
}

function diffClient(result: SnapDiff) {
  const diffs: DiffCall[] = []
  const client: SnapLike = {
    async create() {
      throw new Error("unexpected create")
    },
    async list() {
      return []
    },
    async get() {
      throw new Error("unexpected get")
    },
    async diff(cwd, from, to, page, signal) {
      diffs.push({ cwd, from, to, page, signal })
      return result
    },
    async restorePlan() {
      throw new Error("unexpected restorePlan")
    },
    async close() {},
  }
  return { client, diffs }
}

async function runDiff(result: SnapDiff): Promise<string> {
  const { client } = diffClient(result)
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  try {
    return String(await plugin.tool?.agent_snap_diff.execute({ from: fromID, to: toID }, { directory: "/repo", abort: new AbortController().signal } as never))
  } finally {
    await plugin.dispose?.()
  }
}

test("agent_snap_diff renders full, renamed, binary, lossy, truncated, and deferred files", async () => {
  assert.equal(await runDiff(diffPage), diffPageLines.join("\n"))
})

test("agent_snap_diff names a truncated file that cannot be shown whole", async () => {
  assert.equal(await runDiff(oversizePage), oversizeText)
})

test("agent_snap_diff reports an empty diff and an offset past the end", async () => {
  assert.equal(await runDiff(emptyPage), emptyText)
  assert.equal(await runDiff(pastEndPage), pastEndText)
})

test("agent_snap_diff passes cwd, ids, page fields only when given, and the abort signal", async () => {
  const { client, diffs } = diffClient(emptyPage)
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback" })
  const abort = new AbortController().signal
  try {
    await plugin.tool?.agent_snap_diff.execute({ from: fromID, to: toID }, { directory: "/repo", abort } as never)
    await plugin.tool?.agent_snap_diff.execute({ from: fromID, to: toID, cwd: "/other", offset: 0, max_patch_bytes: 2048 }, { directory: "/repo", abort } as never)
    await plugin.tool?.agent_snap_diff.execute({ from: fromID, to: toID, offset: 15 }, { directory: "/repo", abort } as never)
    assert.deepEqual(diffs, [
      { cwd: "/repo", from: fromID, to: toID, page: {}, signal: abort },
      { cwd: "/other", from: fromID, to: toID, page: { offset: 0, maxPatchBytes: 2048 }, signal: abort },
      { cwd: "/repo", from: fromID, to: toID, page: { offset: 15 }, signal: abort },
    ])
  } finally {
    await plugin.dispose?.()
  }
})

test("agent_snap_diff bounds offset and max_patch_bytes in its schema", async () => {
  const { client } = diffClient(emptyPage)
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  try {
    const { offset, max_patch_bytes } = plugin.tool?.agent_snap_diff.args as unknown as Record<string, { safeParse: (value: unknown) => { success: boolean } }>
    const accepts = (schema: { safeParse: (value: unknown) => { success: boolean } } | undefined, value: unknown) => schema?.safeParse(value).success
    assert.deepEqual([-1, 0, 2147483647, 2147483648, 1.5].map((value) => accepts(offset, value)), [false, true, true, false, false])
    assert.deepEqual([1023, 1024, 1048576, 1048577, 2048.5].map((value) => accepts(max_patch_bytes, value)), [false, true, true, false, false])
  } finally {
    await plugin.dispose?.()
  }
})

test("agent_snap_diff rejects while closing", async () => {
  const { client, diffs } = diffClient(emptyPage)
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  await plugin.dispose?.()
  await assert.rejects(
    () => plugin.tool?.agent_snap_diff.execute({ from: fromID, to: toID }, { directory: "/repo", abort: new AbortController().signal } as never),
    /closing/,
  )
  assert.equal(diffs.length, 0)
})

const planID = "20261029T101500.123456789Z-1a2b3c4d5e6f"
const planTree = "9c1e7a3f00000000000000000000000000000000"
const planSnapshot = { id: planID, commit: "1a2b3c4d5e6f0000000000000000000000000000", tree: "7f3e9b2a00000000000000000000000000000000" }
const side = (oid: string, mode = "100644") => ({ mode, oid })
const noTotals = { entries: 0, create: 0, modify: 0, type_change: 0, delete: 0, staged: 0, unstaged: 0, untracked: 0, blocked: 0 }
const planNotice = "This plan changed nothing. This release cannot apply a restore."

const applicablePlan: SnapRestorePlan = {
  snapshot: planSnapshot,
  current_tree: planTree,
  result_tree: planSnapshot.tree,
  applicable: true,
  blockers: [],
  unmatched: [],
  offset: 0,
  totals: { entries: 3, create: 1, modify: 1, type_change: 0, delete: 1, staged: 1, unstaged: 1, untracked: 0, blocked: 0 },
  entries: [
    { path: "README.md", action: "modify", current: side("8792505e"), target: side("83db48f8"), unstaged: true },
    { path: "docs/old.md", action: "delete", current: side("e69de29b"), staged: true },
    { path: "src/server.go", action: "create", target: side("5716ca59") },
  ],
}

const applicableLines = [
  "Git evidence: restore plan (read-only)",
  `snapshot ${planID}`,
  "scope: whole tree",
  `current_tree ${planTree}`,
  "applicable: yes",
  "3 entries: 1 create, 1 modify, 0 type_change, 1 delete. 1 staged, 1 unstaged, 0 untracked, 0 blocked.",
  "Entries 1-3 of 3.",
  "",
  "1  modify  README.md  flags: unstaged",
  "2  delete  docs/old.md  flags: staged",
  "3  create  src/server.go",
  "",
  planNotice,
]

const blockedPlan: SnapRestorePlan = {
  snapshot: planSnapshot,
  paths: ["src", "docs/missing"],
  current_tree: planTree,
  applicable: false,
  blockers: ["operation_in_progress", "sparse_checkout"],
  unmatched: ["docs/missing"],
  offset: 0,
  totals: { entries: 4, create: 1, modify: 1, type_change: 1, delete: 1, staged: 2, unstaged: 1, untracked: 1, blocked: 2 },
  entries: [
    { path: "src/a.go", action: "modify", current: side("8792505e"), target: side("83db48f8"), staged: true, unstaged: true, blocker: "index_flag" },
    { path: "src/b.go", action: "type_change", current: side("8792505e"), target: side("a1b2c3d4", "120000"), untracked: true },
    { path: "src/cache.log", action: "create", target: side("5716ca59"), blocker: "untracked_in_the_way" },
    { path: "src/gone.go", action: "delete", current: side("e69de29b"), staged: true },
  ],
}

const blockedLines = [
  "Git evidence: restore plan (read-only)",
  `snapshot ${planID}`,
  "scope: paths",
  "  src",
  "  docs/missing",
  `current_tree ${planTree}`,
  "applicable: no",
  "repository blockers:",
  "  operation_in_progress",
  "  sparse_checkout",
  "unmatched paths:",
  "  docs/missing",
  "4 entries: 1 create, 1 modify, 1 type_change, 1 delete. 2 staged, 1 unstaged, 1 untracked, 2 blocked.",
  "Entries 1-4 of 4.",
  "",
  "1  modify  src/a.go  flags: staged, unstaged  blocker: index_flag",
  "2  type_change  src/b.go  flags: untracked",
  "3  create  src/cache.log  blocker: untracked_in_the_way",
  "4  delete  src/gone.go  flags: staged",
  "",
  planNotice,
]

const middleTotals = { entries: 9, create: 3, modify: 3, type_change: 1, delete: 2, staged: 0, unstaged: 1, untracked: 0, blocked: 0 }

const middlePage: SnapRestorePlan = {
  snapshot: planSnapshot,
  paths: ["src"],
  current_tree: planTree,
  result_tree: planSnapshot.tree,
  applicable: true,
  blockers: [],
  unmatched: [],
  offset: 2,
  next_offset: 4,
  totals: middleTotals,
  entries: [
    { path: "src/m.go", action: "modify", current: side("8792505e"), target: side("83db48f8"), unstaged: true },
    { path: "src/n.go", action: "modify", current: side("8792505e"), target: side("83db48f8") },
  ],
}

const middleHead = [
  "Git evidence: restore plan (read-only)",
  `snapshot ${planID}`,
]

const middleTail = (call: string) => [
  `current_tree ${planTree}`,
  "applicable: yes",
  "9 entries: 3 create, 3 modify, 1 type_change, 2 delete. 0 staged, 1 unstaged, 0 untracked, 0 blocked.",
  "Entries 3-4 of 9.",
  "",
  "3  modify  src/m.go  flags: unstaged",
  "4  modify  src/n.go",
  "",
  `Entries from 5 on are not shown. Call agent_snap_restore_plan with ${call}, offset 4, and current_tree ${planTree}.`,
  "",
  planNotice,
]

const middleLines = [...middleHead, "scope: paths", "  src", ...middleTail("the same id and paths")]

const { paths: _scope, ...wholeMiddlePage } = middlePage

const wholeMiddleLines = [...middleHead, "scope: whole tree", ...middleTail("the same id")]

const emptyPlan: SnapRestorePlan = {
  snapshot: planSnapshot,
  current_tree: planTree,
  result_tree: planSnapshot.tree,
  applicable: true,
  blockers: [],
  unmatched: [],
  offset: 0,
  totals: noTotals,
  entries: [],
}

const emptyPlanLines = [
  "Git evidence: restore plan (read-only)",
  `snapshot ${planID}`,
  "scope: whole tree",
  `current_tree ${planTree}`,
  "applicable: yes",
  "0 entries: 0 create, 0 modify, 0 type_change, 0 delete. 0 staged, 0 unstaged, 0 untracked, 0 blocked.",
  "No entries.",
  "",
  planNotice,
]

const pastEndPlan: SnapRestorePlan = { ...applicablePlan, offset: 5, entries: [] }

const pastEndLines = [...applicableLines.slice(0, 6), "No entries at offset 5.", "", planNotice]

interface RestorePlanCall {
  cwd: string
  id: string
  options?: SnapRestorePlanOptions
  signal?: AbortSignal
}

function restorePlanClient(result: SnapRestorePlan, failure?: Error) {
  const plans: RestorePlanCall[] = []
  const client: SnapLike = {
    async create() {
      throw new Error("unexpected create")
    },
    async list() {
      return []
    },
    async get() {
      throw new Error("unexpected get")
    },
    async diff() {
      throw new Error("unexpected diff")
    },
    async restorePlan(cwd, id, options, signal) {
      plans.push({ cwd, id, options, signal })
      if (failure !== undefined) throw failure
      return result
    },
    async close() {},
  }
  return { client, plans }
}

async function runRestorePlan(result: SnapRestorePlan): Promise<string> {
  const { client } = restorePlanClient(result)
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  try {
    return String(await plugin.tool?.agent_snap_restore_plan.execute({ id: planID }, { directory: "/repo", abort: new AbortController().signal } as never))
  } finally {
    await plugin.dispose?.()
  }
}

test("agent_snap_restore_plan renders an applicable whole-tree plan", async () => {
  assert.equal(await runRestorePlan(applicablePlan), applicableLines.join("\n"))
})

test("agent_snap_restore_plan lists repository blockers, unmatched paths, every flag, and every blocker", async () => {
  assert.equal(await runRestorePlan(blockedPlan), blockedLines.join("\n"))
})

test("agent_snap_restore_plan names the offset and current_tree of the next page", async () => {
  assert.equal(await runRestorePlan(middlePage), middleLines.join("\n"))
  assert.equal(await runRestorePlan(wholeMiddlePage), wholeMiddleLines.join("\n"))
})

test("agent_snap_restore_plan reports an empty plan and an offset past the end", async () => {
  assert.equal(await runRestorePlan(emptyPlan), emptyPlanLines.join("\n"))
  assert.equal(await runRestorePlan(pastEndPlan), pastEndLines.join("\n"))
})

test("agent_snap_restore_plan passes cwd, id, options only when given, and the abort signal", async () => {
  const { client, plans } = restorePlanClient(emptyPlan)
  const plugin = createAgentSnapPlugin({ client, directory: "/fallback" })
  const abort = new AbortController().signal
  try {
    await plugin.tool?.agent_snap_restore_plan.execute({ id: planID }, { directory: "/repo", abort } as never)
    await plugin.tool?.agent_snap_restore_plan.execute(
      { id: planID, cwd: "/other", paths: ["src", "docs"], offset: 200, current_tree: planTree },
      { directory: "/repo", abort } as never,
    )
    await plugin.tool?.agent_snap_restore_plan.execute({ id: planID, offset: 0 }, { directory: "/repo", abort } as never)
    assert.deepEqual(plans, [
      { cwd: "/repo", id: planID, options: {}, signal: abort },
      { cwd: "/other", id: planID, options: { paths: ["src", "docs"], offset: 200, currentTree: planTree }, signal: abort },
      { cwd: "/repo", id: planID, options: { offset: 0 }, signal: abort },
    ])
  } finally {
    await plugin.dispose?.()
  }
})

test("agent_snap_restore_plan forwards an aborted signal to the client", async () => {
  const { client, plans } = restorePlanClient(emptyPlan)
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  const controller = new AbortController()
  controller.abort()
  try {
    await plugin.tool?.agent_snap_restore_plan.execute({ id: planID }, { directory: "/repo", abort: controller.signal } as never)
    assert.equal(plans[0]?.signal?.aborted, true)
  } finally {
    await plugin.dispose?.()
  }
})

test("agent_snap_restore_plan lets client errors reach the host", async () => {
  const failure = new Error("agent-snap cannot plan restores")
  const { client } = restorePlanClient(emptyPlan, failure)
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  try {
    await assert.rejects(() => plugin.tool!.agent_snap_restore_plan.execute({ id: planID }, { directory: "/repo", abort: new AbortController().signal } as never), failure)
  } finally {
    await plugin.dispose?.()
  }
})

test("agent_snap_restore_plan bounds paths and offset in its schema", async () => {
  const { client } = restorePlanClient(emptyPlan)
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  try {
    const { paths, offset } = plugin.tool?.agent_snap_restore_plan.args as unknown as Record<string, { safeParse: (value: unknown) => { success: boolean } }>
    const accepts = (schema: { safeParse: (value: unknown) => { success: boolean } } | undefined, value: unknown) => schema?.safeParse(value).success
    const many = (count: number) => Array.from({ length: count }, (_, index) => `p${index}`)
    assert.deepEqual([[], many(1), many(100), many(101), [1]].map((value) => accepts(paths, value)), [false, true, true, false, false])
    assert.deepEqual([-1, 0, 2147483647, 2147483648, 1.5].map((value) => accepts(offset, value)), [false, true, true, false, false])
  } finally {
    await plugin.dispose?.()
  }
})

test("agent_snap_restore_plan rejects while closing", async () => {
  const { client, plans } = restorePlanClient(emptyPlan)
  const plugin = createAgentSnapPlugin({ client, directory: "/repo" })
  await plugin.dispose?.()
  await assert.rejects(
    () => plugin.tool!.agent_snap_restore_plan.execute({ id: planID }, { directory: "/repo", abort: new AbortController().signal } as never),
    /closing/,
  )
  assert.equal(plans.length, 0)
})
