import { test } from "node:test"
import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"

import * as entry from "../src/index.js"

test("package entry is one default export both OpenCode 1.x and 2.x accept", () => {
  assert.deepEqual(Object.keys(entry), ["default"], "1.x would load any other export as a legacy plugin")
  const plugin = entry.default as Record<string, unknown>
  assert.equal(plugin.id, "@agent-snap/opencode")
  assert.equal(typeof plugin.server, "function", "1.x module plugins need server()")
  assert.equal(typeof plugin.setup, "function", "2.x plugins need setup()")
  assert.ok(!("tui" in plugin), "1.x rejects a module with both server() and tui()")
})

// OpenCode 2.x refuses to load `effect` from a plugin's node_modules, and
// @opencode-ai/plugin depends on it. The child blocks the 1.x SDK the same way.
test(
  "loading the entry and its 2.x adapter never loads the OpenCode 1.x SDK",
  { skip: process.versions.bun === undefined ? "the blocking loader hook is Bun's runtime plugin API" : false },
  () => {
    const run = (specifier: string) =>
      execFileSync(
        process.execPath,
        [
          "-e",
          `Bun.plugin({ name: "deny-v1-sdk", setup(build) { build.onLoad({ filter: /[\\\\/]@opencode-ai[\\\\/]plugin[\\\\/]/ }, () => { throw new Error("loaded @opencode-ai/plugin") }) } })
const mod = await import(${JSON.stringify(new URL(specifier, import.meta.url).href)})
console.log(Object.keys(mod).sort().join(","))`,
        ],
        { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] },
      ).trim()

    assert.equal(run("../src/index.ts"), "default")
    assert.equal(run("../src/v2.ts"), "setup,setupAgentSnap")
    assert.throws(() => run("../src/v1.ts"), /loaded @opencode-ai\/plugin/, "the block must be effective for the checks above to mean anything")
  },
)
