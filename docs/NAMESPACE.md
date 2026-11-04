# Namespace decision

**Status:** Decided for Phase 1 ([#1](https://github.com/deneuv34/agent-snap/issues/1)).

Blink is the product name. It appears in documentation and in npm package
descriptions. Every name that a configuration, a script, or a repository
stores keeps the value that 0.1.0 shipped. No release renames these names,
adds an alias for them, or writes a second copy under a new name.

This record is the published migration policy that
[AGENTS.md](../AGENTS.md) and [COMPATIBILITY.md](COMPATIBILITY.md) refer to.
Its policy is that nothing migrates.

## The names that stay

| Name | Value | What depends on it |
| --- | --- | --- |
| Executable | `agent-snap` | `SnapClient.defaultBin()` looks it up on `PATH`. `AGENT_SNAP_BIN` settings and scripts call it. |
| Override variable | `AGENT_SNAP_BIN` | Host environments that point at a binary outside `PATH`. |
| npm packages | `@agent-snap/core`, `@agent-snap/opencode`, `@agent-snap/pi` | OpenCode `plugin` and `plugins` entries, `pi install npm:@agent-snap/pi`, and the Pi direct placement that nests `node_modules/@agent-snap/core`. |
| JSONL methods | `ping`, `snapshot.create`, `snapshot.list`, `snapshot.get`, `snapshot.diff`, `snapshot.restore_plan` | Every `SnapClient` version. None of them carries a product name. |
| Hidden refs | `refs/agent-snapshots/<worktree>/items/<id>` and `refs/agent-snapshots/<worktree>/latest` | Every snapshot already taken. These refs are the only thing that keeps a snapshot commit reachable. |
| Snapshot commit message | `agent-snap: <reason>`, with `Agent-Snap-Source` and `Agent-Snap-Host` lines | Every 0.1.0 and later reader that parses origin (FR-13). |
| Host tools and Pi commands | `agent_snap_*` tools and `agent-snap-*` commands | Agent prompts, permission rules, and session transcripts that name them. |

## Refs stay under `refs/agent-snapshots/`

The ref namespace is invisible in normal use. It never appears in
`git branch`, and no user types it. A rename buys no brand value and puts
every stored snapshot at risk.

| Alternative | Why it loses now |
| --- | --- |
| Dual-write `refs/blink/` and `refs/agent-snapshots/` | A create would move two `latest` refs. Any binary that writes only one namespace, such as 0.1.0 on another tool's `PATH`, makes the two chains diverge. `list` then has no single truth, and deduplication against `latest` picks a side. |
| Move refs to `refs/blink/` | A 0.1.0 binary lists only `refs/agent-snapshots/`, so every moved snapshot disappears from its `list`. If the move deletes the old refs, a move that stops partway leaves commits that no ref names, and `git gc` can prune them. Git keeps no reflog by default for refs outside `refs/heads/`, `refs/remotes/`, and `refs/notes/`, so nothing else keeps those commits. |
| Read both namespaces, write the new one | Same divergence as dual-write as soon as an older binary writes, and it needs the retention rules that Phase 6 owns before any old ref can go. |

A later move needs all of these first:

- A revision of this record that names the new namespace and the release that starts it.
- A `list` and `get` that read both namespaces and return one chain.
- Compatibility tests that run the old and the new binary against the same repository and show that no snapshot disappears from either `list`.
- A rule that no release deletes an old ref until Phase 6 retention owns deletion.

## The executable stays `agent-snap`, with no `blink` alias

| Alternative | Why it loses now |
| --- | --- |
| Ship a `blink` alias next to `agent-snap` | `SnapClient` never looks for `blink`, so the alias helps no host. It doubles the release binaries and their checksums. The npm package `blink` (version 1.1.42 on 2026-11-04) already installs a `blink` command, and it describes itself as "a tool for building and deploying AI agents". A user with both installed would get whichever comes first on `PATH`. |
| Replace `agent-snap` with `blink` | Every `AGENT_SNAP_BIN` setting that names the old path breaks. Without `AGENT_SNAP_BIN`, every installed `@agent-snap/core` keeps looking for `agent-snap` on `PATH` and fails to start. CLI failures print `agent-snap:` on stderr, and scripts that match it break. |

A later rename needs a minor release that installs both names, a
`SnapClient.defaultBin()` that tries both, and compatibility tests that start
every published host against each name.

## Packages stay `@agent-snap/*`

On 2026-11-04 no `@agent-snap` package is on the npm registry yet. A package
rename costs the least before the first publish, so this is the cheapest
point to change it. The names stay anyway, for these reasons:

- The package names match the executable, the variable, the refs, and the commit message. Renaming only the packages leaves the product with two names in one install.
- A user who already built the packages from source has `@agent-snap/opencode` in `opencode.json` and `@agent-snap/core` in a Pi extension directory. A rename breaks both on the next update.
- AGENTS.md forbids a rename without a published migration policy and compatibility tests, and Phase 1 adds neither.

| Alternative | Why it loses now |
| --- | --- |
| Publish the same code as `@blink/*` too | With two names for one plugin, a host could load both and register every `agent_snap_*` tool twice. Each fix ships twice, in lockstep. |
| Publish `@blink/*` and deprecate `@agent-snap/*` | `npm deprecate` warns every current install. The Pi direct placement imports `@agent-snap/core` by name, so it breaks until the user rebuilds it. |

Versions follow [RELEASE.md](RELEASE.md). No `@agent-snap` package is
deprecated.

## Existing configuration keeps working

No name in an existing configuration changes, so an existing configuration
needs no edit. These are the 0.1.0 forms, and they stay valid.

OpenCode, in `opencode.json` or `opencode.jsonc`:

```jsonc
// OpenCode 2.x
{ "plugins": ["@agent-snap/opencode"] }

// OpenCode 1.x
{ "plugin": ["@agent-snap/opencode"] }
```

Pi:

```sh
pi install npm:@agent-snap/pi
```

The core binary, from a checkout:

```sh
go install ./cmd/agent-snap
agent-snap version
```

## Roll back

The release that carries this record changes documentation and package
descriptions only. It changes no stored snapshot, ref, or configuration, so a
rollback has nothing to convert. To go back to an earlier release, install
that release's version of each part. The examples use `0.1.0`.

Pi:

```sh
pi install npm:@agent-snap/pi@0.1.0
```

Pi direct placement, from the project root. The extension directory holds no
snapshot data, so you can delete it.

```sh
rm -rf .pi/extensions/agent-snap
mkdir -p .pi/extensions/agent-snap
cd .pi/extensions/agent-snap
npm init -y >/dev/null
npm install @agent-snap/core@0.1.0
npm pack @agent-snap/pi@0.1.0 --pack-destination . >/dev/null
tar -xzf agent-snap-pi-*.tgz
mv package/package.json package/dist ./
rmdir package && rm agent-snap-pi-*.tgz
```

OpenCode resolves `@agent-snap/opencode` itself, and its configuration
entry stays the same. Install the earlier package version where your setup
loads it, such as `.opencode/plugins/`. Keep it at the version of the binary,
because [RELEASE.md](RELEASE.md) requires the two to match.

The core binary, built from the commit of that release. A short commit hash
works.

```sh
go install github.com/deneuv34/agent-snap/cmd/agent-snap@<commit>
agent-snap version
```

Snapshots taken by either version stay listed by both, because both read and
write the same `refs/agent-snapshots/` refs.

## The docs name only shipped identifiers

`bun run pack-check` reads every fenced code block in `README.md`, in each
`packages/*/README.md`, and in this file. It fails when a block names an
`@agent-snap/` package that it did not pack, an `agent-snap` subcommand that
the built binary does not list in its usage text, a `.tgz` name that no packed
tarball matches, a `./cmd/` directory that does not exist, or `blink` in any
form.
