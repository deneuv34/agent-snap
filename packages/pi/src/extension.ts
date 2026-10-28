import { Type } from "@earendil-works/pi-ai"
import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { createCoalescedScheduler, SnapClient, type SnapLike, type SnapSnapshot, type SnapSnapshotDetail } from "@agent-snap/core"

const IDLE_COALESCE_MS = 1500

function formatCreated(snap: SnapSnapshot): string {
  const reused = snap.reused === true ? " (working tree unchanged — reused previous snapshot)" : ""
  const changed = snap.changed_files && snap.changed_files.length > 0 ? `\nchanged files:\n${snap.changed_files.map((f) => `  ${f}`).join("\n")}` : ""
  return `Snapshot ${snap.id} created${reused}\ncommit: ${snap.commit}\nreason: ${snap.reason}${changed}`
}

function formatList(items: SnapSnapshot[]): string {
  if (items.length === 0) return "No snapshots yet"
  return items
    .map((item, index) => `${index + 1}. ${item.id} — ${item.reason} — ${item.created_at}${item.reused === true ? " (reused)" : ""}`)
    .join("\n")
}

function formatDetail(snap: SnapSnapshotDetail): string {
  const parent = snap.parent === undefined ? "none" : `${snap.parent} (${snap.parent_id === undefined ? "commit" : `snapshot ${snap.parent_id}`})`
  const heading = snap.changed_files_truncated === true ? `changed files (first ${snap.changed_files.length})` : "changed files"
  const changed = snap.changed_files.length > 0 ? `${heading}:\n${snap.changed_files.map((f) => `  ${f}`).join("\n")}` : `${heading}: none`
  return `Snapshot ${snap.id}\ncommit: ${snap.commit}\ntree: ${snap.tree}\nreason: ${snap.reason}\ncreated: ${snap.created_at}\nparent: ${parent}\n${changed}`
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
      const snap = await client.create(cwd, "session-start")
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
      const snap = await client.create(cwd, params.reason ?? "manual")
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
          const snap = await client.create(ctx.cwd, "agent-settled")
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

    pi.registerCommand("agent-snap-create", {
      description: "Create an agent-snap snapshot of the working tree (optional reason as argument)",
      handler: async (args, ctx) => {
        const reason = args.trim() || "manual"
        const snap = await client.create(ctx.cwd, reason)
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
