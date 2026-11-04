import { appendFileSync } from "node:fs"
import { createInterface } from "node:readline"

const label = process.argv[2]
if (!label) {
  console.error("usage: go test ./... -json | go-test-summary.mjs <label>")
  process.exit(2)
}

const tests = new Map()
const packages = new Map()
const packageOutput = new Map()
const failed = []
const unfinished = []
const skipped = []
let passed = 0
let malformed = 0

function emit(text) {
  process.stdout.write(text.endsWith("\n") ? text : `${text}\n`)
}

function skipReason(output) {
  const lines = output.split("\n")
  const start = lines.findLastIndex((l) => /^\s+\S+\.go:\d+: /.test(l))
  if (start < 0) return "no reason logged"
  const message = [lines[start].replace(/^\s+\S+\.go:\d+: /, "")]
  for (const l of lines.slice(start + 1)) {
    if (/^\s*(---|===) /.test(l)) break
    if (l.trim()) message.push(l.trim())
  }
  return message.join(" ")
}

function reportUnfinished(pkg) {
  for (const t of tests.values()) {
    if (t.pkg !== pkg || t.action) continue
    t.action = "unfinished"
    unfinished.push(t)
    emit(`Test did not finish ${t.pkg} ${t.name}:`)
    emit(t.output.trimEnd())
  }
}

function finishPackage(pkg, event) {
  packages.set(pkg, event.Action)
  const elapsed = `${Number(event.Elapsed ?? 0).toFixed(1)}s`
  if (event.Action !== "fail") {
    emit(`${event.Action === "pass" ? "ok  " : event.Action} ${pkg} (${elapsed})`)
    return
  }
  reportUnfinished(pkg)
  const output = (packageOutput.get(pkg) ?? "").trimEnd()
  if (output) emit(output)
  emit(`FAIL ${pkg} (${elapsed})`)
}

function handle(event) {
  const pkg = event.Package
  if (!pkg) {
    if (event.Output) emit(event.Output)
    return
  }
  if (!packages.has(pkg)) packages.set(pkg, "")
  if (!event.Test) {
    if (event.Action === "output") packageOutput.set(pkg, (packageOutput.get(pkg) ?? "") + event.Output)
    else if (["pass", "fail", "skip"].includes(event.Action)) finishPackage(pkg, event)
    return
  }
  const key = `${pkg}\u0000${event.Test}`
  const test = tests.get(key) ?? { pkg, name: event.Test, output: "", action: "" }
  tests.set(key, test)
  if (event.Action === "output") {
    test.output += event.Output
    return
  }
  if (!["pass", "fail", "skip"].includes(event.Action)) return
  test.action = event.Action
  if (event.Action === "pass") passed++
  if (event.Action === "skip") skipped.push({ pkg, name: test.name, reason: skipReason(test.output) })
  if (event.Action === "fail") {
    failed.push(test)
    emit(`Failed test ${pkg} ${test.name}:`)
    emit(test.output.trimEnd())
  }
  test.output = ""
}

for await (const line of createInterface({ input: process.stdin, crlfDelay: Infinity })) {
  if (!line.trim()) continue
  if (!line.startsWith("{")) {
    emit(line)
    continue
  }
  let event
  try {
    event = JSON.parse(line)
  } catch {
    malformed++
    emit(line)
    continue
  }
  handle(event)
}

const incomplete = [...packages].filter(([, action]) => action === "").map(([name]) => name)
for (const name of incomplete) {
  emit(`No result for package ${name}.`)
  reportUnfinished(name)
}
const failedPackages = [...packages].filter(([, action]) => action === "fail").map(([name]) => name)

const counts = `${passed} passed, ${failed.length} failed, ${skipped.length} skipped, ${unfinished.length} did not finish`
const lines = [`Go test summary (${label}): ${counts}`]
if (malformed > 0) lines.push(`Lines of go test output that could not be parsed: ${malformed}`)
if (failedPackages.length + incomplete.length > 0) {
  lines.push("Failed packages:", ...[...failedPackages, ...incomplete].map((name) => `  ${name}`))
}
if (failed.length > 0) lines.push("Failed tests:", ...failed.map((t) => `  ${t.pkg} ${t.name}`))
if (unfinished.length > 0) lines.push("Tests that did not finish:", ...unfinished.map((t) => `  ${t.pkg} ${t.name}`))
if (skipped.length > 0) {
  lines.push("Skipped tests:", ...skipped.map((t) => `  ${t.pkg} ${t.name}: ${t.reason}`))
}
emit(lines.join("\n"))

const summaryFile = process.env.GITHUB_STEP_SUMMARY
if (summaryFile) {
  const md = [`### Go tests (${label})`, "", counts, ""]
  if (skipped.length > 0) {
    md.push("| Package | Test | Skip reason |", "| --- | --- | --- |")
    for (const t of skipped) md.push(`| ${t.pkg} | ${t.name} | ${t.reason.replaceAll("|", "\\|")} |`)
    md.push("")
  }
  appendFileSync(summaryFile, md.join("\n") + "\n")
}

if (failed.length + unfinished.length + failedPackages.length + incomplete.length + malformed > 0) process.exitCode = 1
