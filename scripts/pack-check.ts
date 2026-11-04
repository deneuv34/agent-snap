import { execFileSync } from "node:child_process"
import { cpSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { basename, join } from "node:path"

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

function documentedFiles(root: string): string[] {
  const packageReadmes = readdirSync(join(root, "packages"))
    .map((dir) => join("packages", dir, "README.md"))
    .filter((file) => existsSync(join(root, file)))
  return ["README.md", join("docs", "NAMESPACE.md"), ...packageReadmes]
}

function fencedBlocks(markdown: string): string[] {
  return [...markdown.matchAll(/^```[^\n]*\n([\s\S]*?)^```/gm)].map((match) => match[1])
}

function globMatches(glob: string, name: string): boolean {
  const pattern = glob.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*")
  return new RegExp(`^${pattern}$`).test(name)
}

function checkDocumentedNames(root: string, packed: Array<{ name: string; file: string }>, subcommands: Set<string>): void {
  const modulePath = readFileSync(join(root, "go.mod"), "utf8").match(/^module (\S+)$/m)?.[1]
  if (!modulePath) fail("go.mod names no module")
  const packageNames = new Set(packed.map((p) => p.name))
  const problems: string[] = []
  let packagesSeen = 0
  let commandsSeen = 0
  for (const file of documentedFiles(root)) {
    for (const block of fencedBlocks(readFileSync(join(root, file), "utf8"))) {
      for (const [name] of block.matchAll(/@agent-snap\/[a-z0-9-]+(?:\.[a-z0-9-]+)*/g)) {
        packagesSeen++
        if (!packageNames.has(name)) problems.push(`${file} names ${name}, which is not a packed package`)
      }
      for (const [, command] of block.matchAll(/(?:^|[\s;&|(])agent-snap[ \t]+([^\s"'`|;&)]+)/gm)) {
        commandsSeen++
        if (!subcommands.has(command)) problems.push(`${file} runs agent-snap ${command}, which the binary does not list`)
      }
      for (const [tarball] of block.matchAll(/[\w*.-]+\.tgz/g)) {
        if (!packed.some((p) => globMatches(tarball, p.file))) problems.push(`${file} names ${tarball}, which no packed tarball matches`)
      }
      for (const [, dir] of block.matchAll(new RegExp(`(?:\\.|${modulePath.replace(/\./g, "\\.")})/cmd/([a-z0-9-]+)`, "g"))) {
        if (!existsSync(join(root, "cmd", dir))) problems.push(`${file} names cmd/${dir}, which does not exist`)
      }
      for (const [name] of block.matchAll(/\bblink\b/gi)) {
        problems.push(`${file} names ${name} in a code block, but no shipped package or command is called that (docs/NAMESPACE.md)`)
      }
    }
  }
  if (packagesSeen === 0 || commandsSeen === 0) {
    fail(`documented code blocks named ${packagesSeen} packages and ${commandsSeen} agent-snap commands; the scan found nothing to check`)
  }
  if (problems.length > 0) fail(`documented names that do not ship:\n${problems.join("\n")}`)
  console.log(`pack-check: documented names OK (${packagesSeen} package and ${commandsSeen} command mentions)`)
}

function main(): void {
  if (!bin) fail("AGENT_SNAP_BIN must point at the built agent-snap binary")

  const root = process.cwd()
  const work = mkdtempSync(join(tmpdir(), "agent-snap-pack-"))
  try {
    // 1. Pack each workspace.
    const tarballs: Array<{ workspace: string; path: string }> = []
    for (const workspace of ["@agent-snap/core", "@agent-snap/pi", "@agent-snap/opencode"]) {
      const dir = join(root, "packages", workspace.slice("@agent-snap/".length))
      const output = run("bun", ["pm", "pack", "--destination", work, "--quiet"], dir)
      const path = output.trim().split("\n").pop()?.trim() ?? ""
      if (!path.endsWith(".tgz")) fail(`bun pm pack produced no tarball for ${workspace}: ${output}`)
      tarballs.push({ workspace, path })
    }

    const packed = tarballs.map(({ path }) => ({
      name: JSON.parse(run("tar", ["-xzOf", path, "package/package.json"])).name as string,
      file: basename(path),
    }))
    const subcommands = new Set([...run(bin, []).matchAll(/^\s+agent-snap ([a-z][a-z-]*)/gm)].map((match) => match[1]))
    checkDocumentedNames(root, packed, subcommands)

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

    // 3. Install the tarballs plus the pi peer dependency into a clean project,
    //    with npm because that is how Pi installs packages.
    const project = join(work, "project")
    mkdirSync(project, { recursive: true })
    writeFileSync(join(project, "package.json"), JSON.stringify({ name: "pack-check", private: true }))
    run(
      "npm",
      ["install", "--no-audit", "--no-fund", "--prefer-online", ...tarballs.map((t) => t.path), "@earendil-works/pi-coding-agent@0.84.1"],
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
const plan = await client.restorePlan(repo, snap.id)
if (plan.snapshot.id !== snap.id || plan.entries.length !== 0 || plan.applicable !== true) throw new Error("restore plan mismatch: " + JSON.stringify(plan))
const cliPlan = JSON.parse(execFileSync(bin, ["restore-plan", snap.id, "--cwd", repo], { encoding: "utf8" }))
if (JSON.stringify(cliPlan) !== JSON.stringify(plan)) throw new Error("restore-plan CLI differs from the client plan: " + JSON.stringify(cliPlan))
await client.close()

// opencode tarball: one default export that OpenCode 1.x (server) and 2.x (setup) both load
if (Object.keys(opencodeEntry).join(",") !== "default") throw new Error("opencode entry exports: " + Object.keys(opencodeEntry).join(","))
const opencodePlugin = opencodeEntry.default
if (opencodePlugin.id !== "@agent-snap/opencode" || typeof opencodePlugin.server !== "function" || typeof opencodePlugin.setup !== "function") {
  throw new Error("opencode entry is not a 1.x and 2.x plugin: " + JSON.stringify(Object.keys(opencodePlugin)))
}
if (require.resolve("@agent-snap/opencode/server") !== require.resolve("@agent-snap/opencode")) {
  throw new Error("opencode ./server export does not name the package entry")
}
const opencodeHooks = await opencodePlugin.server({ directory: repo })
for (const tool of ["agent_snap_create", "agent_snap_list", "agent_snap_get", "agent_snap_diff"]) {
  if (!opencodeHooks.tool?.[tool]) throw new Error("opencode 1.x hooks missing tool: " + tool)
}
await opencodeHooks.dispose()

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
for (const tool of ["agent_snap_create", "agent_snap_list", "agent_snap_get", "agent_snap_diff"]) {
  if (!viaConfig[0].tools.has(tool)) throw new Error("missing tool: " + tool)
}
for (const event of ["session_start", "agent_settled", "session_shutdown"]) {
  if (!viaConfig[0].handlers.has(event)) throw new Error("missing handler: " + event)
}
for (const command of ["agent-snap-create", "agent-snap-get", "agent-snap-diff"]) {
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

console.log("pack-check: OK on " + (process.versions.bun ? "bun " + process.versions.bun : "node " + process.versions.node))
`
    const checkFile = join(project, "check.mjs")
    writeFileSync(checkFile, checkScript)
    // Pi hosts run the packages on Node; OpenCode hosts run them on Bun.
    for (const runtime of ["node", "bun"]) {
      console.log(run(runtime, [checkFile], project).trim())
    }
  } finally {
    rmSync(work, { recursive: true, force: true })
  }
}

main()
