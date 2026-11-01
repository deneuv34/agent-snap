import { execFile, spawn, type ChildProcessByStdio } from "node:child_process"
import { createInterface, type Interface } from "node:readline"
import type { Readable, Writable } from "node:stream"
import {
  GROUP_POLL_MS,
  GUARDIAN_WAIT_MS,
  KILL_GRACE_MS,
  WINDOWS_TREE_WAIT_MS,
  groupEmptyDecision,
  windowsTreeCompletionDecision,
  type GroupState,
} from "./teardown.js"
import type {
  PingResult,
  SnapDiff,
  SnapDiffFile,
  SnapDiffHunk,
  SnapDiffPage,
  SnapDiffPatch,
  SnapDiffSide,
  SnapDiffStatus,
  SnapListResult,
  SnapOrigin,
  SnapSnapshot,
  SnapSnapshotDetail,
} from "./types.js"

type SnapChild = ChildProcessByStdio<Writable, Readable, null>

export class SnapRpcError extends Error {
  readonly code: number
  constructor(code: number, message: string) {
    super(message)
    this.name = "SnapRpcError"
    this.code = code
  }
}

export class SnapProtocolError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "SnapProtocolError"
  }
}

export interface SnapClientOptions {
  bin?: string
  serveArgs?: string[]
  env?: NodeJS.ProcessEnv
  timeoutMs?: number
  log?: (message: string) => void
}

interface Pending {
  child: SnapChild
  method: string
  resolve: (value: unknown) => void
  reject: (error: Error) => void
  timer: NodeJS.Timeout
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function errnoCode(err: unknown): string | undefined {
  return (err as { code?: unknown } | null)?.code as string | undefined
}

function isEsrch(err: unknown): boolean {
  return errnoCode(err) === "ESRCH"
}

/**
 * Observes whether the POSIX process group identified by `pid` still has any
 * member. Returns "live"/"empty" and throws on observation failures other
 * than ESRCH (e.g. EPERM), so callers fail closed rather than guessing.
 * Observation only — this never signals.
 */
function observeGroup(pid: number): GroupState {
  try {
    process.kill(-pid, 0)
    return "live"
  } catch (err) {
    if (isEsrch(err)) return "empty"
    throw err
  }
}

function groupSignalError(pid: number, err: unknown): Error {
  return new Error(`failed to inspect agent-snap process group ${pid}: ${err instanceof Error ? err.message : String(err)}`)
}

/**
 * The guardian supervisor (POSIX only).
 *
 * The client never signals a bare process group: it spawns this small
 * detached supervisor as the session and process-group leader, and the
 * guardian spawns agent-snap under it, so the whole tree (git, filters, …)
 * lives in the guardian's group. The guardian owns the group identity for
 * its entire lifetime:
 *
 *  - it ignores SIGTERM, so it is still alive while any delayed cleanup
 *    signal may need to be issued — the PGID cannot be recycled while a
 *    member (the guardian) provably exists;
 *  - it issues every group signal itself (SIGTERM now, SIGKILL after the
 *    grace period), and a signal executed by a member of the group can
 *    never hit a recycled PGID;
 *  - after it exits, the client only *observes* the group (kill(-pgid, 0)
 *    until ESRCH) — never signals — and fails closed if members remain.
 *
 * fd 3: control commands from the client ("shutdown").
 * fd 4: reports to the client ("child-exit …", "spawn-error …").
 * stdout/stdin: transparent passthrough to/from agent-snap.
 */
const GUARDIAN_SCRIPT = `
process.on("SIGTERM", function () {})
process.on("SIGINT", function () {})
var cp = require("node:child_process")
var fs = require("node:fs")
var readline = require("node:readline")
var shuttingDown = false
function shutdown() {
  if (shuttingDown) return
  shuttingDown = true
  try { process.kill(-process.pid, "SIGTERM") } catch (e) {}
  var poll = setInterval(function () {
    var members = -1
    try {
      members = cp
        .execFileSync("pgrep", ["-g", String(process.pid)], { encoding: "utf8" })
        .trim()
        .split("\\n")
        .filter(function (line) {
          return line.length > 0 && Number(line) !== process.pid
        }).length
    } catch (e) {
      // pgrep exits 1 when no process matches: it never lists itself, and
      // macOS does not list its caller either, so "no match" means no OTHER
      // members remain. Anything else (usage error, missing pgrep) stays
      // conservative (-1) and the grace-period SIGKILL settles teardown.
      members = e && e.status === 1 ? 0 : -1
    }
    if (members === 0) {
      clearInterval(poll)
      process.exit(0)
    }
  }, 100)
  setTimeout(function () {
    clearInterval(poll)
    try { process.kill(-process.pid, "SIGKILL") } catch (e) {}
    process.exit(0)
  }, ${KILL_GRACE_MS})
}
var child = null
delete process.env.AGENT_SNAP_GUARDIAN
delete process.env.BUN_BE_BUN
try {
  child = cp.spawn(process.argv[1], process.argv.slice(2), { stdio: ["pipe", "pipe", "inherit"], env: process.env })
} catch (e) {
  try { fs.writeSync(4, "spawn-error " + (e && e.message ? e.message : "unknown") + "\\n") } catch (e2) {}
  process.exit(1)
}
child.on("error", function (e) {
  try { fs.writeSync(4, "spawn-error " + (e && e.message ? e.message : "unknown") + "\\n") } catch (e2) {}
  process.exit(1)
})
child.on("exit", function (code, signal) {
  try { fs.writeSync(4, "child-exit " + (code === null ? "null" : String(code)) + " " + (signal || "none") + "\\n") } catch (e) {}
})
process.stdin.on("error", function () {})
process.stdin.pipe(child.stdin)
child.stdin.on("error", function () {})
child.stdout.pipe(process.stdout)
process.stdin.on("end", function () { shutdown() })
// On Node the control channel must be read via net.Socket, never
// fs.createReadStream: a raw-fd read is serviced by libuv's threadpool and
// blocks forever once the client stops writing, which deadlocks
// process.exit() in uv__threadpool_cleanup (the supervisor would never die).
// Bun's net.Socket never reads an inherited fd, so Bun reads the raw fd,
// which does not block its exit.
var control = process.versions.bun === undefined
  ? new (require("node:net").Socket)({ fd: 3 })
  : fs.createReadStream(null, { fd: 3 })
readline.createInterface({ input: control, terminal: false }).on("line", function (line) {
  if (line === "shutdown") shutdown()
})
`

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((entry) => typeof entry === "string")
}

function isOptionalString(value: unknown): value is string | undefined {
  return value === undefined || typeof value === "string"
}

// A 0.1.0 binary answers origin params with exactly this rejection, before it
// resolves the repository, so a create can be retried without them.
function rejectsOrigin(error: unknown): boolean {
  return (
    error instanceof SnapRpcError &&
    error.code === -32602 &&
    (error.message === "unknown param: source" || error.message === "unknown param: host")
  )
}

function validateSnapshot(result: unknown): SnapSnapshot {
  if (!isRecord(result)) throw new SnapProtocolError("snapshot.create result must be an object")
  const { id, commit, reason, created_at, source, host, changed_files, reused } = result
  if (typeof id !== "string" || typeof commit !== "string" || typeof reason !== "string" || typeof created_at !== "string") {
    throw new SnapProtocolError("snapshot.create result is missing string fields")
  }
  if (!isOptionalString(source) || !isOptionalString(host)) {
    throw new SnapProtocolError("snapshot.create result has a malformed origin")
  }
  if (changed_files !== undefined && !isStringArray(changed_files)) {
    throw new SnapProtocolError("snapshot.create result has malformed changed_files")
  }
  if (reused !== undefined && typeof reused !== "boolean") {
    throw new SnapProtocolError("snapshot.create result has malformed reused flag")
  }
  return {
    id,
    commit,
    reason,
    created_at,
    source,
    host,
    changed_files: changed_files as string[] | undefined,
    reused: reused as boolean | undefined,
  }
}

function validateDetail(result: unknown): SnapSnapshotDetail {
  if (!isRecord(result)) throw new SnapProtocolError("snapshot.get result must be an object")
  const { id, commit, reason, created_at, source, host, tree, parent, parent_id, changed_files, changed_files_truncated } = result
  if (
    typeof id !== "string" ||
    typeof commit !== "string" ||
    typeof reason !== "string" ||
    typeof created_at !== "string" ||
    typeof tree !== "string"
  ) {
    throw new SnapProtocolError("snapshot.get result is missing string fields")
  }
  if (!isOptionalString(source) || !isOptionalString(host)) {
    throw new SnapProtocolError("snapshot.get result has a malformed origin")
  }
  if ((parent !== undefined && typeof parent !== "string") || (parent_id !== undefined && typeof parent_id !== "string")) {
    throw new SnapProtocolError("snapshot.get result has a malformed parent")
  }
  if (!isStringArray(changed_files)) {
    throw new SnapProtocolError("snapshot.get result has malformed changed_files")
  }
  if (changed_files_truncated !== undefined && typeof changed_files_truncated !== "boolean") {
    throw new SnapProtocolError("snapshot.get result has malformed changed_files_truncated flag")
  }
  return {
    id,
    commit,
    reason,
    created_at,
    source,
    host,
    tree,
    parent: parent as string | undefined,
    parent_id: parent_id as string | undefined,
    changed_files,
    changed_files_truncated: changed_files_truncated as boolean | undefined,
  }
}

const MAX_DIFF_FILES = 200
const MAX_PATCH_BYTES_REPORTED = 1_048_576
const DIFF_STATUSES: readonly string[] = ["added", "deleted", "modified", "renamed", "type_changed"]
const DIFF_PATCHES: readonly string[] = ["full", "truncated", "deferred", "binary"]

function isCount(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0
}

function validateDiffSide(side: unknown, name: string): SnapDiffSide {
  if (!isRecord(side) || typeof side.id !== "string" || typeof side.commit !== "string") {
    throw new SnapProtocolError(`snapshot.diff result has a malformed ${name}`)
  }
  return { id: side.id, commit: side.commit }
}

function hasDiffBodyLines(text: string): boolean {
  let start = 0
  while (start < text.length) {
    const end = text.indexOf("\n", start)
    if (end === -1 || end === start || !" +-\\".includes(text[start] as string)) return false
    start = end + 1
  }
  return true
}

function validateDiffHunk(hunk: unknown): SnapDiffHunk {
  if (!isRecord(hunk)) throw new SnapProtocolError("snapshot.diff result has a malformed hunk")
  const { old_start, old_lines, new_start, new_lines, section, text } = hunk
  if (!isCount(old_start) || !isCount(old_lines) || !isCount(new_start) || !isCount(new_lines)) {
    throw new SnapProtocolError("snapshot.diff result has a malformed hunk range")
  }
  if (!isOptionalString(section)) throw new SnapProtocolError("snapshot.diff result has a malformed hunk section")
  if (typeof text !== "string" || !hasDiffBodyLines(text)) {
    throw new SnapProtocolError("snapshot.diff result has malformed hunk text")
  }
  return { old_start, old_lines, new_start, new_lines, section, text }
}

function validateDiffFile(file: unknown): SnapDiffFile {
  if (!isRecord(file)) throw new SnapProtocolError("snapshot.diff result has a malformed file entry")
  const { status, path, old_path, similarity, old_mode, new_mode, old_oid, new_oid, additions, deletions, patch, patch_bytes, hunks, lossy } = file
  if (typeof status !== "string" || !DIFF_STATUSES.includes(status)) {
    throw new SnapProtocolError("snapshot.diff result has a malformed file status")
  }
  if (typeof patch !== "string" || !DIFF_PATCHES.includes(patch)) {
    throw new SnapProtocolError("snapshot.diff result has a malformed patch kind")
  }
  if (typeof path !== "string" || !isOptionalString(old_path)) {
    throw new SnapProtocolError("snapshot.diff result has a malformed file path")
  }
  if (!isOptionalString(old_mode) || !isOptionalString(new_mode) || !isOptionalString(old_oid) || !isOptionalString(new_oid)) {
    throw new SnapProtocolError("snapshot.diff result has a malformed mode or object ID")
  }
  if (!isCount(additions) || !isCount(deletions)) {
    throw new SnapProtocolError("snapshot.diff result has malformed line counts")
  }
  if (similarity !== undefined && (!isCount(similarity) || similarity < 50 || similarity > 100)) {
    throw new SnapProtocolError("snapshot.diff result has a malformed similarity")
  }
  if (patch_bytes !== undefined && !isCount(patch_bytes)) {
    throw new SnapProtocolError("snapshot.diff result has a malformed patch_bytes")
  }
  if (lossy !== undefined && lossy !== true) {
    throw new SnapProtocolError("snapshot.diff result has a malformed lossy flag")
  }
  if (!Array.isArray(hunks)) throw new SnapProtocolError("snapshot.diff result has malformed hunks")

  const renamed = status === "renamed"
  if ((old_path !== undefined) !== renamed || (similarity !== undefined) !== renamed) {
    throw new SnapProtocolError("snapshot.diff old_path and similarity must be present exactly for renamed files")
  }
  const added = status === "added"
  if ((old_mode === undefined) !== added || (old_oid === undefined) !== added) {
    throw new SnapProtocolError("snapshot.diff old_mode and old_oid must be absent exactly for added files")
  }
  const deleted = status === "deleted"
  if ((new_mode === undefined) !== deleted || (new_oid === undefined) !== deleted) {
    throw new SnapProtocolError("snapshot.diff new_mode and new_oid must be absent exactly for deleted files")
  }
  if (patch === "binary" && (additions !== 0 || deletions !== 0 || hunks.length !== 0)) {
    throw new SnapProtocolError("snapshot.diff binary file must have zero line counts and no hunks")
  }
  if (patch === "deferred" && hunks.length !== 0) {
    throw new SnapProtocolError("snapshot.diff deferred file must have no hunks")
  }
  if (patch_bytes !== undefined && patch !== "truncated") {
    throw new SnapProtocolError("snapshot.diff patch_bytes is allowed only on truncated files")
  }
  if (patch_bytes !== undefined && patch_bytes > MAX_PATCH_BYTES_REPORTED) {
    throw new SnapProtocolError("snapshot.diff patch_bytes exceeds the reportable limit")
  }
  return {
    status: status as SnapDiffStatus,
    path,
    old_path,
    similarity,
    old_mode,
    new_mode,
    old_oid,
    new_oid,
    additions,
    deletions,
    patch: patch as SnapDiffPatch,
    patch_bytes,
    hunks: hunks.map((hunk) => validateDiffHunk(hunk)),
    lossy,
  }
}

function validateDiff(result: unknown): SnapDiff {
  if (!isRecord(result)) throw new SnapProtocolError("snapshot.diff result must be an object")
  const { from, to, offset, next_offset, totals, files } = result
  const fromSide = validateDiffSide(from, "from")
  const toSide = validateDiffSide(to, "to")
  if (!isCount(offset) || (next_offset !== undefined && !isCount(next_offset))) {
    throw new SnapProtocolError("snapshot.diff result has a malformed offset")
  }
  if (!isRecord(totals) || !isCount(totals.files) || !isCount(totals.additions) || !isCount(totals.deletions)) {
    throw new SnapProtocolError("snapshot.diff result has malformed totals")
  }
  if (!Array.isArray(files) || files.length > MAX_DIFF_FILES) {
    throw new SnapProtocolError("snapshot.diff result has malformed files")
  }
  if (files.length > 0 && offset + files.length > totals.files) {
    throw new SnapProtocolError("snapshot.diff page extends past totals.files")
  }
  if (next_offset !== undefined && (next_offset <= offset || next_offset > offset + files.length)) {
    throw new SnapProtocolError("snapshot.diff next_offset is outside the page")
  }
  return {
    from: fromSide,
    to: toSide,
    offset,
    next_offset,
    totals: { files: totals.files, additions: totals.additions, deletions: totals.deletions },
    files: files.map((file) => validateDiffFile(file)),
  }
}

function validateResult(method: string, result: unknown): unknown {
  switch (method) {
    case "ping": {
      if (!isRecord(result) || typeof result.status !== "string" || typeof result.version !== "string") {
        throw new SnapProtocolError("ping result must be {status, version}")
      }
      return { status: result.status, version: result.version } as PingResult
    }
    case "snapshot.create":
      return validateSnapshot(result)
    case "snapshot.list": {
      if (!isRecord(result) || !Array.isArray(result.snapshots)) {
        throw new SnapProtocolError("snapshot.list result must be {snapshots: [...]}")
      }
      const snapshots = result.snapshots.map((entry) => validateSnapshot(entry))
      return { snapshots } as SnapListResult
    }
    case "snapshot.get":
      return validateDetail(result)
    case "snapshot.diff":
      return validateDiff(result)
    default:
      throw new SnapProtocolError(`response for "${method}" has a result this client cannot validate`)
  }
}

function validateError(error: unknown): Error {
  if (!isRecord(error) || typeof error.code !== "number" || typeof error.message !== "string") {
    return new SnapProtocolError("response has a malformed error object")
  }
  return new SnapRpcError(error.code, error.message)
}

export function abortError(): Error {
  return new DOMException("The operation was aborted", "AbortError")
}

export class SnapClient {
  readonly #bin: string
  readonly #serveArgs: string[]
  readonly #env: NodeJS.ProcessEnv | undefined
  readonly #timeoutMs: number
  readonly #log: (message: string) => void
  #child: SnapChild | null = null
  #rl: Interface | null = null
  #reportRl: Interface | null = null
  #pending = new Map<number, Pending>()
  #nextId = 1
  // Every teardown starts immediately and runs concurrently; this array is
  // the aggregate barrier close() drains. A teardown never rejects here —
  // failures are latched per child in #failedChildren.
  #teardowns: Array<Promise<void>> = []
  // Latch: a child stays failed until a retry actually verifies emptiness.
  // Never consumed into a false success.
  #failedChildren = new Map<SnapChild, Error>()
  #killPromises = new WeakMap<SnapChild, Promise<void>>()
  #tearingDown = new WeakSet<SnapChild>()
  #closing = false
  #originDropLogged = false

  constructor(options: SnapClientOptions = {}) {
    this.#bin = options.bin ?? SnapClient.defaultBin()
    this.#serveArgs = options.serveArgs ?? ["serve"]
    this.#env = options.env
    this.#timeoutMs = options.timeoutMs ?? 30_000
    this.#log = options.log ?? (() => {})
  }

  static defaultBin(env: NodeJS.ProcessEnv = process.env): string {
    const explicit = env.AGENT_SNAP_BIN?.trim()
    return explicit && explicit.length > 0 ? explicit : "agent-snap"
  }

  static fromEnv(env: NodeJS.ProcessEnv = process.env, options: SnapClientOptions = {}): SnapClient {
    return new SnapClient({ ...options, bin: options.bin ?? SnapClient.defaultBin(env) })
  }

  get bin(): string {
    return this.#bin
  }

  get pid(): number | undefined {
    return this.#child?.pid
  }

  async ping(signal?: AbortSignal): Promise<PingResult> {
    return this.request<PingResult>("ping", undefined, { signal })
  }

  async create(cwd: string, reason?: string, signal?: AbortSignal, origin?: SnapOrigin): Promise<SnapSnapshot> {
    const params: Record<string, unknown> = { cwd }
    if (reason && reason.trim().length > 0) params.reason = reason
    if (origin?.source === undefined && origin?.host === undefined) {
      return this.request<SnapSnapshot>("snapshot.create", params, { signal })
    }
    const withOrigin = { ...params }
    if (origin.source !== undefined) withOrigin.source = origin.source
    if (origin.host !== undefined) withOrigin.host = origin.host
    try {
      return await this.request<SnapSnapshot>("snapshot.create", withOrigin, { signal })
    } catch (error) {
      if (!rejectsOrigin(error)) throw error
    }
    // A 0.1.0 binary stores any reason, so the retry must not send one that a
    // newer reader would take as origin.
    if (typeof params.reason === "string" && params.reason.includes("\n\nAgent-Snap-")) {
      throw new Error("agent-snap binary does not accept snapshot origin; refusing to send it a reason that ends like an origin paragraph")
    }
    if (!this.#originDropLogged) {
      this.#originDropLogged = true
      this.#log("agent-snap binary does not accept snapshot origin; creating snapshots without source and host. Upgrade agent-snap to record them.")
    }
    return this.request<SnapSnapshot>("snapshot.create", params, { signal })
  }

  async list(cwd: string, limit?: number, signal?: AbortSignal): Promise<SnapSnapshot[]> {
    const params: Record<string, unknown> = { cwd }
    if (limit !== undefined && limit > 0) params.limit = limit
    const result = await this.request<SnapListResult>("snapshot.list", params, { signal })
    return result.snapshots ?? []
  }

  async get(cwd: string, id: string, signal?: AbortSignal): Promise<SnapSnapshotDetail> {
    return this.request<SnapSnapshotDetail>("snapshot.get", { cwd, id }, { signal })
  }

  async diff(cwd: string, from: string, to: string, page?: SnapDiffPage, signal?: AbortSignal): Promise<SnapDiff> {
    const params: Record<string, unknown> = { cwd, from, to }
    if (page?.offset !== undefined) params.offset = page.offset
    if (page?.maxPatchBytes !== undefined) params.max_patch_bytes = page.maxPatchBytes
    return this.request<SnapDiff>("snapshot.diff", params, { signal })
  }

  /**
   * Sends one JSON-RPC request. Requests made while close() is in progress
   * reject with "agent-snap client is closing" — close() must exclusively
   * cover the children that existed when it started, so no untracked
   * replacement can be spawned mid-close. Once close() has resolved the
   * client may be reused (it respawns lazily).
   */
  async request<T>(method: string, params?: Record<string, unknown>, options: { signal?: AbortSignal } = {}): Promise<T> {
    const signal = options.signal
    if (signal?.aborted) {
      throw abortError()
    }
    this.#ensureStarted()
    const child = this.#child
    if (child === null) {
      throw new Error("agent-snap process is not running")
    }
    const id = this.#nextId++
    const payload = JSON.stringify(params === undefined ? { id, method } : { id, method, params })
    return await new Promise<T>((resolve, reject) => {
      let settled = false
      const onAbort = () => {
        if (settled) return
        settled = true
        clearTimeout(timer)
        this.#pending.delete(id)
        this.#startTeardown(child, `agent-snap request "${method}" aborted`)
        reject(abortError())
      }
      const timer = setTimeout(() => {
        settled = true
        signal?.removeEventListener("abort", onAbort)
        this.#pending.delete(id)
        this.#startTeardown(child, `agent-snap request "${method}" timed out after ${this.#timeoutMs}ms`)
        reject(new Error(`agent-snap request "${method}" timed out after ${this.#timeoutMs}ms`))
      }, this.#timeoutMs)
      signal?.addEventListener("abort", onAbort, { once: true })
      this.#pending.set(id, {
        child,
        method,
        resolve: (value: unknown) => {
          settled = true
          clearTimeout(timer)
          signal?.removeEventListener("abort", onAbort)
          resolve(value as T)
        },
        reject: (error: Error) => {
          settled = true
          clearTimeout(timer)
          signal?.removeEventListener("abort", onAbort)
          reject(error)
        },
        timer,
      })
      child.stdin.write(payload + "\n", (err) => {
        if (err === null || err === undefined) return
        if (settled) return
        settled = true
        clearTimeout(timer)
        signal?.removeEventListener("abort", onAbort)
        this.#pending.delete(id)
        this.#startTeardown(child, `failed to write to agent-snap stdin: ${err.message}`)
        reject(new Error(`failed to write to agent-snap stdin: ${err.message}`))
      })
    })
  }

  /**
   * Terminates the current child and awaits every teardown — the one just
   * started and any earlier ones (timeouts/aborts/unexpected exits) that
   * are still reaping their process trees. Rejects if any teardown failed
   * to verify the tree is gone; a failure stays latched across close()
   * calls until a retry actually verifies emptiness. Never resolves while
   * descendants may be alive.
   */
  async close(): Promise<void> {
    this.#closing = true
    try {
      const child = this.#child
      this.#child = null
      this.#rl?.close()
      this.#rl = null
      this.#reportRl?.close()
      this.#reportRl = null
      if (child !== null) {
        this.#startTeardown(child, "agent-snap client closed")
      }
      await this.#drainTeardowns()
      for (const child of [...this.#failedChildren.keys()]) {
        try {
          await this.#killAndWait(child)
          this.#failedChildren.delete(child)
        } catch (err) {
          this.#failedChildren.set(child, err instanceof Error ? err : new Error(String(err)))
        }
      }
      if (this.#failedChildren.size > 0) {
        throw [...this.#failedChildren.values()][0]
      }
    } finally {
      this.#closing = false
    }
  }

  async #drainTeardowns(): Promise<void> {
    for (;;) {
      const batch = this.#teardowns
      if (batch.length === 0) return
      this.#teardowns = []
      await Promise.all(batch)
    }
  }

  #ensureStarted(): void {
    if (this.#closing) {
      throw new Error("agent-snap client is closing")
    }
    if (this.#child !== null && this.#child.exitCode === null && this.#child.signalCode === null) return
    const env = this.#env === undefined ? process.env : { ...process.env, ...this.#env }
    if (process.platform === "win32") {
      const child = spawn(this.#bin, this.#serveArgs, {
        stdio: ["pipe", "pipe", "inherit"],
        env,
      })
      this.#child = child
      const rl = createInterface({ input: child.stdout })
      this.#rl = rl
      rl.on("line", (line) => this.#onLine(line))
      child.on("error", (err: NodeJS.ErrnoException) => {
        const message =
          err.code === "ENOENT"
            ? `agent-snap binary not found at "${this.#bin}" — install it (go install ./cmd/agent-snap) or set AGENT_SNAP_BIN`
            : `agent-snap failed to start: ${err.message}`
        this.#log(message)
        this.#failAll(new Error(message), child)
        if (this.#child === child) this.#child = null
        rl.close()
      })
      child.on("exit", (code, signal) => {
        this.#log(`agent-snap exited (code=${code}, signal=${signal ?? "none"})`)
        const message = `agent-snap exited unexpectedly (code=${code}, signal=${signal ?? "none"})`
        this.#failAll(new Error(message), child)
        if (this.#child === child) this.#child = null
        rl.close()
        if (!this.#tearingDown.has(child)) {
          this.#startTeardown(child, message)
        }
      })
      return
    }
    // POSIX: spawn the guardian supervisor, which owns the process group
    // and spawns agent-snap inside it. In a compiled Bun host such as
    // OpenCode, process.execPath is the host's own CLI, which rejects
    // --eval; BUN_BE_BUN makes it act as the bun CLI. An executable that
    // ignores both runs its own entrypoint instead, and that copy would
    // spawn another: AGENT_SNAP_GUARDIAN marks the copy so it refuses. The
    // guardian deletes both so agent-snap's tree never inherits them.
    if (process.env.AGENT_SNAP_GUARDIAN !== undefined) {
      throw new Error("agent-snap supervisor cannot start: this host's executable ran its own entrypoint instead of the supervisor script")
    }
    const guardianEnv: NodeJS.ProcessEnv = { ...env, AGENT_SNAP_GUARDIAN: "1" }
    if (process.versions.bun !== undefined) guardianEnv.BUN_BE_BUN = "1"
    const child = spawn(process.execPath, ["--eval", GUARDIAN_SCRIPT, this.#bin, ...this.#serveArgs], {
      stdio: ["pipe", "pipe", "inherit", "pipe", "pipe"],
      env: guardianEnv,
      detached: true,
    }) as SnapChild
    this.#child = child
    const rl = createInterface({ input: child.stdout })
    this.#rl = rl
    rl.on("line", (line) => this.#onLine(line))
    const reportRl = createInterface({ input: child.stdio[4] as Readable })
    this.#reportRl = reportRl
    reportRl.on("line", (line) => this.#onReport(child, line))
    // Extra stdio fds are duplex sockets on Linux: when the guardian dies with
    // an unread control command, the client's side resets with ECONNRESET.
    // The exit handler below owns that failure, so the reset is only logged
    // instead of escaping as an uncaught exception that crashes the host.
    const control = child.stdio[3] as Writable
    control.on("error", (err) => this.#log(`agent-snap control pipe error: ${err.message}`))
    child.on("error", (err: NodeJS.ErrnoException) => {
      const message = `agent-snap supervisor failed to start: ${err.message}`
      this.#log(message)
      this.#failAll(new Error(message), child)
      if (this.#child === child) this.#child = null
      rl.close()
      reportRl.close()
    })
    child.on("exit", (code, signal) => {
      this.#log(`agent-snap supervisor exited (code=${code}, signal=${signal ?? "none"})`)
      const message = `agent-snap supervisor exited (code=${code}, signal=${signal ?? "none"})`
      this.#failAll(new Error(message), child)
      if (this.#child === child) this.#child = null
      reportRl.close()
      if (!this.#tearingDown.has(child)) {
        this.#startTeardown(child, message)
      }
    })
  }

  #onReport(child: SnapChild, line: string): void {
    if (line.startsWith("child-exit ")) {
      const parts = line.slice("child-exit ".length).trim().split(/\s+/)
      const code = parts[0] ?? "null"
      const signal = parts[1] ?? "none"
      this.#log(`agent-snap exited (code=${code}, signal=${signal === "none" ? "none" : signal})`)
      const message = `agent-snap exited unexpectedly (code=${code}, signal=${signal})`
      this.#failAll(new Error(message), child)
      if (this.#child === child) this.#child = null
      this.#startTeardown(child, message)
      return
    }
    if (line.startsWith("spawn-error ")) {
      const detail = line.slice("spawn-error ".length)
      const message = detail.includes("ENOENT")
        ? `agent-snap binary not found at "${this.#bin}" — install it (go install ./cmd/agent-snap) or set AGENT_SNAP_BIN`
        : `agent-snap failed to start: ${detail}`
      this.#log(message)
      this.#failAll(new Error(message), child)
      if (this.#child === child) this.#child = null
      this.#startTeardown(child, message)
      return
    }
  }

  /**
   * Starts a teardown for a child. Each teardown begins immediately (its
   * first signal is never delayed behind another tree's grace period) and
   * runs concurrently; failures latch on the child until verified away.
   */
  #startTeardown(child: SnapChild, message: string): void {
    this.#tearingDown.add(child)
    if (this.#child === child) this.#child = null
    this.#failAll(new Error(message), child)
    const teardown = this.#killAndWait(child).catch((err: unknown) => {
      this.#failedChildren.set(child, err instanceof Error ? err : new Error(String(err)))
    })
    this.#teardowns.push(teardown)
  }

  #killAndWait(child: SnapChild): Promise<void> {
    const existing = this.#killPromises.get(child)
    if (existing !== undefined) return existing
    const promise = new Promise<void>((resolve, reject) => {
      const pid = child.pid
      if (pid === undefined) {
        resolve()
        return
      }
      if (process.platform === "win32") {
        this.#killTreeWindows(child, pid, resolve, reject)
        return
      }
      this.#killGuardianPosix(child, pid, resolve, reject)
    }).finally(() => this.#killPromises.delete(child))
    this.#killPromises.set(child, promise)
    return promise
  }

  /**
   * POSIX teardown with the guardian supervisor.
   *
   * Invariant (no probe-then-signal): the client never signals a process
   * group by number. All group signals (TERM now, KILL after the grace
   * period) are issued by the guardian itself, which is a live member of
   * the group at the instant of the syscall — a member-executed group
   * signal cannot observe a recycled PGID. While the guardian lives, the
   * PGID cannot recycle at all. Once the guardian exits, the client only
   * observes (kill(-pgid, 0) until ESRCH) and fails closed if members
   * remain. The only client-issued signal is a last-resort SIGKILL to the
   * guardian's own pid, gated on it still being unreaped (an unreaped
   * child's pid cannot have been recycled).
   */
  #killGuardianPosix(child: SnapChild, pid: number, resolve: () => void, reject: (error: Error) => void): void {
    if (child.exitCode !== null || child.signalCode !== null) {
      this.#awaitGroupEmpty(pid, resolve, reject)
      return
    }
    let finished = false
    const done = () => {
      if (finished) return
      finished = true
      clearTimeout(hard)
      resolve()
    }
    const fail = (err: Error) => {
      if (finished) return
      finished = true
      clearTimeout(hard)
      reject(err)
    }
    const verify = () => this.#awaitGroupEmpty(pid, done, fail)
    child.once("exit", verify)
    const hard = setTimeout(() => {
      if (child.exitCode === null && child.signalCode === null) {
        try {
          child.kill("SIGKILL")
        } catch {
          // already gone
        }
        setTimeout(() => {
          if (child.exitCode === null && child.signalCode === null) {
            fail(new Error(`agent-snap supervisor (pid ${pid}) did not exit after being killed`))
          } else {
            verify()
          }
        }, 2_000)
        return
      }
      verify()
    }, GUARDIAN_WAIT_MS)
    const control = child.stdio[3] as Writable | undefined
    if (control === undefined || control === null || !control.writable) {
      fail(new Error(`agent-snap supervisor (pid ${pid}) has no control channel`))
      return
    }
    control.write("shutdown\n", (err) => {
      if (err === null || err === undefined) return
      // The supervisor died before we could command it: settle by
      // observation only, never by signalling a bare group.
      verify()
    })
  }

  /**
   * Observation-only emptiness verification: wait for kill(-pgid, 0) to
   * report ESRCH, fail closed on observation errors (EPERM etc.) or when
   * the window expires with members remaining.
   */
  #awaitGroupEmpty(pid: number, resolve: () => void, reject: (error: Error) => void): void {
    let finished = false
    const started = Date.now()
    const done = () => {
      if (finished) return
      finished = true
      clearInterval(timer)
      resolve()
    }
    const fail = (err: Error) => {
      if (finished) return
      finished = true
      clearInterval(timer)
      reject(err)
    }
    const timer = setInterval(() => {
      let state: GroupState
      try {
        state = observeGroup(pid)
      } catch (err) {
        fail(groupSignalError(pid, err))
        return
      }
      const action = groupEmptyDecision({ state, elapsedMs: Date.now() - started })
      if (action === "done") {
        done()
      } else if (action === "fail") {
        fail(new Error(`agent-snap process group ${pid} did not terminate within the verification window`))
      }
    }, GROUP_POLL_MS)
  }

  /**
   * Windows tree teardown. Cleanup succeeds only when BOTH conditions hold:
   * taskkill /T /F reported success AND the root exited — within a bounded
   * window. A taskkill failure always fails closed, even if the root has
   * exited (its tree survives the root on Windows).
   */
  #killTreeWindows(child: SnapChild, pid: number, resolve: () => void, reject: (error: Error) => void): void {
    if (child.exitCode !== null || child.signalCode !== null) {
      reject(
        new Error(
          `agent-snap (pid ${pid}) exited before its process tree could be terminated on Windows; descendants may still be running`,
        ),
      )
      return
    }
    let finished = false
    let taskkillDone = false
    let taskkillFailed = false
    let rootExited = false
    const started = Date.now()
    const settle = () => {
      if (finished) return
      const action = windowsTreeCompletionDecision({
        taskkillDone,
        taskkillFailed,
        rootExited,
        elapsedMs: Date.now() - started,
      })
      if (action === "wait") return
      finished = true
      clearTimeout(bound)
      if (action === "done") {
        resolve()
        return
      }
      reject(
        taskkillFailed
          ? new Error(`taskkill failed for agent-snap (pid ${pid}): the process tree may still be running`)
          : new Error(`agent-snap (pid ${pid}) did not terminate within the Windows tree-cleanup window`),
      )
    }
    const bound = setTimeout(settle, WINDOWS_TREE_WAIT_MS)
    child.once("exit", () => {
      rootExited = true
      settle()
    })
    execFile("taskkill", ["/pid", String(pid), "/T", "/F"], { windowsHide: true }, (err) => {
      if (err !== null) {
        taskkillFailed = true
        try {
          child.kill("SIGKILL")
        } catch {
          // already gone
        }
      } else {
        taskkillDone = true
      }
      settle()
    })
  }

  #onLine(line: string): void {
    const trimmed = line.trim()
    if (trimmed.length === 0) return
    let parsed: unknown
    try {
      parsed = JSON.parse(trimmed)
    } catch {
      this.#log(`ignoring non-JSON line from agent-snap: ${trimmed.slice(0, 200)}`)
      return
    }
    if (!isRecord(parsed)) {
      this.#log(`ignoring non-object line from agent-snap: ${trimmed.slice(0, 200)}`)
      return
    }
    if (typeof parsed.id !== "number") return
    const pending = this.#pending.get(parsed.id)
    if (pending === undefined) return
    this.#pending.delete(parsed.id)
    clearTimeout(pending.timer)
    try {
      if ("error" in parsed) {
        pending.reject(validateError(parsed.error))
        return
      }
      if (!("result" in parsed)) {
        pending.reject(new SnapProtocolError(`response for "${pending.method}" has neither result nor error`))
        return
      }
      pending.resolve(validateResult(pending.method, parsed.result))
    } catch (err) {
      pending.reject(err instanceof Error ? err : new SnapProtocolError(String(err)))
    }
  }

  #failAll(error: Error, child: SnapChild | null): void {
    for (const [id, pending] of this.#pending) {
      if (child !== null && pending.child !== child) continue
      clearTimeout(pending.timer)
      pending.reject(error)
      this.#pending.delete(id)
    }
  }
}
