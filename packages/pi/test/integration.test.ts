import { test } from "node:test"
import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"
import { mkdtempSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { discoverAndLoadExtensions, type LoadExtensionsResult } from "@earendil-works/pi-coding-agent"

const bin = process.env.AGENT_SNAP_BIN?.trim()

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

function git(dir: string, args: string[]): string {
  return execFileSync("git", args, {
    cwd: dir,
    encoding: "utf8",
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "Pi Integration Test",
      GIT_AUTHOR_EMAIL: "test@example.com",
      GIT_COMMITTER_NAME: "Pi Integration Test",
      GIT_COMMITTER_EMAIL: "test@example.com",
    },
  }).trim()
}

test(
  "session_start baselines a real repo through the real binary via the official loader",
  { skip: bin === undefined ? "AGENT_SNAP_BIN not set" : false },
  async () => {
    const repo = mkdtempSync(join(tmpdir(), "agent-snap-pi-it-"))
    git(repo, ["init", "-b", "main"])
    writeFileSync(join(repo, "file.txt"), "initial\n")
    git(repo, ["add", "file.txt"])
    git(repo, ["commit", "-m", "initial"])
    const headBefore = git(repo, ["rev-parse", "HEAD"])

    const previous = process.env.AGENT_SNAP_BIN
    process.env.AGENT_SNAP_BIN = bin
    try {
      const distEntry = fileURLToPath(new URL("../dist/extension.js", import.meta.url))
      const result: LoadExtensionsResult = await discoverAndLoadExtensions([distEntry], repo)
      assert.equal(result.errors.length, 0, JSON.stringify(result.errors))
      const mine = (result.extensions as unknown as Array<{ path: string; handlers: Map<string, Array<(event: unknown, ctx: unknown) => unknown>> }>).filter(
        (extension) => extension.path === distEntry,
      )
      assert.equal(mine.length, 1, "expected the agent-snap extension to load")
      const extension = mine[0] as {
        handlers: Map<string, Array<(event: unknown, ctx: unknown) => unknown>>
      }
      const sessionStart = extension.handlers.get("session_start")?.[0]
      const sessionShutdown = extension.handlers.get("session_shutdown")?.[0]
      assert.ok(sessionStart && sessionShutdown)

      const ctx = { cwd: repo, ui: { notify: () => {} } }
      await sessionStart({ type: "session_start", reason: "startup" }, ctx)

      let refs = ""
      for (let attempt = 0; attempt < 80; attempt += 1) {
        refs = git(repo, ["for-each-ref", "refs/agent-snapshots"])
        if (refs.trim().length > 0) break
        await sleep(50)
      }
      assert.ok(refs.trim().length > 0, "expected hidden snapshot refs after session_start")

      assert.equal(git(repo, ["rev-parse", "HEAD"]), headBefore, "HEAD must not move")

      await sessionShutdown({ type: "session_shutdown", reason: "quit" }, ctx)
    } finally {
      if (previous === undefined) delete process.env.AGENT_SNAP_BIN
      else process.env.AGENT_SNAP_BIN = previous
    }
  },
)
