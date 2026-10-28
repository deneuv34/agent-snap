import { Type } from "@earendil-works/pi-ai"
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent"
import {
  createCoalescedScheduler,
  SnapClient,
  type SnapDiff,
  type SnapDiffFile,
  type SnapDiffPage,
  type SnapLike,
  type SnapOrigin,
  type SnapSnapshot,
  type SnapSnapshotDetail,
} from "@agent-snap/core"

const IDLE_COALESCE_MS = 1500
const HOST = "pi"

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

export interface AgentSnapExtensionDeps {
  client: SnapLike
  coalesceDelayMs?: number
  log?: (message: string) => void
}

export function createAgentSnapExtension(deps: AgentSnapExtensionDeps): (pi: ExtensionAPI) => void {
  const client = deps.client
  const log = deps.log ?? (() => {})
  const scheduler = createCoalescedScheduler(deps.coalesceDelayMs ?? IDLE_COALESCE_MS)
  const baselines = new Set<Promise<void>>()

  const trackBaseline = (promise: Promise<void>) => {
    baselines.add(promise)
    void promise.finally(() => baselines.delete(promise))
  }

  const baseline = async (cwd: string) => {
    try {
      const snap = await client.create(cwd, "session-start", undefined, { source: "session-start", host: HOST })
      log(`agent-snap: baseline snapshot ${snap.id}`)
    } catch (err) {
      log(`agent-snap: baseline snapshot failed: ${err instanceof Error ? err.message : String(err)}`)
    }
  }

  const createTool = defineTool({
    name: "agent_snap_create",
    label: "Agent Snap: Create snapshot",
    description:
      "Create a snapshot of the current git working tree, stored in hidden refs (refs/agent-snapshots/...). HEAD and the index are never touched. Unchanged trees reuse the previous snapshot.",
    promptSnippet: "agent_snap_create(reason?, cwd?) - snapshot the git working tree without touching HEAD or the index",
    parameters: Type.Object({
      reason: Type.Optional(Type.String({ description: "Why the snapshot is being taken" })),
      cwd: Type.Optional(Type.String({ description: "Repository directory; defaults to the session directory" })),
    }),
    async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
      const cwd = params.cwd ?? ctx.cwd
      const snap = await client.create(cwd, params.reason ?? "manual", undefined, { source: "tool", host: HOST })
      return {
        content: [{ type: "text", text: formatCreated(snap) }],
        details: { snapshot: snap },
      }
    },
  })

  const listTool = defineTool({
    name: "agent_snap_list",
    label: "Agent Snap: List snapshots",
    description: "List agent-snap snapshots for a repository, newest first.",
    promptSnippet: "agent_snap_list(cwd?, limit?) - list agent-snap snapshots for the git working tree",
    parameters: Type.Object({
      cwd: Type.Optional(Type.String({ description: "Repository directory; defaults to the session directory" })),
      limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "Maximum number of snapshots to return" })),
    }),
    async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
      const cwd = params.cwd ?? ctx.cwd
      const snapshots = await client.list(cwd, params.limit ?? 20)
      return {
        content: [{ type: "text", text: formatList(snapshots) }],
        details: { snapshots },
      }
    },
  })

  const getTool = defineTool({
    name: "agent_snap_get",
    label: "Agent Snap: Describe snapshot",
    description:
      "Describe one agent-snap snapshot by id: its commit, tree, parent, and the files changed since its parent. Read-only. Snapshot ids come from agent_snap_list or agent_snap_create.",
    promptSnippet: "agent_snap_get(id, cwd?) - describe one agent-snap snapshot: commit, tree, parent, changed files",
    parameters: Type.Object({
      id: Type.String({ description: "Snapshot id from agent_snap_list or agent_snap_create" }),
      cwd: Type.Optional(Type.String({ description: "Repository directory; defaults to the session directory" })),
    }),
    async execute(_toolCallId, params, signal, _onUpdate, ctx) {
      const cwd = params.cwd ?? ctx.cwd
      const snapshot = await client.get(cwd, params.id, signal)
      return {
        content: [{ type: "text", text: formatDetail(snapshot) }],
        details: { snapshot },
      }
    },
  })

  const diffTool = defineTool({
    name: "agent_snap_diff",
    label: "Agent Snap: Diff snapshots",
    description:
      "Show what changed between two agent-snap snapshots as Git hunks. Read-only. Snapshot ids come from agent_snap_list. Large diffs are paged: pass the offset the output names. Cite findings by path and hunk header.",
    promptSnippet: "agent_snap_diff(from, to, offset?, max_patch_bytes?, cwd?) - show what changed between two agent-snap snapshots as Git hunks",
    parameters: Type.Object({
      from: Type.String({ description: "Snapshot id from agent_snap_list to diff from" }),
      to: Type.String({ description: "Snapshot id from agent_snap_list to diff to" }),
      offset: Type.Optional(Type.Integer({ minimum: 0, maximum: 2147483647, description: "Index of the first file to show; use the offset the previous page named" })),
      max_patch_bytes: Type.Optional(Type.Integer({ minimum: 1024, maximum: 1048576, description: "Patch byte budget for this page, 1024 to 1048576" })),
      cwd: Type.Optional(Type.String({ description: "Repository directory; defaults to the session directory" })),
    }),
    async execute(_toolCallId, params, signal, _onUpdate, ctx) {
      const cwd = params.cwd ?? ctx.cwd
      const diff = await client.diff(cwd, params.from, params.to, diffPage(params.offset, params.max_patch_bytes), signal)
      return {
        content: [{ type: "text", text: formatDiff(diff) }],
        details: { diff },
      }
    },
  })

  return function agentSnapExtension(pi: ExtensionAPI): void {
    pi.on("session_start", async (_event, ctx) => {
      // Barrier: the session does not proceed until the baseline snapshot
      // has completed (failures are logged, never thrown). The promise is
      // also tracked so session_shutdown waits for any in-flight baseline.
      const promise = baseline(ctx.cwd)
      trackBaseline(promise)
      await promise
    })

    pi.on("agent_settled", async (_event, ctx) => {
      scheduler.trigger(ctx.cwd, async () => {
        try {
          const snap = await client.create(ctx.cwd, "agent-settled", undefined, { source: "agent-settled", host: HOST })
          log(`agent-snap: settled snapshot ${snap.id}`)
        } catch (err) {
          log(`agent-snap: settled snapshot failed: ${err instanceof Error ? err.message : String(err)}`)
        }
      })
    })

    pi.on("session_shutdown", async () => {
      await scheduler.flush()
      scheduler.dispose()
      await Promise.allSettled([...baselines])
      try {
        await client.close()
      } catch (err) {
        log(`agent-snap: shutdown cleanup failed: ${err instanceof Error ? err.message : String(err)}`)
      }
    })

    pi.registerTool(createTool)
    pi.registerTool(listTool)
    pi.registerTool(getTool)
    pi.registerTool(diffTool)

    pi.registerCommand("agent-snap-create", {
      description: "Create an agent-snap snapshot of the working tree (optional reason as argument)",
      handler: async (args, ctx) => {
        const reason = args.trim() || "manual"
        const snap = await client.create(ctx.cwd, reason, undefined, { source: "command", host: HOST })
        ctx.ui.notify(`agent-snap: snapshot ${snap.id} created${snap.reused === true ? " (reused)" : ""}`, "info")
      },
    })

    pi.registerCommand("agent-snap-list", {
      description: "List agent-snap snapshots for the working tree",
      handler: async (_args, ctx) => {
        const snapshots = await client.list(ctx.cwd, 20)
        ctx.ui.notify(formatList(snapshots).split("\n").slice(0, 5).join("\n"), "info")
      },
    })

    pi.registerCommand("agent-snap-get", {
      description: "Describe one agent-snap snapshot by id (id as argument)",
      handler: async (args, ctx) => {
        const snapshot = await client.get(ctx.cwd, args.trim())
        ctx.ui.notify(formatDetail(snapshot).split("\n").slice(0, 12).join("\n"), "info")
      },
    })

    pi.registerCommand("agent-snap-diff", {
      description: "Show what changed between two agent-snap snapshots (from and to ids as arguments)",
      handler: async (args, ctx) => {
        const [from, to, ...extra] = args.trim().split(/\s+/)
        if (from === undefined || from === "" || to === undefined || extra.length > 0) {
          ctx.ui.notify("Usage: agent-snap-diff <from> <to>", "warning")
          return
        }
        const diff = await client.diff(ctx.cwd, from, to)
        ctx.ui.notify(formatDiff(diff).split("\n").slice(0, 12).join("\n"), "info")
      },
    })
  }
}

/**
 * Default extension factory. Pi loads this as the module default export
 * (see pi.extensions in package.json). The client is resolved here, at load
 * time, so each load honours the current AGENT_SNAP_BIN environment.
 */
export default function agentSnapExtension(pi: ExtensionAPI): void {
  return createAgentSnapExtension({
    client: SnapClient.fromEnv(),
    log: (message) => console.log(message),
  })(pi)
}
