import { test } from "node:test"
import assert from "node:assert/strict"

import * as entry from "../src/index.js"

test("package entry exposes exactly one loadable plugin function", () => {
  const functions = Object.entries(entry)
    .filter(([, value]) => typeof value === "function")
    .map(([key]) => key)
  assert.deepEqual(functions, ["AgentSnapPlugin"], "the loader would invoke every function export as a plugin")
  assert.equal(typeof entry.AgentSnapPlugin, "function")
  assert.ok(!("default" in entry), "unexpected default export")
  assert.ok(!("createAgentSnapPlugin" in entry), "test factory must not leak into the entry module")
})
