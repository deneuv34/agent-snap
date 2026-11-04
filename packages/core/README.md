# @agent-snap/core

Type-safe TypeScript client for Blink. It spawns the Blink core binary as
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
  and cleanup fails closed unless taskkill succeeds *and* the root exits
  (see [docs/DIAGNOSTICS.md](../../docs/DIAGNOSTICS.md));
- `serveArgs` and `request` cannot reach the restore entry. The constructor
  copies `serveArgs` into a frozen array by index, calling no method of the
  caller's value, and throws a `TypeError` when an element starts with
  `restore` after its leading `-` characters. `request`
  rejects with a `TypeError` for a method that is not a primitive string and
  for `snapshot.restore`;
- responses are runtime-validated before being handed to the host;
- the client respawns the transport lazily after any crash or close.
