import type { Hooks, PluginInput, PluginModule } from "@opencode-ai/plugin"
import type { Plugin } from "@opencode/plugin"

// OpenCode 1.x calls `server`; 2.x calls `setup` and ignores `server`. Each
// adapter is imported only when its host calls it: the 1.x adapter loads
// @opencode-ai/plugin, whose `effect` dependency the 2.x runtime refuses to
// load from a plugin's node_modules.
const agentSnap = {
  id: "@agent-snap/opencode",
  server: async (input: PluginInput): Promise<Hooks> => (await import("./v1.js")).server(input),
  setup: async (context: Plugin.Context): Promise<Plugin.Cleanup> => (await import("./v2.js")).setup(context),
} satisfies PluginModule & Plugin.Plugin

export default agentSnap
