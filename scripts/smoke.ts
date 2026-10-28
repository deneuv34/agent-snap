import { execFileSync, spawnSync } from "node:child_process"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { SnapClient, type SnapSnapshot, type SnapSnapshotDetail } from "@agent-snap/core"

const bin = process.env.AGENT_SNAP_BIN?.trim() ?? "agent-snap"

function fail(message: string): never {
  console.error(`smoke: FAIL: ${message}`)
  process.exit(1)
}

function git(dir: string, args: string[]): string {
  return execFileSync("git", args, {
    cwd: dir,
    encoding: "utf8",
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "Smoke Test",
      GIT_AUTHOR_EMAIL: "smoke@example.com",
      GIT_COMMITTER_NAME: "Smoke Test",
      GIT_COMMITTER_EMAIL: "smoke@example.com",
    },
  }).trim()
}

function runJson(args: string[]): unknown {
  const result = execFileSync(bin, args, { encoding: "utf8" })
  return JSON.parse(result)
}

async function main(): Promise<void> {
  const dir = mkdtempSync(join(tmpdir(), "agent-snap-smoke-"))
  try {
    await runChecks(dir)
    console.log(`smoke: OK (repo ${dir})`)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

async function runChecks(dir: string): Promise<void> {
  git(dir, ["init", "-b", "main"])
  writeFileSync(join(dir, "file.txt"), "initial\n")
  git(dir, ["add", "file.txt"])
  git(dir, ["commit", "-m", "initial"])

  const headBefore = git(dir, ["rev-parse", "HEAD"])
  const indexBefore = git(dir, ["write-tree"])

  // 1. Direct CLI: create + list + get.
  const created = runJson(["create", "smoke-direct", "--cwd", dir]) as SnapSnapshot
  if (!created.id || !created.commit) fail(`direct create returned unexpected JSON: ${JSON.stringify(created)}`)

  const listed = runJson(["list", "--cwd", dir, "--limit", "10"]) as { snapshots: SnapSnapshot[] }
  if (listed.snapshots.length !== 1 || listed.snapshots[0]?.id !== created.id) {
    fail(`direct list mismatch: ${JSON.stringify(listed)}`)
  }

  const detail = runJson(["get", created.id, "--cwd", dir]) as SnapSnapshotDetail
  if (detail.commit !== created.commit || detail.parent !== headBefore || detail.tree !== git(dir, ["rev-parse", `${created.commit}^{tree}`])) {
    fail(`direct get mismatch: ${JSON.stringify(detail)}`)
  }

  // 2. JSONL serve mode through the binary.
  const input =
    [
      JSON.stringify({ id: 1, method: "ping" }),
      JSON.stringify({ id: 2, method: "snapshot.create", params: { cwd: dir, reason: "smoke-jsonl" } }),
      JSON.stringify({ id: 3, method: "snapshot.list", params: { cwd: dir, limit: 10 } }),
      JSON.stringify({ id: 4, method: "snapshot.get", params: { cwd: dir, id: created.id } }),
    ].join("\n") + "\n"
  const proc = spawnSync(bin, ["serve"], { input, encoding: "utf8" })
  if (proc.status !== 0) fail(`serve mode exited with ${proc.status}: ${proc.stderr}`)
  const lines = proc.stdout.trim().split("\n")
  if (lines.length !== 4) fail(`serve mode returned ${lines.length} lines: ${proc.stdout}`)
  const ping = JSON.parse(lines[0] ?? "{}") as { result?: { status?: string } }
  if (ping.result?.status !== "ok") fail(`ping failed: ${lines[0]}`)
  const jsonlSnap = (JSON.parse(lines[1] ?? "{}") as { result?: SnapSnapshot }).result
  if (jsonlSnap?.reused !== true) fail(`expected unchanged tree reuse via JSONL, got: ${lines[1]}`)
  const jsonlList = (JSON.parse(lines[2] ?? "{}") as { result?: { snapshots?: SnapSnapshot[] } }).result
  if (jsonlList?.snapshots?.length !== 1) fail(`list via JSONL mismatch: ${lines[2]}`)
  const jsonlDetail = (JSON.parse(lines[3] ?? "{}") as { result?: SnapSnapshotDetail }).result
  if (jsonlDetail?.commit !== created.commit) fail(`get via JSONL mismatch: ${lines[3]}`)

  // 3. TypeScript client against serve mode.
  writeFileSync(join(dir, "file.txt"), "changed\n")
  const client = SnapClient.fromEnv({ AGENT_SNAP_BIN: bin })
  try {
    const snap = await client.create(dir, "smoke-client")
    if (!snap.changed_files?.includes("file.txt")) {
      fail(`expected changed_files to include file.txt: ${JSON.stringify(snap)}`)
    }
    const again = await client.create(dir, "smoke-client-again")
    if (again.reused !== true) fail(`expected reuse of unchanged tree: ${JSON.stringify(again)}`)
    const items = await client.list(dir, 10)
    if (items.length !== 2) fail(`expected 2 snapshots, got ${items.length}: ${JSON.stringify(items)}`)
    const described = await client.get(dir, snap.id)
    if (described.parent_id !== created.id || described.changed_files.join(",") !== "file.txt") {
      fail(`expected the client snapshot to chain onto ${created.id}: ${JSON.stringify(described)}`)
    }
  } finally {
    await client.close()
  }

  // 4. Safety invariants: HEAD and the real index are untouched.
  if (git(dir, ["rev-parse", "HEAD"]) !== headBefore) fail("HEAD moved")
  if (git(dir, ["write-tree"]) !== indexBefore) fail("index changed")

  // 5. Snapshot content is captured.
  const latest = runJson(["list", "--cwd", dir, "--limit", "1"]) as { snapshots: SnapSnapshot[] }
  const captured = git(dir, ["show", `${latest.snapshots[0]?.commit}:file.txt`])
  if (captured !== "changed") fail(`snapshot content mismatch: got ${JSON.stringify(captured)}`)
}

main().catch((err: unknown) => fail(err instanceof Error ? err.message : String(err)))
