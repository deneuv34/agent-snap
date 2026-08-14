import { appendFileSync, existsSync, writeFileSync } from "node:fs"
import { createInterface } from "node:readline"

const mode = process.env.FAKE_AGENT_SNAP_MODE ?? ""
const logFile = process.env.FAKE_AGENT_SNAP_LOG
const stateFile = process.env.FAKE_AGENT_SNAP_STATE

const record = (obj) => {
  if (logFile) appendFileSync(logFile, JSON.stringify({ pid: process.pid, ...obj }) + "\n")
}

if (mode === "trap-term") {
  process.on("SIGTERM", () => {
    record({ event: "sigterm" })
    setTimeout(() => process.exit(0), 5000)
  })
}

const rl = createInterface({ input: process.stdin })
let created = 0

rl.on("line", (line) => {
  let req
  try {
    req = JSON.parse(line)
  } catch {
    respond(null, null, { code: -32700, message: "parse error" })
    return
  }
  record({ event: "request", method: req.method, params: req.params ?? null })
  if (mode === "exit-on-list" && req.method === "snapshot.list") {
    process.exit(0)
  }
  if (mode === "garbage-line" && req.method === "ping") {
    console.log("this is not valid json at all")
  }
  const slow =
    (mode === "slow-create" && req.method === "snapshot.create") ||
    (mode === "slow-first-then-fast" && req.method === "snapshot.create" && !existsSync(stateFile ?? ""))
  if (slow) {
    if (stateFile) writeFileSync(stateFile, "done")
    setTimeout(() => {
      record({ event: "completed", method: req.method })
      handle(req)
    }, 5000)
    return
  }
  handle(req)
})

function handle(req) {
  switch (req.method) {
    case "ping":
      respond(req, { status: "ok", version: "0.0.0-fake" })
      break
    case "snapshot.create":
      created += 1
      if (mode === "null-result") {
        respond(req, null)
      } else if (mode === "array-result") {
        respond(req, [])
      } else if (mode === "scalar-result") {
        respond(req, "just a string")
      } else if (mode === "malformed-snapshot") {
        respond(req, { id: 42, commit: "0123456789ab" })
      } else {
        respond(req, {
          id: `20260101T000000.000000000Z-fake${process.pid}-${created}`,
          commit: "0123456789ab",
          reason: req.params?.reason ?? "manual",
          created_at: new Date().toISOString(),
          changed_files: [],
          reused: false,
        })
      }
      break
    case "snapshot.list":
      respond(req, { snapshots: [] })
      break
    default:
      if (mode === "bad-error") {
        respond(req, null, "boom")
      } else {
        respond(req, null, { code: -32601, message: "method not found" })
      }
  }
}

function respond(req, result, error) {
  console.log(JSON.stringify({ id: req?.id ?? null, result, error }))
}
