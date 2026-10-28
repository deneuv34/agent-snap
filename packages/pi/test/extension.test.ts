import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import type { SnapDiff, SnapDiffFile, SnapDiffPage, SnapLike } from "@agent-snap/core"
import { discoverAndLoadExtensions, type LoadExtensionsResult } from "@earendil-works/pi-coding-agent"
import { createAgentSnapExtension } from "../src/extension.js"

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
  assert.ok(extension.tools.has("agent_snap_diff"), "missing diff tool")
  assert.ok(extension.handlers.has("session_start"), "missing session_start baseline handler")
  assert.ok(extension.handlers.has("agent_settled"), "missing agent_settled handler")
  assert.ok(extension.handlers.has("session_shutdown"), "missing session_shutdown cleanup handler")
  assert.ok(extension.commands.has("agent-snap-create"), "missing create command")
  assert.ok(extension.commands.has("agent-snap-list"), "missing list command")
  assert.ok(extension.commands.has("agent-snap-get"), "missing get command")
  assert.ok(extension.commands.has("agent-snap-diff"), "missing diff command")
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
  "the diff tool and command render a page through the official loader",
  { skip: process.platform === "win32" ? "fake binary wrapper is POSIX-only" : false },
  async () => {
    const { dir, log, wrapper } = makeFakeBinary()
    const previous = process.env.AGENT_SNAP_BIN
    process.env.AGENT_SNAP_BIN = wrapper
    process.env.FAKE_AGENT_SNAP_LOG = log
    const from = "20260101T000000.000000000Z-1a2b3c4d5e6f"
    const to = "20260102T000000.000000000Z-9f8e7d6c5b4a"
    try {
      const extension = await load()
      const tool = extension.tools.get("agent_snap_diff") as {
        definition: { execute: (...args: unknown[]) => Promise<{ content: Array<{ text: string }>; details: { diff: { offset: number } } }> }
      }
      const command = extension.commands.get("agent-snap-diff") as { handler: (args: string, ctx: unknown) => Promise<void> }
      const sessionShutdown = extension.handlers.get("session_shutdown")?.[0]
      const notes: string[] = []
      const base = fakeCtx(dir)
      const ctx = { ...base, ui: { ...base.ui, notify: (message: string) => notes.push(message) } }

      const result = await tool.definition.execute("call-1", { from, to, offset: 4, max_patch_bytes: 2048 }, undefined, undefined, ctx)
      await command.handler(` ${from} ${to} `, ctx)
      await sessionShutdown?.({ type: "session_shutdown", reason: "quit" }, ctx)

      const text = result.content[0]?.text ?? ""
      assert.ok(text.startsWith(`Git evidence: snapshot diff (read-only)\nfrom ${from}\nto   ${to}\n9 files changed, +14 -5. Files 5-7 of 9.\n`), text)
      assert.equal(result.details.diff.offset, 4)
      assert.equal(notes.length, 1)
      assert.equal(notes[0]?.split("\n").length, 12)
      const diffs = readFakeLog(log).filter((entry) => entry.event === "request" && entry.method === "snapshot.diff")
      assert.deepEqual(
        diffs.map((entry) => entry.params),
        [
          { cwd: dir, from, to, offset: 4, max_patch_bytes: 2048 },
          { cwd: dir, from, to },
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

type DiffTool = {
  parameters: { properties: Record<string, { minimum?: number; maximum?: number; type?: string }> }
  execute: (...args: unknown[]) => Promise<{ content: Array<{ text: string }>; details: { diff: SnapDiff } }>
}

type DiffCommand = { handler: (args: string, ctx: unknown) => Promise<void> }

function loadDiff(result: SnapDiff) {
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
    async close() {},
  }
  const tools = new Map<string, unknown>()
  const commands = new Map<string, unknown>()
  const pi = {
    on() {},
    registerTool: (definition: { name: string }) => tools.set(definition.name, definition),
    registerCommand: (name: string, options: unknown) => commands.set(name, options),
  }
  createAgentSnapExtension({ client })(pi as never)
  return { diffs, tool: tools.get("agent_snap_diff") as DiffTool, command: commands.get("agent-snap-diff") as DiffCommand }
}

async function runDiff(result: SnapDiff): Promise<string> {
  const { tool } = loadDiff(result)
  const reply = await tool.execute("call-1", { from: fromID, to: toID }, undefined, undefined, fakeCtx("/repo"))
  return reply.content[0]?.text ?? ""
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

test("agent_snap_diff passes cwd, ids, page fields only when given, and the signal, and returns the page as details", async () => {
  const { tool, diffs } = loadDiff(diffPage)
  const signal = new AbortController().signal
  const ctx = fakeCtx("/repo")
  const reply = await tool.execute("call-1", { from: fromID, to: toID }, signal, undefined, ctx)
  await tool.execute("call-2", { from: fromID, to: toID, cwd: "/other", offset: 0, max_patch_bytes: 2048 }, signal, undefined, ctx)
  await tool.execute("call-3", { from: fromID, to: toID, offset: 15 }, signal, undefined, ctx)
  assert.deepEqual(reply.details, { diff: diffPage })
  assert.deepEqual(diffs, [
    { cwd: "/repo", from: fromID, to: toID, page: {}, signal },
    { cwd: "/other", from: fromID, to: toID, page: { offset: 0, maxPatchBytes: 2048 }, signal },
    { cwd: "/repo", from: fromID, to: toID, page: { offset: 15 }, signal },
  ])
})

test("agent_snap_diff bounds offset and max_patch_bytes in its schema", () => {
  const { tool } = loadDiff(emptyPage)
  const { offset, max_patch_bytes } = tool.parameters.properties
  assert.deepEqual([offset?.type, offset?.minimum, offset?.maximum], ["integer", 0, 2147483647])
  assert.deepEqual([max_patch_bytes?.type, max_patch_bytes?.minimum, max_patch_bytes?.maximum], ["integer", 1024, 1048576])
})

test("the diff command notifies the first 12 lines and asks for two ids", async () => {
  const { command, diffs } = loadDiff(diffPage)
  const notes: Array<[string, string | undefined]> = []
  const base = fakeCtx("/repo")
  const ctx = { ...base, ui: { ...base.ui, notify: (message: string, type?: string) => notes.push([message, type]) } }
  await command.handler(`  ${fromID}   ${toID} `, ctx)
  await command.handler(fromID, ctx)
  await command.handler("", ctx)
  await command.handler(`${fromID} ${toID} extra`, ctx)
  assert.deepEqual(diffs, [{ cwd: "/repo", from: fromID, to: toID, page: undefined, signal: undefined }])
  assert.deepEqual(notes, [
    [diffPageLines.slice(0, 12).join("\n"), "info"],
    ["Usage: agent-snap-diff <from> <to>", "warning"],
    ["Usage: agent-snap-diff <from> <to>", "warning"],
    ["Usage: agent-snap-diff <from> <to>", "warning"],
  ])
})
