import { execFileSync } from "node:child_process"
import { cpSync, existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

const bin = process.env.AGENT_SNAP_BIN?.trim()

function fail(message: string): never {
  console.error(`pack-check: FAIL: ${message}`)
  process.exit(1)
}

function run(command: string, args: string[], cwd?: string): string {
  try {
    return execFileSync(command, args, { cwd, encoding: "utf8" })
  } catch (err) {
    const stderr = (err as { stderr?: string }).stderr ?? String(err)
    fail(`${command} ${args.join(" ")} failed: ${stderr}`)
  }
}

function main(): void {
  if (!bin) fail("AGENT_SNAP_BIN must point at the built agent-snap binary")

  const root = process.cwd()
  const work = mkdtempSync(join(tmpdir(), "agent-snap-pack-"))
  try {
    // 1. Pack each workspace.
    const tarballs: Array<{ workspace: string; path: string }> = []
    for (const workspace of ["@agent-snap/core", "@agent-snap/pi", "@agent-snap/opencode"]) {
      const output = run("npm", ["pack", "--pack-destination", work, "-w", workspace], root)
      const name = output.trim().split("\n").pop()?.trim() ?? ""
      if (!name.endsWith(".tgz")) fail(`npm pack produced no tarball for ${workspace}: ${output}`)
      tarballs.push({ workspace, path: join(work, name) })
    }

    // 2. Validate tarball contents (host entry present, stale files absent).
    for (const { workspace, path } of tarballs) {
      const listing = run("tar", ["-tzf", path])
      if (workspace === "@agent-snap/opencode") {
        if (listing.includes("plugin.js") || listing.includes("plugin.d.ts")) {
          fail(`opencode tarball contains the stale plugin.ts artifact:\n${listing}`)
        }
        if (!listing.includes("package/dist/index.js")) fail("opencode tarball missing dist/index.js")
      }
      if (workspace === "@agent-snap/pi") {
        if (!listing.includes("package/dist/extension.js")) fail("pi tarball missing dist/extension.js")
        if (listing.includes("orca-plugin.json")) fail("pi tarball still contains the removed orca manifest")
      }
      if (workspace === "@agent-snap/core") {
        if (!listing.includes("package/dist/index.js")) fail("core tarball missing dist/index.js")
      }
    }

    // 3. Install the tarballs plus the pi peer dependency into a clean project.
    const project = join(work, "project")
    mkdirSync(project, { recursive: true })
    writeFileSync(join(project, "package.json"), JSON.stringify({ name: "pack-check", private: true }))
    run(
      "npm",
      ["install", "--no-audit", "--no-fund", ...tarballs.map((t) => t.path), "@earendil-works/pi-coding-agent@0.84.1"],
      project,
    )

    // 4. Run checks against the installed tarballs.
    const checkScript = `
import { execFileSync } from "node:child_process"
import { cpSync, mkdtempSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { createRequire } from "node:module"
import { SnapClient } from "@agent-snap/core"
import { discoverAndLoadExtensions } from "@earendil-works/pi-coding-agent"
import * as opencodeEntry from "@agent-snap/opencode"

const require = createRequire(import.meta.url)
const bin = process.env.AGENT_SNAP_BIN

function git(dir, args) {
  return execFileSync("git", args, {
    cwd: dir, encoding: "utf8",
    env: { ...process.env, GIT_AUTHOR_NAME: "Pack Check", GIT_AUTHOR_EMAIL: "t@e.c", GIT_COMMITTER_NAME: "Pack Check", GIT_COMMITTER_EMAIL: "t@e.c" },
  }).trim()
}

const repo = mkdtempSync(join(tmpdir(), "pack-check-repo-"))
git(repo, ["init", "-b", "main"])
writeFileSync(join(repo, "file.txt"), "initial\\n")
git(repo, ["add", "file.txt"])
git(repo, ["commit", "-m", "initial"])
writeFileSync(join(repo, "file.txt"), "changed\\n")

// core tarball: real binary over JSONL
const client = SnapClient.fromEnv({ AGENT_SNAP_BIN: bin })
const snap = await client.create(repo, "pack-check")
if (!snap.changed_files?.includes("file.txt")) throw new Error("changed_files mismatch: " + JSON.stringify(snap))
const items = await client.list(repo, 10)
if (items.length !== 1) throw new Error("list mismatch")
await client.close()

// opencode tarball: exactly one loadable plugin entry
const fns = Object.entries(opencodeEntry).filter(([, v]) => typeof v === "function").map(([k]) => k)
if (fns.join(",") !== "AgentSnapPlugin") throw new Error("opencode entry exports: " + fns.join(","))

// pi tarball: manifest inside the packed tarball survives npm install
const piPkg = require("@agent-snap/pi/package.json")
if (JSON.stringify(piPkg.pi?.extensions) !== JSON.stringify(["dist/extension.js"])) {
  throw new Error("pi manifest lost in packing: " + JSON.stringify(piPkg.pi))
}

// pi tarball: official loader validates the packed entry via a configured path
const piEntry = require.resolve("@agent-snap/pi")
let result = await discoverAndLoadExtensions([piEntry], repo)
if (result.errors.length > 0) throw new Error("pi load errors: " + JSON.stringify(result.errors))
const viaConfig = result.extensions.filter((e) => e.path === piEntry)
if (viaConfig.length !== 1) throw new Error("pi extension did not load via configured path")
for (const tool of ["agent_snap_create", "agent_snap_list", "agent_snap_get"]) {
  if (!viaConfig[0].tools.has(tool)) throw new Error("missing tool: " + tool)
}
for (const event of ["session_start", "agent_settled", "session_shutdown"]) {
  if (!viaConfig[0].handlers.has(event)) throw new Error("missing handler: " + event)
}
for (const command of ["agent-snap-create", "agent-snap-get"]) {
  if (!viaConfig[0].commands.has(command)) throw new Error("missing command: " + command)
}

// pi: documented direct-placement topology — discovery WITHOUT configured paths
const piRoot = require.resolve("@agent-snap/pi/package.json").replace(/package\\.json$/, "")
const fakeCwd = mkdtempSync(join(tmpdir(), "pack-check-pi-topology-"))
const extDir = join(fakeCwd, ".pi", "extensions", "agent-snap")
require("node:fs").mkdirSync(extDir, { recursive: true })
cpSync(join(piRoot, "package.json"), join(extDir, "package.json"))
cpSync(join(piRoot, "dist"), join(extDir, "dist"), { recursive: true })
const coreRoot = require.resolve("@agent-snap/core/package.json").replace(/package\\.json$/, "")
require("node:fs").mkdirSync(join(extDir, "node_modules", "@agent-snap"), { recursive: true })
cpSync(coreRoot, join(extDir, "node_modules", "@agent-snap", "core"), { recursive: true })

result = await discoverAndLoadExtensions([], fakeCwd)
if (result.errors.length > 0) throw new Error("topology load errors: " + JSON.stringify(result.errors))
const viaDiscovery = result.extensions.filter((e) => e.path.endsWith(".pi" + "/extensions/agent-snap/dist/extension.js") || e.path.endsWith(".pi\\\\extensions\\\\agent-snap\\\\dist\\\\extension.js"))
if (viaDiscovery.length !== 1) throw new Error("pi extension not discovered from the documented topology: " + result.extensions.map((e) => e.path).join(", "))
if (!viaDiscovery[0].handlers.has("session_start")) throw new Error("discovered extension missing session_start handler")

console.log("pack-check: OK")
`
    const checkFile = join(project, "check.mjs")
    writeFileSync(checkFile, checkScript)
    const output = run(process.execPath, [checkFile], project)
    console.log(output.trim())
  } finally {
    rmSync(work, { recursive: true, force: true })
  }
}

main()
