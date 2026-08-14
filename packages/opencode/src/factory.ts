import { tool } from "@opencode-ai/plugin"
import type { Hooks } from "@opencode-ai/plugin"
import { createCoalescedScheduler, type SnapLike, type SnapSnapshot } from "@agent-snap/core"

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

export interface AgentSnapDeps {
  client: SnapLike
  directory: string
  coalesceDelayMs?: number
  log?: (message: string) => void
}

interface SessionState {
  dir: string
  baseline: { controller: AbortController } | null
  idle: { controller: AbortController } | null
}

export function createAgentSnapPlugin(deps: AgentSnapDeps): Hooks {
  const { client } = deps
  const log = deps.log ?? (() => {})
  const sessions = new Map<string, SessionState>()
  const baselines = new Map<string, Promise<void>>()
  const idle = createCoalescedScheduler(deps.coalesceDelayMs ?? 1500)
  let closing = false

  const baseline = async (dir: string, signal?: AbortSignal): Promise<void> => {
    try {
      const snap = await client.create(dir, "session-start", signal)
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
      const snap = await client.create(dir, "session-idle", signal)
      log(`idle snapshot ${snap.id} for ${dir}`)
    } catch (err) {
      if (signal.aborted) {
        log(`idle snapshot cancelled for session ${sessionID}`)
        return
      }
      log(`idle snapshot failed: ${err instanceof Error ? err.message : String(err)}`)
    }
  }

  return {
    event: async ({ event }) => {
      if (closing) return
      if (event.type === "session.created") {
        const dir = event.properties.info.directory || deps.directory
        const controller = new AbortController()
        sessions.set(event.properties.info.id, { dir, baseline: { controller }, idle: null })
        trackBaseline(event.properties.info.id, baseline(dir, controller.signal))
      } else if (event.type === "session.idle") {
        const state = sessions.get(event.properties.sessionID)
        if (state === undefined) {
          log(`ignoring session.idle for unknown session ${event.properties.sessionID}`)
          return
        }
        const controller = new AbortController()
        state.idle = { controller }
        idle.trigger(event.properties.sessionID, () =>
          takeIdleSnapshot(event.properties.sessionID, state.dir, controller.signal),
        )
      } else if (event.type === "session.status") {
        if (event.properties.status.type !== "busy") return
        const state = sessions.get(event.properties.sessionID)
        if (state === undefined) return
        idle.cancel(event.properties.sessionID)
        state.idle?.controller.abort()
      } else if (event.type === "session.deleted") {
        const state = sessions.get(event.properties.info.id)
        if (state === undefined) return
        idle.cancel(event.properties.info.id)
        state.baseline?.controller.abort()
        state.idle?.controller.abort()
        sessions.delete(event.properties.info.id)
      }
    },
    tool: {
      agent_snap_create: tool({
        description:
          "Create a snapshot of the current git working tree, stored in hidden refs (refs/agent-snapshots/...). HEAD and the index are never touched. Unchanged trees reuse the previous snapshot.",
        args: {
          reason: tool.schema.string().describe("Why the snapshot is being taken").optional(),
          cwd: tool.schema.string().describe("Repository directory; defaults to the session directory").optional(),
        },
        async execute(args, ctx) {
          if (closing) throw new Error("agent-snap plugin is closing")
          const cwd = args.cwd ?? ctx.directory
          const snap = await client.create(cwd, args.reason ?? "manual", ctx.abort)
          return formatCreated(snap)
        },
      }),
      agent_snap_list: tool({
        description: "List agent-snap snapshots for a repository, newest first.",
        args: {
          cwd: tool.schema.string().describe("Repository directory; defaults to the session directory").optional(),
          limit: tool.schema.number().int().positive().describe("Maximum number of snapshots to return").optional(),
        },
        async execute(args, ctx) {
          if (closing) throw new Error("agent-snap plugin is closing")
          const items = await client.list(args.cwd ?? ctx.directory, args.limit ?? 20, ctx.abort)
          return formatList(items)
        },
      }),
    },
    dispose: async () => {
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
