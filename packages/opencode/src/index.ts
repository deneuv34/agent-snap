import type { Plugin } from "@opencode-ai/plugin"
import { SnapClient } from "@agent-snap/core"
import { createAgentSnapPlugin } from "./factory.js"

export const AgentSnapPlugin: Plugin = async ({ directory }) => {
  const client = SnapClient.fromEnv()
  const log = (message: string) => console.log(`[agent-snap] ${message}`)
  return createAgentSnapPlugin({ client, directory, log })
}
