import { appendFileSync, readFileSync } from "node:fs"

const [file, label] = process.argv.slice(2)
if (!file || !label) {
  console.error("usage: go-test-summary.mjs <go-test-json-file> <label>")
  process.exit(2)
}

const tests = new Map()
const packages = new Map()
const packageOutput = new Map()

for (const line of readFileSync(file, "utf8").split("\n")) {
  if (!line.startsWith("{")) continue
  const event = JSON.parse(line)
  const pkg = event.Package
  if (!pkg) continue
  if (!event.Test) {
    if (event.Action === "output") {
      packageOutput.set(pkg, (packageOutput.get(pkg) ?? "") + event.Output)
    } else if (["pass", "fail", "skip"].includes(event.Action)) {
      packages.set(pkg, { action: event.Action, elapsed: event.Elapsed ?? 0 })
    }
    continue
  }
  const key = `${pkg}\u0000${event.Test}`
  const test = tests.get(key) ?? { pkg, name: event.Test, output: "", action: "" }
  if (event.Action === "output") test.output += event.Output
  else if (["pass", "fail", "skip"].includes(event.Action)) test.action = event.Action
  tests.set(key, test)
}

const all = [...tests.values()]
const failed = all.filter((t) => t.action === "fail")
const skipped = all.filter((t) => t.action === "skip")
const passed = all.filter((t) => t.action === "pass")
const failedPackages = [...packages].filter(([, p]) => p.action === "fail").map(([name]) => name)

function skipReason(test) {
  const reasons = test.output
    .split("\n")
    .map((l) => l.match(/^\s+\S+\.go:\d+: (.*)$/)?.[1])
    .filter(Boolean)
  return reasons.join(" ") || "no reason logged"
}

const lines = []
lines.push(`Go test summary (${label}): ${passed.length} passed, ${failed.length} failed, ${skipped.length} skipped`)
for (const [name, p] of [...packages].sort()) lines.push(`  ${p.action.padEnd(4)} ${name} (${Number(p.elapsed).toFixed(1)}s)`)
if (skipped.length > 0) {
  lines.push("Skipped tests:")
  for (const t of skipped) lines.push(`  ${t.pkg} ${t.name}: ${skipReason(t)}`)
}
for (const t of failed) {
  lines.push(`Failed test ${t.pkg} ${t.name}:`)
  lines.push(t.output.trimEnd())
}
for (const name of failedPackages) {
  if (!failed.some((t) => t.pkg === name)) {
    lines.push(`Failed package ${name}:`)
    lines.push((packageOutput.get(name) ?? "").trimEnd())
  }
}
console.log(lines.join("\n"))

const summaryFile = process.env.GITHUB_STEP_SUMMARY
if (summaryFile) {
  const md = [`### Go tests (${label})`, "", `${passed.length} passed, ${failed.length} failed, ${skipped.length} skipped`, ""]
  if (skipped.length > 0) {
    md.push("| Package | Test | Skip reason |", "| --- | --- | --- |")
    for (const t of skipped) md.push(`| ${t.pkg} | ${t.name} | ${skipReason(t).replaceAll("|", "\\|")} |`)
    md.push("")
  }
  appendFileSync(summaryFile, md.join("\n") + "\n")
}
