import { appendFileSync, existsSync, readFileSync, writeFileSync } from "node:fs"
import { spawn } from "node:child_process"
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

if (mode === "survive-eof") {
  // Keep running even if stdin reaches EOF (e.g. the guardian supervisor is
  // killed), so tests can exercise cleanup of a tree that survives its
  // supervisor.
  setInterval(() => {}, 10_000)
}

if (mode === "record-env") {
  record({ event: "env", guardian: process.env.AGENT_SNAP_GUARDIAN ?? null, bunBeBun: process.env.BUN_BE_BUN ?? null })
}

const rl = createInterface({ input: process.stdin })
let created = 0
let grandchildSpawned = false

rl.on("line", (line) => {
  let req
  try {
    req = JSON.parse(line)
  } catch {
    respond(null, null, { code: -32700, message: "parse error" })
    return
  }
  record({ event: "request", method: req.method, params: req.params ?? null })
  if (mode === "grandchild" && req.method === "ping" && !grandchildSpawned) {
    grandchildSpawned = true
    // The grandchild reports its own termination: the fake process itself is
    // dead by the time the group signal is handled, so it cannot record it.
    // The grandchild first installs its TERM trap, then signals readiness;
    // the fake only answers once ready, so a close() racing the spawn can
    // never TERM the shell before its trap exists.
    spawn(
      "sh",
      ["-c", `trap 'echo grandchild-exited >> "$FAKE_LOG"' TERM; echo grandchild-ready >> "$FAKE_LOG"; sleep 30`],
      { stdio: "ignore", env: { ...process.env, FAKE_LOG: logFile ?? "" } },
    )
    const respondWhenReady = () => {
      try {
        if (readFileSync(logFile ?? "", "utf8").includes("grandchild-ready")) {
          handle(req)
          return
        }
      } catch {
        // log not written yet
      }
      setTimeout(respondWhenReady, 10)
    }
    respondWhenReady()
    return
  }
  if (mode === "exit-on-list" && req.method === "snapshot.list") {
    process.exit(0)
  }
  if (mode === "exit-on-ping" && req.method === "ping") {
    process.exit(0)
  }
  if (mode === "slow-create-grandchild" && req.method === "snapshot.create" && !grandchildSpawned) {
    grandchildSpawned = true
    // Hang forever with a grandchild so the client times out and must tear
    // down the whole process group. The grandchild reports its own
    // termination; the fake never responds to the create.
    spawn(
      "sh",
      ["-c", `trap 'echo grandchild-exited >> "$FAKE_LOG"' TERM; sleep 30`],
      { stdio: "ignore", env: { ...process.env, FAKE_LOG: logFile ?? "" } },
    )
    return
  }
  if (mode === "stubborn-create-grandchild" && req.method === "snapshot.create" && !grandchildSpawned) {
    grandchildSpawned = true
    // Like slow-create-grandchild, but the grandchild traps SIGTERM and
    // keeps sleeping, so the tree only dies when the guardian escalates to
    // SIGKILL after the grace period — this makes teardown duration
    // observable for concurrency tests.
    spawn(
      "sh",
      ["-c", `trap 'echo grandchild-saw-term >> "$FAKE_LOG"' TERM; sleep 30`],
      { stdio: "ignore", env: { ...process.env, FAKE_LOG: logFile ?? "" } },
    )
    return
  }
  if (mode === "v010" && req.method === "snapshot.create") {
    // A 0.1.0 binary rejects keys it does not know before any other work.
    const unknown = Object.keys(req.params ?? {}).find((key) => key !== "cwd" && key !== "reason")
    if (unknown !== undefined) {
      respond(req, null, { code: -32602, message: `unknown param: ${unknown}` })
      return
    }
  }
  if (mode === "invalid-origin" && req.method === "snapshot.create") {
    respond(req, null, { code: -32602, message: "param must be an origin value: source" })
    return
  }
  if (mode === "internal-unknown-param" && req.method === "snapshot.create") {
    respond(req, null, { code: -32603, message: "unknown param: source" })
    return
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
          source: mode === "malformed-origin" ? 7 : req.params?.source,
          host: mode === "malformed-origin-host" ? 7 : req.params?.host,
          changed_files: [],
          reused: false,
        })
      }
      break
    case "snapshot.list":
      respond(req, { snapshots: [] })
      break
    case "snapshot.get":
      if (mode === "detail-not-found") {
        respond(req, null, { code: -32001, message: `snapshot not found: ${req.params?.id}` })
      } else if (mode === "detail-scalar") {
        respond(req, "just a string")
      } else {
        respond(req, { ...detail(req.params?.id), ...(malformedDetails[mode] ?? {}) })
      }
      break
    default:
      if (mode === "bad-error") {
        respond(req, null, "boom")
      } else {
        respond(req, null, { code: -32601, message: "method not found" })
      }
  }
}

const malformedDetails = {
  "detail-missing-tree": { tree: undefined },
  "detail-missing-changed-files": { changed_files: undefined },
  "detail-bad-changed-files": { changed_files: [1] },
  "detail-bad-parent": { parent: 7 },
  "detail-bad-parent-id": { parent_id: null },
  "detail-bad-truncated": { changed_files_truncated: "yes" },
  "detail-bad-source": { source: 7 },
  "detail-bad-host": { host: null },
}

function detail(id) {
  return {
    id,
    commit: "0123456789ab0000000000000000000000000000",
    reason: "manual",
    created_at: "2026-01-01T00:00:00Z",
    tree: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
    parent: "fedcba9876540000000000000000000000000000",
    parent_id: "20251231T000000.000000000Z-fedcba987654",
    changed_files: ["src/a.ts"],
  }
}

function respond(req, result, error) {
  console.log(JSON.stringify({ id: req?.id ?? null, result, error }))
}
