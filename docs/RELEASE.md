# Release checklist

Use this before publishing `agent-snap` **0.1.0** or any later version. Identity
and wire names stay frozen; see [COMPATIBILITY.md](COMPATIBILITY.md).

## Supported matrix

| Component | Minimum | CI / release target |
| --- | --- | --- |
| Git | 2.30 | runner default |
| Go | 1.26 | `1.26.x` |
| Bun (toolchain; OpenCode runtime) | 1.4.0 | `1.4.0` (`BUN_VERSION` in CI) |
| Node.js (Pi runtime) | 22.19.0 | `22` (latest 22.x ≥ 22.19) |
| OS (Go core + tests) | Linux, Windows | `ubuntu-latest`, `windows-latest` |
| OS (TypeScript + teardown) | Linux, Windows, macOS | `ubuntu-latest`, `windows-latest`, `macos-latest` |
| Smoke + pack-check | Linux | `ubuntu-latest` (Windows smoke also runs) |

POSIX process-group teardown is covered by Linux and macOS tests on Bun
and on Node. Windows process-tree teardown (`taskkill /T /F`) is covered by
Windows tests on both runtimes. Smoke runs on Linux and Windows against the
binary built on that OS, including from a compiled Bun host like OpenCode.
`bun run pack-check` runs on Linux as the publication check.

## Versioning

- Semver. Bump `internal/protocol.Version` and every `@agent-snap/*` `package.json`
  together.
- Record the change in [CHANGELOG.md](../CHANGELOG.md).
- Release binaries should set
  `-ldflags "-X github.com/deneuv34/agent-snap/internal/protocol.Version=x.y.z"`.
- Do not publish npm packages whose version disagrees with `agent-snap version`.

## Artifacts

### Go binary

Build one binary per target. Suggested names:

```
agent-snap_<version>_linux_amd64
agent-snap_<version>_linux_arm64
agent-snap_<version>_darwin_amd64
agent-snap_<version>_darwin_arm64
agent-snap_<version>_windows_amd64.exe
agent-snap_<version>_windows_arm64.exe
```

Example:

```sh
VERSION=0.1.0
GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X github.com/deneuv34/agent-snap/internal/protocol.Version=${VERSION}" \
  -o "dist/agent-snap_${VERSION}_linux_amd64" ./cmd/agent-snap
```

Publish SHA-256 checksums next to the binaries:

```sh
cd dist
shasum -a 256 agent-snap_* > SHA256SUMS
```

Verify with `shasum -a 256 -c SHA256SUMS`.

### npm packages

Published packages and their tarball contents (`files` in each `package.json`):

| Package | Required entries |
| --- | --- |
| `@agent-snap/core` | `dist/`, `README.md`, `LICENSE` (`dist/index.js` present) |
| `@agent-snap/opencode` | `dist/`, `README.md`, `LICENSE` (`dist/index.js` present; no stale `plugin.js`) |
| `@agent-snap/pi` | `dist/`, `README.md`, `LICENSE` (`dist/extension.js` present; no `orca-plugin.json`) |

`bun run pack-check` is the automated content and clean-install gate.

## Pre-publish checklist

- [ ] [CHANGELOG.md](../CHANGELOG.md) has an entry for this version
- [ ] `internal/protocol.Version` and all three npm versions match
- [ ] Local gates pass (same set as CI; include `gofmt`):

```sh
test -z "$(gofmt -l .)"
go test ./...
go vet ./...
bun run build
bun run typecheck
bun run test
bun run test:node
go build -o bin/agent-snap ./cmd/agent-snap
AGENT_SNAP_BIN="$PWD/bin/agent-snap" bun run smoke
AGENT_SNAP_BIN="$PWD/bin/agent-snap" bun run pack-check
```

- [ ] CI is green on the matrix above
- [ ] Binaries built with the version ldflag
- [ ] `SHA256SUMS` generated and stored with the binaries
- [ ] `bun pm pack` for each workspace inspected (or pack-check passed)
- [ ] npm publish in dependency order: `core`, then `opencode` and `pi`
- [ ] GitHub release notes point at the changelog and checksums
- [ ] No restore, rename, or new protocol method shipped in a baseline patch

## Rollback

- npm: `npm unpublish` is only possible within the npm time window. Prefer
  publishing a follow-up patch and documenting the yanked version in the
  changelog.
- GitHub release: delete or mark the release as prerelease and keep the
  previous binaries available.
- Hosts pin `@agent-snap/*@0.1.0`. Rolling back is reinstalling that version
  and the matching `agent-snap` binary.

Do not leave hosts on a binary whose protocol version does not match the
installed TypeScript packages.
