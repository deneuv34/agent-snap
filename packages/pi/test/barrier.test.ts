import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { discoverAndLoadExtensions, type LoadExtensionsResult } from "@earendil-works/pi-coding-agent"

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

// The Pi loader imports the entry module, so a generated entry lets us inject
// a deferred fake client while still exercising the real discovery + load
// path. The fake bridges into globals owned by this test process.
const factorySource = `
import { createAgentSnapExtension } from ${JSON.stringify(fileURLToPath(new URL("../src/extension.ts", import.meta.url)))}
const gate = (() => { let resolve; const promise = new Promise((r) => { resolve = r }); return { promise, resolve } })()
;(globalThis).__agent_snap_test_release = () => gate.resolve()
;(globalThis).__agent_snap_test_logs = []
;(globalThis).__agent_snap_test_closed = false
export default createAgentSnapExtension({
  client: {
    async create(cwd, reason) {
      ;(globalThis).__agent_snap_test_logs.push({ cwd, reason })
      await gate.promise
      return { id: "test-1", commit: "0".repeat(40), reason: reason ?? "manual", created_at: new Date().toISOString() }
    },
    async list() { return [] },
    async close() { ;(globalThis).__agent_snap_test_closed = true },
  },
  coalesceDelayMs: 30,
})
`

interface GlobalHooks {
  __agent_snap_test_release?: () => void
  __agent_snap_test_logs?: Array<{ cwd: string; reason?: string }>
  __agent_snap_test_closed?: boolean
}

type ExtensionShape = {
  path: string
  handlers: Map<string, Array<(event: unknown, ctx: unknown) => unknown>>
}

test("session_start awaits the baseline as a barrier (deferred client through the official loader)", async () => {
  const dir = mkdtempSync(join(tmpdir(), "agent-snap-pi-barrier-"))
  try {
    const entry = join(dir, "entry.mjs")
    writeFileSync(entry, factorySource)

    const result: LoadExtensionsResult = await discoverAndLoadExtensions([entry], dir)
    assert.equal(result.errors.length, 0, JSON.stringify(result.errors))
    const mine = (result.extensions as unknown as ExtensionShape[]).filter((extension) => extension.path === entry)
    assert.equal(mine.length, 1)
    const sessionStart = mine[0]?.handlers.get("session_start")?.[0]
    const sessionShutdown = mine[0]?.handlers.get("session_shutdown")?.[0]
    assert.ok(sessionStart && sessionShutdown)

    const globals = globalThis as GlobalHooks
    globals.__agent_snap_test_logs = []

    const ctx = { cwd: "/repo", ui: { notify: () => {} } }
    let settled = false
    const barrier = Promise.resolve(sessionStart({ type: "session_start", reason: "startup" }, ctx)).then(() => {
      settled = true
    })
    await sleep(50)
    assert.equal(settled, false, "session_start must not settle while the baseline is deferred")
    assert.equal(globals.__agent_snap_test_logs?.length, 1, "baseline must have started")
    assert.deepEqual(globals.__agent_snap_test_logs?.[0], { cwd: "/repo", reason: "session-start" })

    globals.__agent_snap_test_release?.()
    await barrier
    assert.equal(settled, true, "session_start must settle after the baseline completes")

    await sessionShutdown({ type: "session_shutdown", reason: "quit" }, ctx)
    assert.equal(globals.__agent_snap_test_closed, true, "session_shutdown must close the client")
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
