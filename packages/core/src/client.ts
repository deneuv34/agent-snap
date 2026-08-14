import { execFile, spawn, type ChildProcessByStdio } from "node:child_process"
import { createInterface, type Interface } from "node:readline"
import type { Readable, Writable } from "node:stream"
import type { PingResult, SnapListResult, SnapSnapshot } from "./types.js"

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
  child: ChildProcessByStdio<Writable, Readable, null>
  method: string
  resolve: (value: unknown) => void
  reject: (error: Error) => void
  timer: NodeJS.Timeout
}

const KILL_GRACE_MS = 2_000

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((entry) => typeof entry === "string")
}

function validateSnapshot(result: unknown): SnapSnapshot {
  if (!isRecord(result)) throw new SnapProtocolError("snapshot.create result must be an object")
  const { id, commit, reason, created_at, changed_files, reused } = result
  if (typeof id !== "string" || typeof commit !== "string" || typeof reason !== "string" || typeof created_at !== "string") {
    throw new SnapProtocolError("snapshot.create result is missing string fields")
  }
  if (changed_files !== undefined && !isStringArray(changed_files)) {
    throw new SnapProtocolError("snapshot.create result has malformed changed_files")
  }
  if (reused !== undefined && typeof reused !== "boolean") {
    throw new SnapProtocolError("snapshot.create result has malformed reused flag")
  }
  return { id, commit, reason, created_at, changed_files: changed_files as string[] | undefined, reused: reused as boolean | undefined }
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
    default:
      return result
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
  #child: ChildProcessByStdio<Writable, Readable, null> | null = null
  #rl: Interface | null = null
  #pending = new Map<number, Pending>()
  #nextId = 1

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

  async ping(signal?: AbortSignal): Promise<PingResult> {
    return this.request<PingResult>("ping", undefined, { signal })
  }

  async create(cwd: string, reason?: string, signal?: AbortSignal): Promise<SnapSnapshot> {
    const params: Record<string, unknown> = { cwd }
    if (reason && reason.trim().length > 0) params.reason = reason
    return this.request<SnapSnapshot>("snapshot.create", params, { signal })
  }

  async list(cwd: string, limit?: number, signal?: AbortSignal): Promise<SnapSnapshot[]> {
    const params: Record<string, unknown> = { cwd }
    if (limit !== undefined && limit > 0) params.limit = limit
    const result = await this.request<SnapListResult>("snapshot.list", params, { signal })
    return result.snapshots ?? []
  }

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
        void this.#teardown(child, `agent-snap request "${method}" aborted`)
        reject(abortError())
      }
      const timer = setTimeout(() => {
        settled = true
        signal?.removeEventListener("abort", onAbort)
        this.#pending.delete(id)
        void this.#teardown(child, `agent-snap request "${method}" timed out after ${this.#timeoutMs}ms`)
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
        void this.#teardown(child, `failed to write to agent-snap stdin: ${err.message}`)
        reject(new Error(`failed to write to agent-snap stdin: ${err.message}`))
      })
    })
  }

  async close(): Promise<void> {
    const child = this.#child
    this.#child = null
    this.#rl?.close()
    this.#rl = null
    this.#failAll(new Error("agent-snap client closed"), child)
    if (child !== null) await this.#killAndWait(child)
  }

  #ensureStarted(): void {
    if (this.#child !== null && this.#child.exitCode === null && this.#child.signalCode === null) return
    const child = spawn(this.#bin, this.#serveArgs, {
      stdio: ["pipe", "pipe", "inherit"],
      env: this.#env === undefined ? process.env : { ...process.env, ...this.#env },
      detached: process.platform !== "win32",
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
      this.#failAll(new Error(`agent-snap exited unexpectedly (code=${code}, signal=${signal ?? "none"})`), child)
      if (this.#child === child) this.#child = null
      rl.close()
    })
  }

  async #teardown(child: ChildProcessByStdio<Writable, Readable, null>, message: string): Promise<void> {
    if (this.#child === child) this.#child = null
    this.#failAll(new Error(message), child)
    await this.#killAndWait(child)
  }

  #killAndWait(child: ChildProcessByStdio<Writable, Readable, null>): Promise<void> {
    return new Promise((resolve) => {
      let settled = false
      const killTimer = setTimeout(() => this.#signalGroup(child, "SIGKILL"), KILL_GRACE_MS)
      const hardTimer = setTimeout(done, KILL_GRACE_MS + 5_000)
      function done() {
        if (settled) return
        settled = true
        clearTimeout(killTimer)
        clearTimeout(hardTimer)
        resolve()
      }
      child.once("exit", done)
      if (child.exitCode !== null || child.signalCode !== null) {
        done()
        return
      }
      this.#signalGroup(child, "SIGTERM")
    })
  }

  #signalGroup(child: ChildProcessByStdio<Writable, Readable, null>, signal: NodeJS.Signals): void {
    if (process.platform === "win32") {
      if (child.pid !== undefined) {
        execFile("taskkill", ["/pid", String(child.pid), "/T", "/F"], { windowsHide: true }, () => {
          // fallback if taskkill is unavailable
          try {
            child.kill(signal)
          } catch {
            // already gone
          }
        })
      }
      return
    }
    try {
      if (child.pid !== undefined) {
        process.kill(-child.pid, signal)
      } else {
        child.kill(signal)
      }
    } catch {
      // process already gone
    }
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

  #failAll(error: Error, child: ChildProcessByStdio<Writable, Readable, null> | null): void {
    for (const [id, pending] of this.#pending) {
      if (child !== null && pending.child !== child) continue
      clearTimeout(pending.timer)
      pending.reject(error)
      this.#pending.delete(id)
    }
  }
}
