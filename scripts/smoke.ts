import { execFileSync, spawnSync } from "node:child_process"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { createRequire } from "node:module"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { SnapClient, type SnapDiff, type SnapSnapshot, type SnapSnapshotDetail } from "@agent-snap/core"

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

  // 1. Direct CLI: create + list + get + diff.
  const created = runJson(["create", "smoke-direct", "--cwd", dir]) as SnapSnapshot
  if (!created.id || !created.commit) fail(`direct create returned unexpected JSON: ${JSON.stringify(created)}`)
  if (created.source !== "cli" || created.host !== undefined) fail(`direct create must record source cli and no host: ${JSON.stringify(created)}`)

  const listed = runJson(["list", "--cwd", dir, "--limit", "10"]) as { snapshots: SnapSnapshot[] }
  if (listed.snapshots.length !== 1 || listed.snapshots[0]?.id !== created.id) {
    fail(`direct list mismatch: ${JSON.stringify(listed)}`)
  }

  const detail = runJson(["get", created.id, "--cwd", dir]) as SnapSnapshotDetail
  if (detail.commit !== created.commit || detail.parent !== headBefore || detail.tree !== git(dir, ["rev-parse", `${created.commit}^{tree}`])) {
    fail(`direct get mismatch: ${JSON.stringify(detail)}`)
  }
  if (detail.reason !== "smoke-direct" || detail.source !== "cli") fail(`direct get origin mismatch: ${JSON.stringify(detail)}`)

  const sameDiff = runJson(["diff", created.id, created.id, "--cwd", dir]) as SnapDiff
  if (sameDiff.totals.files !== 0 || sameDiff.files.length !== 0 || sameDiff.from.id !== created.id) {
    fail(`direct diff of a snapshot with itself must be empty: ${JSON.stringify(sameDiff)}`)
  }

  // 2. JSONL serve mode through the binary.
  const input =
    [
      JSON.stringify({ id: 1, method: "ping" }),
      JSON.stringify({ id: 2, method: "snapshot.create", params: { cwd: dir, reason: "smoke-jsonl", source: "tool", host: "smoke" } }),
      JSON.stringify({ id: 3, method: "snapshot.list", params: { cwd: dir, limit: 10 } }),
      JSON.stringify({ id: 4, method: "snapshot.get", params: { cwd: dir, id: created.id } }),
      JSON.stringify({ id: 5, method: "snapshot.create", params: { cwd: dir, reason: "x\n\nAgent-Snap-Source: tool" } }),
      JSON.stringify({ id: 6, method: "snapshot.diff", params: { cwd: dir, from: created.id, to: created.id } }),
      JSON.stringify({ id: 7, method: "snapshot.diff", params: { cwd: dir, from: created.id, to: created.id, max_patch_bytes: 1023 } }),
    ].join("\n") + "\n"
  const proc = spawnSync(bin, ["serve"], { input, encoding: "utf8" })
  if (proc.status !== 0) fail(`serve mode exited with ${proc.status}: ${proc.stderr}`)
  const lines = proc.stdout.trim().split("\n")
  if (lines.length !== 7) fail(`serve mode returned ${lines.length} lines: ${proc.stdout}`)
  const ping = JSON.parse(lines[0] ?? "{}") as { result?: { status?: string } }
  if (ping.result?.status !== "ok") fail(`ping failed: ${lines[0]}`)
  const jsonlSnap = (JSON.parse(lines[1] ?? "{}") as { result?: SnapSnapshot }).result
  if (jsonlSnap?.reused !== true) fail(`expected unchanged tree reuse via JSONL, got: ${lines[1]}`)
  if (jsonlSnap.source !== "cli" || jsonlSnap.host !== undefined) fail(`a reused snapshot must report its stored origin: ${lines[1]}`)
  const jsonlList = (JSON.parse(lines[2] ?? "{}") as { result?: { snapshots?: SnapSnapshot[] } }).result
  if (jsonlList?.snapshots?.length !== 1) fail(`list via JSONL mismatch: ${lines[2]}`)
  const jsonlDetail = (JSON.parse(lines[3] ?? "{}") as { result?: SnapSnapshotDetail }).result
  if (jsonlDetail?.commit !== created.commit) fail(`get via JSONL mismatch: ${lines[3]}`)
  const forged = JSON.parse(lines[4] ?? "{}") as { error?: { code?: number; message?: string } }
  if (forged.error?.code !== -32602 || forged.error.message !== "param must not contain a NUL byte or end with an origin paragraph: reason") {
    fail(`a reason that reads as origin must be rejected: ${lines[4]}`)
  }
  const jsonlDiff = (JSON.parse(lines[5] ?? "{}") as { result?: SnapDiff }).result
  if (jsonlDiff?.totals.files !== 0 || jsonlDiff.to.commit !== created.commit) fail(`diff via JSONL mismatch: ${lines[5]}`)
  const badBudget = JSON.parse(lines[6] ?? "{}") as { error?: { code?: number; message?: string } }
  if (badBudget.error?.code !== -32602 || badBudget.error.message !== "param must be an integer between 1024 and 1048576: max_patch_bytes") {
    fail(`a diff budget below 1024 must be rejected: ${lines[6]}`)
  }

  // 3. TypeScript client against serve mode.
  writeFileSync(join(dir, "file.txt"), "changed\n")
  const client = SnapClient.fromEnv({ AGENT_SNAP_BIN: bin })
  try {
    const snap = await client.create(dir, "smoke-client", undefined, { source: "tool", host: "smoke" })
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
    if (snap.source !== "tool" || described.reason !== "smoke-client" || described.source !== "tool" || described.host !== "smoke") {
      fail(`client origin did not round-trip: ${JSON.stringify({ snap, described })}`)
    }
    writeFileSync(join(dir, ".gitattributes"), "* -diff\n")
    const diff = await client.diff(dir, created.id, snap.id)
    rmSync(join(dir, ".gitattributes"))
    const changed = diff.files[0]
    const hunk = changed?.hunks[0]
    if (diff.files.length !== 1 || changed?.path !== "file.txt" || changed.status !== "modified" || changed.patch !== "full") {
      fail(`client diff must show file.txt as a full text change despite a -diff attribute: ${JSON.stringify(diff)}`)
    }
    if (hunk?.old_start !== 1 || hunk.old_lines !== 1 || hunk.new_start !== 1 || hunk.new_lines !== 1 || hunk.text !== "-initial\n+changed\n") {
      fail(`client diff hunk mismatch: ${JSON.stringify(changed)}`)
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

  // 6. A compiled Bun host, the way OpenCode ships: process.execPath is the
  //    host's own CLI, not a JavaScript runtime.
  writeFileSync(join(dir, "file.txt"), "compiled\n")
  const hosted = runCompiledHost(dir)
  if (hosted.reason !== "smoke-compiled-host" || !hosted.changed_files?.includes("file.txt")) {
    fail(`compiled host snapshot mismatch: ${JSON.stringify(hosted)}`)
  }
}

function runCompiledHost(dir: string): SnapSnapshot {
  const work = mkdtempSync(join(tmpdir(), "agent-snap-smoke-host-"))
  try {
    const core = createRequire(import.meta.url).resolve("@agent-snap/core")
    const entry = join(work, "host.ts")
    // A host CLI that ran its own entrypoint for --eval would recurse; exit instead.
    writeFileSync(
      entry,
      `import { SnapClient } from ${JSON.stringify(core)}
if (process.argv.includes("--eval")) process.exit(3)
const client = SnapClient.fromEnv()
try {
  console.log(JSON.stringify(await client.create(process.env.SMOKE_REPO ?? "", "smoke-compiled-host")))
} finally {
  await client.close()
}
`,
    )
    const host = join(work, process.platform === "win32" ? "host.exe" : "host")
    execFileSync(process.execPath, ["build", "--compile", entry, "--outfile", host], { stdio: "ignore" })
    // Recent macOS kills Bun's compiled output for an invalid signature.
    if (process.platform === "darwin") execFileSync("codesign", ["--force", "--sign", "-", host], { stdio: "ignore" })
    const output = execFileSync(host, [], {
      encoding: "utf8",
      timeout: 60_000,
      env: { ...process.env, AGENT_SNAP_BIN: bin, SMOKE_REPO: dir },
    })
    return JSON.parse(output) as SnapSnapshot
  } finally {
    rmSync(work, { recursive: true, force: true })
  }
}

main().catch((err: unknown) => fail(err instanceof Error ? err.message : String(err)))
