import { test } from "node:test"
import assert from "node:assert/strict"
import { execFileSync, execSync } from "node:child_process"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { SnapClient } from "../src/index.js"

const bin = process.env.AGENT_SNAP_BIN?.trim()

function git(dir: string, args: string[]): string {
  return execFileSync("git", args, {
    cwd: dir,
    encoding: "utf8",
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "Integration Test",
      GIT_AUTHOR_EMAIL: "test@example.com",
      GIT_COMMITTER_NAME: "Integration Test",
      GIT_COMMITTER_EMAIL: "test@example.com",
    },
  }).trim()
}

test(
  "SnapClient talks to the real agent-snap binary over JSONL",
  { skip: bin === undefined ? "AGENT_SNAP_BIN not set" : false },
  async () => {
    const dir = mkdtempSync(join(tmpdir(), "agent-snap-it-"))
    git(dir, ["init", "-b", "main"])
    writeFileSync(join(dir, "file.txt"), "initial\n")
    git(dir, ["add", "file.txt"])
    git(dir, ["commit", "-m", "initial"])
    writeFileSync(join(dir, "file.txt"), "changed\n")

    const client = SnapClient.fromEnv({ AGENT_SNAP_BIN: bin })
    try {
      const ping = await client.ping()
      assert.equal(ping.status, "ok")

      const snap = await client.create(dir, "integration-test")
      assert.ok(snap.id.length > 0)
      assert.deepEqual(snap.changed_files, ["file.txt"])

      const items = await client.list(dir, 10)
      assert.equal(items.length, 1)
      assert.equal(items[0]?.commit, snap.commit)
    } finally {
      await client.close()
    }
  },
)

test(
  "timeout kills the whole git process tree, including filter children",
  { skip: bin === undefined ? "AGENT_SNAP_BIN not set" : process.platform === "win32" ? "process-tree kill not implemented on win32" : false },
  async () => {
    const dir = mkdtempSync(join(tmpdir(), "agent-snap-filter-"))
    try {
      git(dir, ["init", "-b", "main"])
      git(dir, ["config", "filter.sleepy.clean", "sleep 37"])
      writeFileSync(join(dir, ".gitattributes"), "*.txt filter=sleepy\n")
      writeFileSync(join(dir, "slow.txt"), "hello\n")

      const client = new SnapClient({ bin, timeoutMs: 800 })
      try {
        await assert.rejects(() => client.create(dir, "slow-filter"), /timed out/)
      } finally {
        await client.close()
      }

      // The `sleep 37` filter child must have been killed with the group.
      let survivors = ""
      try {
        survivors = execSync("pgrep -f 'sleep 37'", { encoding: "utf8" })
      } catch {
        survivors = ""
      }
      assert.equal(survivors.trim(), "", `filter child survived the timeout: ${survivors}`)

      // The repo must be usable afterwards.
      git(dir, ["config", "--unset", "filter.sleepy.clean"])
      const clean = new SnapClient({ bin })
      try {
        const snap = await clean.create(dir, "after-filter-cleanup")
        assert.ok(snap.id.length > 0)
      } finally {
        await clean.close()
      }
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
  },
)
