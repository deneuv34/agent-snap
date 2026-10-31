# @agent-snap/opencode

OpenCode plugin for agent-snap, for OpenCode 1.x (>= 1.18.18) and 2.x
(>= 2.2.5). See the repository README for full details.

- Baseline snapshot at `session.created` (promise tracked; failures are
  logged, never unhandled).
- Coalesced snapshot at `session.idle` (1.5 s burst window); `session.status`
  → `busy` cancels a pending idle snapshot and aborts one already in flight.
- `session.deleted` aborts in-flight work and forgets the session; idle for
  an unknown session is ignored (never falls back to the plugin directory).
- Events are rejected once the plugin starts closing; `dispose` is a shutdown
  barrier: flushes pending snapshots, aborts leftover work, and closes the Go
  subprocess.
- Tools: `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`,
  `agent_snap_get(id, cwd?)`,
  `agent_snap_diff(from, to, offset?, max_patch_bytes?, cwd?)` — all honour
  the caller's abort signal. `agent_snap_diff` returns one page of Git
  evidence and names the `offset` of the next page.
- Every snapshot records host `opencode` and source `session-start`,
  `session-idle`, or `tool`. No session identifier is stored.

Enable in `opencode.json(c)`:

```jsonc
// OpenCode 2.x
{ "plugins": ["@agent-snap/opencode"] }

// OpenCode 1.x
{ "plugin": ["@agent-snap/opencode"] }
```

The entry's default export carries `server` for 1.x and `setup` for 2.x;
each host imports only its own adapter. On 2.x, tools that omit `cwd` use the
calling session's directory.

Set `AGENT_SNAP_BIN` if the agent-snap Go binary is not on `PATH`.
