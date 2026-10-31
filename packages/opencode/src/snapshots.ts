import {
  createCoalescedScheduler,
  type SnapDiff,
  type SnapDiffFile,
  type SnapDiffPage,
  type SnapLike,
  type SnapOrigin,
  type SnapSnapshot,
  type SnapSnapshotDetail,
} from "@agent-snap/core"

const HOST = "opencode"

export const DESCRIPTIONS = {
  create:
    "Create a snapshot of the current git working tree, stored in hidden refs (refs/agent-snapshots/...). HEAD and the index are never touched. Unchanged trees reuse the previous snapshot.",
  list: "List agent-snap snapshots for a repository, newest first.",
  get: "Describe one agent-snap snapshot by id: its commit, tree, parent, and the files changed since its parent. Read-only. Snapshot ids come from agent_snap_list or agent_snap_create.",
  diff: "Show what changed between two agent-snap snapshots as Git hunks. Read-only. Snapshot ids come from agent_snap_list. Large diffs are paged: pass the offset the output names. Cite findings by path and hunk header.",
  reason: "Why the snapshot is being taken",
  cwd: "Repository directory; defaults to the session directory",
  limit: "Maximum number of snapshots to return",
  id: "Snapshot id from agent_snap_list or agent_snap_create",
  from: "Snapshot id from agent_snap_list to diff from",
  to: "Snapshot id from agent_snap_list to diff to",
  offset: "Index of the first file to show; use the offset the previous page named",
  maxPatchBytes: "Patch byte budget for this page, 1024 to 1048576",
} as const

export const DIFF_BOUNDS = {
  offset: { min: 0, max: 2147483647 },
  maxPatchBytes: { min: 1024, max: 1048576 },
} as const

function formatCreated(snap: SnapSnapshot): string {
  const reused = snap.reused === true ? " (working tree unchanged — reused previous snapshot)" : ""
  const changed = snap.changed_files && snap.changed_files.length > 0 ? `\nchanged files:\n${snap.changed_files.map((f) => `  ${f}`).join("\n")}` : ""
  return `Snapshot ${snap.id} created${reused}\ncommit: ${snap.commit}\nreason: ${snap.reason}${changed}`
}

function formatList(items: SnapSnapshot[]): string {
  if (items.length === 0) return "No snapshots yet"
  return items
    .map((item, index) => {
      const origin = formatOrigin(item)
      return `${index + 1}. ${item.id} — ${item.reason} — ${item.created_at}${origin === "" ? "" : ` — ${origin}`}${item.reused === true ? " (reused)" : ""}`
    })
    .join("\n")
}

function formatOrigin(snap: SnapOrigin): string {
  return [snap.source, snap.host === undefined ? undefined : `via ${snap.host}`].filter((part) => part !== undefined).join(" ")
}

function formatDetail(snap: SnapSnapshotDetail): string {
  const parent = snap.parent === undefined ? "none" : `${snap.parent} (${snap.parent_id === undefined ? "commit" : `snapshot ${snap.parent_id}`})`
  const heading = snap.changed_files_truncated === true ? `changed files (first ${snap.changed_files.length})` : "changed files"
  const changed = snap.changed_files.length > 0 ? `${heading}:\n${snap.changed_files.map((f) => `  ${f}`).join("\n")}` : `${heading}: none`
  const origin = formatOrigin(snap)
  return `Snapshot ${snap.id}\ncommit: ${snap.commit}\ntree: ${snap.tree}\nreason: ${snap.reason}\n${origin === "" ? "" : `origin: ${origin}\n`}created: ${snap.created_at}\nparent: ${parent}\n${changed}`
}

function formatDiffCounts(file: SnapDiffFile): string {
  return file.patch === "binary" ? "binary" : `+${file.additions} -${file.deletions}`
}

function formatDiffIndexLine(file: SnapDiffFile, number: number): string {
  const rename = file.status === "renamed" ? ` <- ${file.old_path} (${file.similarity}%)` : ""
  return `${number}  ${file.status}  ${file.path}${rename}  ${formatDiffCounts(file)}\n`
}

function formatDiffPatch(file: SnapDiffFile, number: number): string {
  const lossy = file.lossy === true ? " (non-UTF-8 bytes shown as U+FFFD)" : ""
  let out = `== ${number} ${file.path}${lossy}\n`
  for (const hunk of file.hunks) {
    const section = hunk.section ? ` ${hunk.section}` : ""
    out += `@@ -${hunk.old_start},${hunk.old_lines} +${hunk.new_start},${hunk.new_lines} @@${section}\n${hunk.text}`
  }
  if (file.patch === "truncated") {
    out +=
      file.patch_bytes === undefined
        ? "Patch truncated. The rest of this file is over 1048576 bytes and cannot be shown.\n"
        : `Patch truncated. Call agent_snap_diff with offset ${number - 1} and max_patch_bytes ${file.patch_bytes} to see this file whole.\n`
  }
  return out
}

function formatDiff(diff: SnapDiff): string {
  const { from, to, offset, totals, files } = diff
  let out = `Git evidence: snapshot diff (read-only)\nfrom ${from.id}\nto   ${to.id}\n`
  if (totals.files === 0) {
    out += "No files changed.\n"
  } else {
    const window = files.length === 0 ? `No files at offset ${offset}.` : `Files ${offset + 1}-${offset + files.length} of ${totals.files}.`
    out += `${totals.files} files changed, +${totals.additions} -${totals.deletions}. ${window}\n`
    if (files.length > 0) out += `\n${files.map((file, index) => formatDiffIndexLine(file, offset + index + 1)).join("")}`
    const patches = files.flatMap((file, index) => (file.patch === "full" || file.patch === "truncated" ? [formatDiffPatch(file, offset + index + 1)] : []))
    if (patches.length > 0) out += `\n${patches.join("")}`
    if (diff.next_offset !== undefined) {
      out += `\nHunks from file ${diff.next_offset + 1} on are not shown. Call agent_snap_diff with offset ${diff.next_offset}.\n`
    }
  }
  return `${out}\nCite a finding as \`<path> @@ -a,b +c,d @@\` from this output. Lines not shown here are not evidence.`
}

function diffPage(offset: number | undefined, maxPatchBytes: number | undefined): SnapDiffPage {
  const page: SnapDiffPage = {}
  if (offset !== undefined) page.offset = offset
  if (maxPatchBytes !== undefined) page.maxPatchBytes = maxPatchBytes
  return page
}

export interface AgentSnapDeps {
  client: SnapLike
  coalesceDelayMs?: number
  log?: (message: string) => void
}

export interface DiffArgs {
  from: string
  to: string
  offset?: number
  max_patch_bytes?: number
}

/** Host-neutral session lifecycle and tool operations shared by the OpenCode 1.x and 2.x adapters. */
export interface AgentSnap {
  sessionCreated(sessionID: string, dir: string): void
  sessionIdle(sessionID: string): void
  sessionBusy(sessionID: string): void
  sessionDeleted(sessionID: string): void
  create(cwd: string, reason: string | undefined, signal: AbortSignal): Promise<string>
  list(cwd: string, limit: number | undefined, signal: AbortSignal): Promise<string>
  get(cwd: string, id: string, signal: AbortSignal): Promise<string>
  diff(cwd: string, args: DiffArgs, signal: AbortSignal): Promise<string>
  dispose(): Promise<void>
}

interface SessionState {
  dir: string
  baseline: { controller: AbortController } | null
  idle: { controller: AbortController } | null
}

export function createAgentSnap(deps: AgentSnapDeps): AgentSnap {
  const { client } = deps
  const log = deps.log ?? (() => {})
  const sessions = new Map<string, SessionState>()
  const baselines = new Map<string, Promise<void>>()
  const idle = createCoalescedScheduler(deps.coalesceDelayMs ?? 1500)
  let closing = false

  const baseline = async (dir: string, signal?: AbortSignal): Promise<void> => {
    try {
      const snap = await client.create(dir, "session-start", signal, { source: "session-start", host: HOST })
      log(`baseline snapshot ${snap.id} for ${dir}`)
    } catch (err) {
      if (signal?.aborted) return
      log(`baseline snapshot failed: ${err instanceof Error ? err.message : String(err)}`)
    }
  }

  const trackBaseline = (sessionID: string, promise: Promise<void>) => {
    baselines.set(sessionID, promise)
    void promise.finally(() => {
      if (baselines.get(sessionID) === promise) baselines.delete(sessionID)
    })
  }

  const takeIdleSnapshot = async (sessionID: string, dir: string, signal: AbortSignal): Promise<void> => {
    try {
      const snap = await client.create(dir, "session-idle", signal, { source: "session-idle", host: HOST })
      log(`idle snapshot ${snap.id} for ${dir}`)
    } catch (err) {
      if (signal.aborted) {
        log(`idle snapshot cancelled for session ${sessionID}`)
        return
      }
      log(`idle snapshot failed: ${err instanceof Error ? err.message : String(err)}`)
    }
  }

  const open = () => {
    if (closing) throw new Error("agent-snap plugin is closing")
  }

  return {
    sessionCreated(sessionID, dir) {
      if (closing) return
      const controller = new AbortController()
      sessions.set(sessionID, { dir, baseline: { controller }, idle: null })
      trackBaseline(sessionID, baseline(dir, controller.signal))
    },
    sessionIdle(sessionID) {
      if (closing) return
      const state = sessions.get(sessionID)
      if (state === undefined) {
        log(`ignoring session.idle for unknown session ${sessionID}`)
        return
      }
      const controller = new AbortController()
      state.idle = { controller }
      idle.trigger(sessionID, () => takeIdleSnapshot(sessionID, state.dir, controller.signal))
    },
    sessionBusy(sessionID) {
      if (closing) return
      const state = sessions.get(sessionID)
      if (state === undefined) return
      idle.cancel(sessionID)
      state.idle?.controller.abort()
    },
    sessionDeleted(sessionID) {
      if (closing) return
      const state = sessions.get(sessionID)
      if (state === undefined) return
      idle.cancel(sessionID)
      state.baseline?.controller.abort()
      state.idle?.controller.abort()
      sessions.delete(sessionID)
    },
    async create(cwd, reason, signal) {
      open()
      return formatCreated(await client.create(cwd, reason ?? "manual", signal, { source: "tool", host: HOST }))
    },
    async list(cwd, limit, signal) {
      open()
      return formatList(await client.list(cwd, limit ?? 20, signal))
    },
    async get(cwd, id, signal) {
      open()
      return formatDetail(await client.get(cwd, id, signal))
    },
    async diff(cwd, args, signal) {
      open()
      return formatDiff(await client.diff(cwd, args.from, args.to, diffPage(args.offset, args.max_patch_bytes), signal))
    },
    async dispose() {
      closing = true
      await idle.flush()
      idle.dispose()
      for (const state of sessions.values()) {
        state.baseline?.controller.abort()
        state.idle?.controller.abort()
      }
      sessions.clear()
      await Promise.allSettled([...baselines.values()])
      await client.close()
    },
  }
}
