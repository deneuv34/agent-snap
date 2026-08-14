# @agent-snap/opencode

OpenCode plugin for agent-snap. See the repository README for full details.

- Baseline snapshot at `session.created` (promise tracked; failures are
  logged, never unhandled).
- Coalesced snapshot at `session.idle` (1.5 s burst window); `session.status`
  → `busy` cancels a pending idle snapshot and aborts one already in flight.
- `session.deleted` aborts in-flight work and forgets the session; idle for
  an unknown session is ignored (never falls back to the plugin directory).
- Events are rejected once the plugin starts closing; `dispose` is a shutdown
  barrier: flushes pending snapshots, aborts leftover work, and closes the Go
  subprocess.
- Tools: `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`
  — both honour the caller's abort signal.

Enable in `opencode.json`:

```json
{ "plugin": ["@agent-snap/opencode"] }
```

Set `AGENT_SNAP_BIN` if the agent-snap Go binary is not on `PATH`.
