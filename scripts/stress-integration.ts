import { execSync, spawn } from "node:child_process"
import { fileURLToPath } from "node:url"
import { join } from "node:path"

const root = fileURLToPath(new URL("..", import.meta.url))
const bin = join(root, "bin", "agent-snap")
const testFile = join(root, "packages/core/test/integration.test.ts")
const iterations = Number(process.env.ITERATIONS ?? "10")
const perIterationMs = Number(process.env.PER_ITERATION_MS ?? "90000")

function psTreeDump(): string {
  try {
    return execSync(
      `ps -eo pid,ppid,pgid,state,etime,command | grep -E "agent-snap|fake-agent-snap|--eval|sleep 37|--test|tsx" | grep -v grep`,
      { encoding: "utf8" },
    )
  } catch {
    return "(unable to dump process state)"
  }
}

function runIteration(index: number): Promise<"pass" | "fail" | "timeout"> {
  const started = Date.now()
  return new Promise((resolveIteration) => {
    let output = ""
    const child = spawn(process.execPath, ["--import", "tsx", "--test", testFile], {
      env: { ...process.env, AGENT_SNAP_BIN: bin },
      stdio: ["ignore", "pipe", "pipe"],
      detached: true,
    })
    const killer = setTimeout(() => {
      const dump = psTreeDump()
      process.stdout.write(`\n[iteration ${index}] TIMEOUT after ${perIterationMs}ms — process tree at kill time:\n${dump}\n`)
      output += `\n[timeout] process tree:\n${dump}\n`
      try {
        process.kill(-(child.pid ?? 0), "SIGKILL")
      } catch {
        // gone
      }
      resolveIteration("timeout")
    }, perIterationMs)
    child.stdout.on("data", (chunk: Buffer) => {
      output += chunk.toString()
    })
    child.stderr.on("data", (chunk: Buffer) => {
      output += chunk.toString()
    })
    child.on("error", (err) => {
      clearTimeout(killer)
      output += `\n[spawn error] ${err.message}\n`
      resolveIteration("fail")
    })
    child.on("exit", (code, signal) => {
      clearTimeout(killer)
      const elapsed = Date.now() - started
      const passed = code === 0
      process.stdout.write(`[iteration ${index}] ${passed ? "pass" : "fail"} (${elapsed}ms, code=${code}, signal=${signal ?? "none"})\n`)
      if (!passed) process.stdout.write(output.slice(-4000) + "\n")
      resolveIteration(passed ? "pass" : "fail")
    })
  })
}

let pass = 0
let fail = 0
let timeout = 0
for (let i = 1; i <= iterations; i++) {
  const outcome = await runIteration(i)
  if (outcome === "pass") pass++
  else if (outcome === "fail") fail++
  else timeout++
}
process.stdout.write(`\nstress: ${pass}/${iterations} pass, ${fail} fail, ${timeout} timeout\n`)
process.exit(timeout > 0 || fail > 0 ? 1 : 0)
