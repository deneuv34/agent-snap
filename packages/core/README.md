# @agent-snap/core

Type-safe TypeScript client for the agent-snap Go core. Spawns
`agent-snap serve` and speaks the JSONL wire protocol over stdin/stdout:

```ts
import { SnapClient } from "@agent-snap/core"

const client = SnapClient.fromEnv() // honours AGENT_SNAP_BIN
const snap = await client.create("/path/to/repo", "before-refactor")
const items = await client.list("/path/to/repo", 10)
await client.close()
```

Safety properties:

- requests support `AbortSignal`; aborting (or a timeout) tears down the
  child process group — a cancelled create can never silently produce a late
  snapshot;
- on Unix the whole process group is signalled (SIGTERM, then SIGKILL after
  a grace period); on Windows the child tree is killed via `taskkill /T /F`
  with a plain `child.kill` fallback if `taskkill` is unavailable;
- responses are runtime-validated before being handed to the host;
- the client respawns the transport lazily after any crash or close.
