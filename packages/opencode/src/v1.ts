import { tool } from "@opencode-ai/plugin"
import type { Hooks, Plugin } from "@opencode-ai/plugin"
import { SnapClient } from "@agent-snap/core"
import { createAgentSnap, DESCRIPTIONS, DIFF_BOUNDS, type AgentSnapDeps } from "./snapshots.js"

export interface AgentSnapPluginDeps extends AgentSnapDeps {
  directory: string
}

export function createAgentSnapPlugin(deps: AgentSnapPluginDeps): Hooks {
  const snap = createAgentSnap(deps)

  return {
    event: async ({ event }) => {
      if (event.type === "session.created") {
        snap.sessionCreated(event.properties.info.id, event.properties.info.directory || deps.directory)
      } else if (event.type === "session.idle") {
        snap.sessionIdle(event.properties.sessionID)
      } else if (event.type === "session.status") {
        if (event.properties.status.type === "busy") snap.sessionBusy(event.properties.sessionID)
      } else if (event.type === "session.deleted") {
        snap.sessionDeleted(event.properties.info.id)
      }
    },
    tool: {
      agent_snap_create: tool({
        description: DESCRIPTIONS.create,
        args: {
          reason: tool.schema.string().describe(DESCRIPTIONS.reason).optional(),
          cwd: tool.schema.string().describe(DESCRIPTIONS.cwd).optional(),
        },
        execute: (args, ctx) => snap.create(args.cwd ?? ctx.directory, args.reason, ctx.abort),
      }),
      agent_snap_list: tool({
        description: DESCRIPTIONS.list,
        args: {
          cwd: tool.schema.string().describe(DESCRIPTIONS.cwd).optional(),
          limit: tool.schema.number().int().positive().describe(DESCRIPTIONS.limit).optional(),
        },
        execute: (args, ctx) => snap.list(args.cwd ?? ctx.directory, args.limit, ctx.abort),
      }),
      agent_snap_get: tool({
        description: DESCRIPTIONS.get,
        args: {
          id: tool.schema.string().describe(DESCRIPTIONS.id),
          cwd: tool.schema.string().describe(DESCRIPTIONS.cwd).optional(),
        },
        execute: (args, ctx) => snap.get(args.cwd ?? ctx.directory, args.id, ctx.abort),
      }),
      agent_snap_diff: tool({
        description: DESCRIPTIONS.diff,
        args: {
          from: tool.schema.string().describe(DESCRIPTIONS.from),
          to: tool.schema.string().describe(DESCRIPTIONS.to),
          offset: tool.schema.number().int().min(DIFF_BOUNDS.offset.min).max(DIFF_BOUNDS.offset.max).describe(DESCRIPTIONS.offset).optional(),
          max_patch_bytes: tool.schema
            .number()
            .int()
            .min(DIFF_BOUNDS.maxPatchBytes.min)
            .max(DIFF_BOUNDS.maxPatchBytes.max)
            .describe(DESCRIPTIONS.maxPatchBytes)
            .optional(),
          cwd: tool.schema.string().describe(DESCRIPTIONS.cwd).optional(),
        },
        execute: (args, ctx) => snap.diff(args.cwd ?? ctx.directory, args, ctx.abort),
      }),
    },
    dispose: () => snap.dispose(),
  }
}

export const server: Plugin = async ({ directory }) =>
  createAgentSnapPlugin({
    client: SnapClient.fromEnv(),
    directory,
    log: (message) => console.log(`[agent-snap] ${message}`),
  })
