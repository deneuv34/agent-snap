# @agent-snap/pi

agent-snap extension for **Pi Coding Agent** (pi.dev,
`@earendil-works/pi-coding-agent` >= 0.84, Node >= 22.19).

The package declares `pi.extensions: ["dist/extension.js"]` in its
`package.json`, which Pi's extension discovery reads. The extension:

- takes a baseline snapshot at `session_start` (awaited as a barrier — the
  session does not proceed until the baseline completes),
- takes a coalesced (1.5 s burst window) snapshot at `agent_settled`,
- flushes pending snapshots and shuts the Go subprocess down at
  `session_shutdown`,
- registers `agent_snap_create(reason?, cwd?)`, `agent_snap_list(cwd?, limit?)`,
  `agent_snap_get(id, cwd?)`, and
  `agent_snap_diff(from, to, offset?, max_patch_bytes?, cwd?)` tools,
- registers `agent-snap-create`, `agent-snap-list`, `agent-snap-get <id>`, and
  `agent-snap-diff <from> <to>` commands,
- records host `pi` and source `session-start`, `agent-settled`, `tool`, or
  `command` on each snapshot. No session identifier is stored.

## Install

Use Pi's package installer:

```sh
pi install npm:@agent-snap/pi
```

Pi installs the package (including its `@agent-snap/core` dependency) and
discovers it via the `pi.extensions` manifest.

### Direct placement (verified topology)

Pi's discovery scans extension directories one level deep and does **not**
recurse into wrapper `node_modules`. If you place files manually, the
`package.json` manifest must live at the top of the extension directory:

```sh
mkdir -p .pi/extensions/agent-snap
cd .pi/extensions/agent-snap
npm init -y >/dev/null
npm install @agent-snap/core          # runtime dependency of dist/extension.js
npm pack @agent-snap/pi --pack-destination . >/dev/null
tar -xzf agent-snap-pi-*.tgz
mv package/package.json package/dist ./
rmdir package && rm agent-snap-pi-*.tgz
```

The resulting layout — `package.json` with `pi.extensions`, `dist/`, and
`node_modules/@agent-snap/core` — is what Pi's loader consumes and is
validated by the repository's pack-check.

Set `AGENT_SNAP_BIN` if the agent-snap Go binary is not on `PATH`.
