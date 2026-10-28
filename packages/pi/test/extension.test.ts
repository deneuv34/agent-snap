import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { discoverAndLoadExtensions, type LoadExtensionsResult } from "@earendil-works/pi-coding-agent"

const distEntry = fileURLToPath(new URL("../dist/extension.js", import.meta.url))

type ExtensionShape = {
  path: string
  resolvedPath: string
  tools: Map<string, unknown>
  handlers: Map<string, Array<(event: unknown, ctx: unknown) => unknown>>
  commands: Map<string, unknown>
}

function load(): Promise<ExtensionShape> {
  return discoverAndLoadExtensions([distEntry], process.cwd()).then((result: LoadExtensionsResult) => {
    assert.equal(result.errors.length, 0, `official loader reported errors: ${JSON.stringify(result.errors)}`)
    const mine = (result.extensions as unknown as ExtensionShape[]).filter((extension) => extension.path === distEntry)
    assert.equal(mine.length, 1, "expected exactly one loaded instance of the agent-snap extension")
    return mine[0] as ExtensionShape
  })
}

function fakeCtx(cwd: string) {
  return {
    cwd,
    mode: "tui",
    hasUI: true,
    ui: { notify: () => {}, select: async () => undefined, confirm: async () => false, input: async () => undefined },
    isIdle: () => true,
    isProjectTrusted: () => true,
    abort: () => {},
    hasPendingMessages: () => false,
    shutdown: () => {},
  }
}

test("package.json declares pi.extensions pointing at the built entry", () => {
  const manifest = JSON.parse(readFileSync(fileURLToPath(new URL("../package.json", import.meta.url)), "utf8")) as {
    pi?: { extensions?: string[] }
    peerDependencies?: Record<string, string>
  }
  assert.deepEqual(manifest.pi?.extensions, ["dist/extension.js"])
  assert.ok(manifest.peerDependencies?.["@earendil-works/pi-coding-agent"], "must declare the pi-coding-agent peer dependency")
  assert.ok(manifest.peerDependencies?.["@earendil-works/pi-ai"], "must declare the pi-ai peer dependency")
})

test("the official Pi loader loads the built extension and registers tools, handlers, and commands", async () => {
  const extension = await load()
  assert.ok(extension.tools.has("agent_snap_create"), "missing create tool")
  assert.ok(extension.tools.has("agent_snap_list"), "missing list tool")
  assert.ok(extension.tools.has("agent_snap_get"), "missing get tool")
  assert.ok(extension.handlers.has("session_start"), "missing session_start baseline handler")
  assert.ok(extension.handlers.has("agent_settled"), "missing agent_settled handler")
  assert.ok(extension.handlers.has("session_shutdown"), "missing session_shutdown cleanup handler")
  assert.ok(extension.commands.has("agent-snap-create"), "missing create command")
  assert.ok(extension.commands.has("agent-snap-list"), "missing list command")
  assert.ok(extension.commands.has("agent-snap-get"), "missing get command")
})

test(
  "the get tool and command describe a snapshot through the official loader",
  { skip: process.platform === "win32" ? "fake binary wrapper is POSIX-only" : false },
  async () => {
    const { dir, log, wrapper } = makeFakeBinary()
    const previous = process.env.AGENT_SNAP_BIN
    process.env.AGENT_SNAP_BIN = wrapper
    process.env.FAKE_AGENT_SNAP_LOG = log
    const id = "20260101T000000.000000000Z-0123456789ab"
    try {
      const extension = await load()
      const tool = extension.tools.get("agent_snap_get") as {
        definition: { execute: (...args: unknown[]) => Promise<{ content: Array<{ text: string }> }> }
      }
      const command = extension.commands.get("agent-snap-get") as { handler: (args: string, ctx: unknown) => Promise<void> }
      const sessionShutdown = extension.handlers.get("session_shutdown")?.[0]
      const notes: string[] = []
      const base = fakeCtx(dir)
      const ctx = { ...base, ui: { ...base.ui, notify: (message: string) => notes.push(message) } }

      const result = await tool.definition.execute("call-1", { id }, undefined, undefined, ctx)
      await command.handler(` ${id} `, ctx)
      await sessionShutdown?.({ type: "session_shutdown", reason: "quit" }, ctx)

      const text = result.content[0]?.text ?? ""
      assert.equal(
        text,
        `Snapshot ${id}\ncommit: 0123456789ab0000000000000000000000000000\ntree: 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n` +
          "reason: manual\ncreated: 2026-01-01T00:00:00Z\n" +
          "parent: fedcba9876540000000000000000000000000000 (snapshot 20251231T000000.000000000Z-fedcba987654)\n" +
          "changed files:\n  src/a.ts",
      )
      assert.deepEqual(notes, [text])
      const gets = readFakeLog(log).filter((entry) => entry.event === "request" && entry.method === "snapshot.get")
      assert.deepEqual(
        gets.map((entry) => entry.params),
        [
          { cwd: dir, id },
          { cwd: dir, id },
        ],
      )
    } finally {
      if (previous === undefined) delete process.env.AGENT_SNAP_BIN
      else process.env.AGENT_SNAP_BIN = previous
      delete process.env.FAKE_AGENT_SNAP_LOG
    }
  },
)

test(
  "lifecycle through the official loader with a recording fake binary",
  { skip: process.platform === "win32" ? "fake binary wrapper is POSIX-only" : false },
  async () => {
    const { dir, log, wrapper } = makeFakeBinary()
    const previous = process.env.AGENT_SNAP_BIN
    process.env.AGENT_SNAP_BIN = wrapper
    process.env.FAKE_AGENT_SNAP_MODE = "record"
    process.env.FAKE_AGENT_SNAP_LOG = log
    try {
      const extension = await load()
      const sessionStart = extension.handlers.get("session_start")?.[0]
      const agentSettled = extension.handlers.get("agent_settled")?.[0]
      const sessionShutdown = extension.handlers.get("session_shutdown")?.[0]
      assert.ok(sessionStart && agentSettled && sessionShutdown, "handlers must be registered")

      const ctx = fakeCtx(dir)
      await sessionStart({ type: "session_start", reason: "startup" }, ctx)
      await agentSettled({ type: "agent_settled" }, ctx)
      await agentSettled({ type: "agent_settled" }, ctx)
      await sessionShutdown({ type: "session_shutdown", reason: "quit" }, ctx)

      const entries = readFakeLog(log)
      const creates = entries.filter((entry) => entry.event === "request" && entry.method === "snapshot.create")
      assert.deepEqual(
        creates.map((entry) => entry.params),
        [
          { cwd: dir, reason: "session-start", source: "session-start", host: "pi" },
          { cwd: dir, reason: "agent-settled", source: "agent-settled", host: "pi" },
        ],
        "one baseline and one coalesced settled snapshot, each with its source and the pi host",
      )
    } finally {
      if (previous === undefined) delete process.env.AGENT_SNAP_BIN
      else process.env.AGENT_SNAP_BIN = previous
      delete process.env.FAKE_AGENT_SNAP_MODE
      delete process.env.FAKE_AGENT_SNAP_LOG
    }
  },
)

test(
  "the create tool and command send their own source and the pi host",
  { skip: process.platform === "win32" ? "fake binary wrapper is POSIX-only" : false },
  async () => {
    const { dir, log, wrapper } = makeFakeBinary()
    const previous = process.env.AGENT_SNAP_BIN
    process.env.AGENT_SNAP_BIN = wrapper
    process.env.FAKE_AGENT_SNAP_LOG = log
    try {
      const extension = await load()
      const tool = extension.tools.get("agent_snap_create") as {
        definition: { execute: (...args: unknown[]) => Promise<{ details: { snapshot: { source?: string; host?: string } } }> }
      }
      const command = extension.commands.get("agent-snap-create") as { handler: (args: string, ctx: unknown) => Promise<void> }
      const sessionShutdown = extension.handlers.get("session_shutdown")?.[0]
      const ctx = fakeCtx(dir)

      const result = await tool.definition.execute("call-1", {}, undefined, undefined, ctx)
      await command.handler(" before refactor ", ctx)
      await sessionShutdown?.({ type: "session_shutdown", reason: "quit" }, ctx)

      assert.deepEqual(result.details.snapshot.source, "tool")
      assert.deepEqual(result.details.snapshot.host, "pi")
      const creates = readFakeLog(log).filter((entry) => entry.event === "request" && entry.method === "snapshot.create")
      assert.deepEqual(
        creates.map((entry) => entry.params),
        [
          { cwd: dir, reason: "manual", source: "tool", host: "pi" },
          { cwd: dir, reason: "before refactor", source: "command", host: "pi" },
        ],
      )
    } finally {
      if (previous === undefined) delete process.env.AGENT_SNAP_BIN
      else process.env.AGENT_SNAP_BIN = previous
      delete process.env.FAKE_AGENT_SNAP_LOG
    }
  },
)

function makeFakeBinary() {
  const dir = mkdtempSync(join(tmpdir(), "agent-snap-pi-fake-"))
  const log = join(dir, "fake.log")
  const fixture = fileURLToPath(new URL("../../core/test/fixtures/fake-agent-snap.mjs", import.meta.url))
  const wrapper = join(dir, "fake-agent-snap")
  writeFileSync(wrapper, `#!/bin/sh\nexec node ${JSON.stringify(fixture)} serve\n`, { mode: 0o755 })
  return { dir, log, wrapper }
}

function readFakeLog(log: string): Array<Record<string, unknown>> {
  return readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line) as Record<string, unknown>)
}
