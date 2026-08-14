import { test } from "node:test"
import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"
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

function execErr(err: unknown): { status: number | null; stderr: string; stdout: string } {
  const e = err as { status?: number; stderr?: Buffer | string; stdout?: Buffer | string; message?: string }
  return {
    status: typeof e.status === "number" ? e.status : null,
    stderr: String(e.stderr ?? ""),
    stdout: String(e.stdout ?? ""),
  }
}

/**
 * Fail-closed pgrep: status 0 → matches, status 1 with empty stderr → no
 * match, anything else (or any other throw) is an inspection error and fails
 * the test instead of being silently treated as "nothing to see".
 */
function pgrep(args: string[]): string[] {
  try {
    return execFileSync("pgrep", args, { encoding: "utf8" })
      .trim()
      .split("\n")
      .filter((line) => line.length > 0)
  } catch (err) {
    const { status, stderr } = execErr(err)
    if (status === 1 && stderr.trim().length === 0) return []
    throw new Error(`pgrep ${JSON.stringify(args)} failed (status ${status}): ${stderr}`)
  }
}

/**
 * Fail-closed per-PID command line: any ps failure throws.
 */
function commandOf(pid: number): string {
  try {
    return execFileSync("ps", ["-o", "command=", "-p", String(pid)], { encoding: "utf8" }).trim()
  } catch (err) {
    const { status, stderr } = execErr(err)
    throw new Error(`ps -o command= -p ${pid} failed (status ${status}): ${stderr}`)
  }
}

/**
 * Fail-closed per-PID state: "gone" (reaped), "zombie" (terminated, awaiting
 * reap), or "live". Only an empty no-match (status 1, no stderr) counts as
 * gone; any other ps failure throws.
 */
function processState(pid: number): "gone" | "zombie" | "live" {
  try {
    const stat = execFileSync("ps", ["-o", "stat=", "-p", String(pid)], { encoding: "utf8" }).trim()
    if (stat.startsWith("Z")) return "zombie"
    return "live"
  } catch (err) {
    const { status, stderr } = execErr(err)
    if (status === 1 && stderr.trim().length === 0) return "gone"
    throw new Error(`ps -o stat= -p ${pid} failed (status ${status}): ${stderr}`)
  }
}

test("inspection helpers fail closed instead of masking errors", () => {
  assert.throws(() => pgrep(["--definitely-not-a-pgrep-flag"]), /failed/)
  assert.throws(() => processState(-1), /failed/)
})

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
      const pending = client.create(dir, "slow-filter")

      // Capture the known spawned tree while it is running: the client's
      // child (the guardian supervisor on POSIX) is the process-group
      // leader, so every descendant (git, the filter shell, sleep 37)
      // shares its pgid. The tree is identified strictly by group
      // membership — never by a global command-line pattern, which could
      // match unrelated processes. Fail closed if the tree never appears:
      // the test must not pass vacuously.
      const captureDeadline = Date.now() + 5_000
      let agentPid: number | undefined
      let groupPids: string[] = []
      let filterPids: string[] = []
      for (;;) {
        agentPid = client.pid
        if (agentPid !== undefined) {
          groupPids = pgrep(["-g", String(agentPid)])
          filterPids = groupPids.filter((pid) => commandOf(Number(pid)).includes("sleep 37"))
          if (groupPids.length > 0 && filterPids.length > 0) break
        }
        if (Date.now() > captureDeadline) {
          throw new Error(
            `timed out waiting for the sleepy filter tree (agentPid=${agentPid}, group=${JSON.stringify(groupPids)}, filter=${JSON.stringify(filterPids)})`,
          )
        }
        await new Promise((resolve) => setTimeout(resolve, 50))
      }
      assert.ok(agentPid !== undefined, "client pid must be known while the request is in flight")
      assert.ok(groupPids.length >= 2, `expected the git tree (git + filter) in the process group, saw ${JSON.stringify(groupPids)}`)
      assert.ok(filterPids.length >= 1, `expected a spawned sleep-37 filter inside the process group ${agentPid}`)

      const tracked = [...new Set([String(agentPid), ...groupPids, ...filterPids].map((p) => Number(p)))]
      let closeError: unknown = null
      try {
        await assert.rejects(() => pending, /timed out/)
      } finally {
        try {
          await client.close()
        } catch (err) {
          closeError = err
        }
      }
      assert.equal(closeError, null, `close() must complete the tree teardown: ${String(closeError)}`)

      // At the moment close() resolves, no tracked process may still be
      // LIVE — a single live tracked PID here is proof that close()
      // returned while the tree was still running. Zombies are terminated
      // (the kill has landed) and may await reaping; only live processes
      // violate the contract.
      const stillLive = tracked.filter((pid) => processState(pid) === "live")
      assert.deepEqual(
        stillLive,
        [],
        `close() returned while these tracked processes were still alive: ${stillLive.join(",")}`,
      )

      // Reaping is the OS's job, so allow a bounded wait and distinguish
      // zombies (terminated, awaiting reap) from live processes (a genuine
      // orphan: the contract violation).
      const deadline = Date.now() + 5_000
      let live: number[] = []
      for (;;) {
        live = tracked.filter((pid) => processState(pid) === "live")
        if (live.length === 0 || Date.now() > deadline) break
        await new Promise((resolve) => setTimeout(resolve, 100))
      }
      assert.deepEqual(
        live,
        [],
        `live processes survived the timeout (${live.join(",")}) — process-tree cancellation contract violated`,
      )
      assert.deepEqual(
        pgrep(["-g", String(agentPid)]).filter((pid) => processState(Number(pid)) === "live"),
        [],
        `process group ${agentPid} still contains live members`,
      )

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
