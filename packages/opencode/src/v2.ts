import type { Plugin } from "@opencode/plugin"
import { SnapClient } from "@agent-snap/core"
import { createAgentSnap, DESCRIPTIONS, DIFF_BOUNDS, type AgentSnap, type AgentSnapDeps } from "./snapshots.js"

/** The parts of the OpenCode 2.x plugin context the adapter uses. */
export interface AgentSnapHost {
  readonly event: Pick<Plugin.Context["event"], "subscribe">
  readonly tool: Pick<Plugin.Context["tool"], "transform">
  readonly session: Pick<Plugin.Context["session"], "get">
}

type ToolEditor = Parameters<Parameters<Plugin.Context["tool"]["transform"]>[0]>[0]
type ToolContext = Parameters<Parameters<ToolEditor["add"]>[0]["execute"]>[1]
type Input = Readonly<Record<string, unknown>>

const SESSION_EVENTS = ["session.created", "session.idle", "session.status", "session.deleted"] as const

function inputObject(input: unknown): Input {
  if (typeof input === "object" && input !== null && !Array.isArray(input)) return input as Input
  throw new TypeError("tool input must be an object")
}

function optionalString(input: Input, key: string): string | undefined {
  const value = input[key]
  if (value === undefined || typeof value === "string") return value
  throw new TypeError(`${key} must be a string`)
}

function requiredString(input: Input, key: string): string {
  const value = optionalString(input, key)
  if (value === undefined) throw new TypeError(`${key} is required`)
  return value
}

function optionalInteger(input: Input, key: string): number | undefined {
  const value = input[key]
  if (value === undefined) return undefined
  if (typeof value === "number" && Number.isInteger(value)) return value
  throw new TypeError(`${key} must be an integer`)
}

function objectSchema(properties: Record<string, object>, required: string[] = []) {
  return { type: "object", properties, required, additionalProperties: false }
}

const cwdSchema = { type: "string", description: DESCRIPTIONS.cwd }

function registerTools(editor: ToolEditor, snap: AgentSnap, cwdOf: (cwd: string | undefined, context: ToolContext) => Promise<string>) {
  editor.add({
    name: "agent_snap_create",
    description: DESCRIPTIONS.create,
    input: objectSchema({ reason: { type: "string", description: DESCRIPTIONS.reason }, cwd: cwdSchema }),
    async execute(raw, context) {
      const input = inputObject(raw)
      const reason = optionalString(input, "reason")
      const cwd = optionalString(input, "cwd")
      return { content: await snap.create(await cwdOf(cwd, context), reason, context.signal) }
    },
  })
  editor.add({
    name: "agent_snap_list",
    description: DESCRIPTIONS.list,
    input: objectSchema({ cwd: cwdSchema, limit: { type: "integer", minimum: 1, maximum: 100, description: DESCRIPTIONS.limit } }),
    async execute(raw, context) {
      const input = inputObject(raw)
      const limit = optionalInteger(input, "limit")
      const cwd = optionalString(input, "cwd")
      return { content: await snap.list(await cwdOf(cwd, context), limit, context.signal) }
    },
  })
  editor.add({
    name: "agent_snap_get",
    description: DESCRIPTIONS.get,
    input: objectSchema({ id: { type: "string", description: DESCRIPTIONS.id }, cwd: cwdSchema }, ["id"]),
    async execute(raw, context) {
      const input = inputObject(raw)
      const id = requiredString(input, "id")
      const cwd = optionalString(input, "cwd")
      return { content: await snap.get(await cwdOf(cwd, context), id, context.signal) }
    },
  })
  editor.add({
    name: "agent_snap_diff",
    description: DESCRIPTIONS.diff,
    input: objectSchema(
      {
        from: { type: "string", description: DESCRIPTIONS.from },
        to: { type: "string", description: DESCRIPTIONS.to },
        offset: { type: "integer", minimum: DIFF_BOUNDS.offset.min, maximum: DIFF_BOUNDS.offset.max, description: DESCRIPTIONS.offset },
        max_patch_bytes: {
          type: "integer",
          minimum: DIFF_BOUNDS.maxPatchBytes.min,
          maximum: DIFF_BOUNDS.maxPatchBytes.max,
          description: DESCRIPTIONS.maxPatchBytes,
        },
        cwd: cwdSchema,
      },
      ["from", "to"],
    ),
    async execute(raw, context) {
      const input = inputObject(raw)
      const args = {
        from: requiredString(input, "from"),
        to: requiredString(input, "to"),
        offset: optionalInteger(input, "offset"),
        max_patch_bytes: optionalInteger(input, "max_patch_bytes"),
      }
      const cwd = optionalString(input, "cwd")
      return { content: await snap.diff(await cwdOf(cwd, context), args, context.signal) }
    },
  })
}

export async function setupAgentSnap(host: AgentSnapHost, deps: AgentSnapDeps): Promise<Plugin.Cleanup> {
  const snap = createAgentSnap(deps)
  const log = deps.log ?? (() => {})
  const subscription = new AbortController()

  const events = (async () => {
    try {
      for await (const event of host.event.subscribe({ types: SESSION_EVENTS, signal: subscription.signal })) {
        if (event.type === "session.created") snap.sessionCreated(event.data.sessionID, event.data.location.directory)
        else if (event.type === "session.idle") snap.sessionIdle(event.data.sessionID)
        else if (event.type === "session.status") {
          if (event.data.status.type === "busy") snap.sessionBusy(event.data.sessionID)
        } else if (event.type === "session.deleted") snap.sessionDeleted(event.data.sessionID)
      }
    } catch (err) {
      if (!subscription.signal.aborted) log(`session event stream failed: ${err instanceof Error ? err.message : String(err)}`)
    }
  })()

  const cleanup = async () => {
    subscription.abort()
    await events
    await snap.dispose()
  }

  const cwdOf = async (cwd: string | undefined, context: ToolContext) =>
    cwd ?? (await host.session.get({ sessionID: context.sessionID }, { signal: context.signal })).location.directory

  try {
    await host.tool.transform((editor) => registerTools(editor, snap, cwdOf))
  } catch (err) {
    await cleanup()
    throw err
  }
  return cleanup
}

export const setup = (context: Plugin.Context): Promise<Plugin.Cleanup> =>
  setupAgentSnap(context, {
    client: SnapClient.fromEnv(),
    log: (message) => console.log(`[agent-snap] ${message}`),
  })
