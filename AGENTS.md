# agent-snap

Product name: **Blink**. Implementation name: **agent-snap** until a published migration policy says otherwise.

Blink records zero-impact Git working-tree checkpoints for AI agent sessions. Spec and phased roadmap:

- `docs/BLINK_SPEC.md`
- `docs/BLINK_DEVELOPMENT_PLAN.md`

Identity is frozen: executable `agent-snap`, packages `@agent-snap/*`, JSONL methods as shipped, refs under `refs/agent-snapshots/`. Do not rename, dual-write, or migrate those identifiers.

## Safety invariants

These are non-negotiable. Capture, list, and inspect must never modify `HEAD`, the real index, normal refs, or the working tree.

- Snapshot creation uses a private scratch `GIT_INDEX_FILE` outside the repository. Fail closed if that cannot be guaranteed.
- Every Git invocation strips inherited `GIT_*` repository-redirect variables and runs against the resolved repo. The one exception is `Store.Diff`: it runs Git in a private bare evidence repository outside the worktree whose only link to the resolved repo is a read-only `objects/info/alternates` entry, so attributes, configuration, and replace refs cannot shape the evidence.
- `Store` owns every hidden-ref path. Callers never construct `refs/agent-snapshots/...` themselves.
- Host plugins and extensions never invoke Git and never spawn `agent-snap` except through `SnapClient`.
- Any operation that would write working-tree files needs a dedicated design review, adversarial tests, and explicit user confirmation UX that lifecycle hooks cannot manufacture.
- Timeout, cancellation, teardown, and concurrency fail closed. A timed-out create must not produce an unobserved checkpoint.
- Public protocol results are runtime-validated at the TypeScript boundary before they reach a host.

## Architecture

```text
OpenCode plugin ─┐
                 ├─ packages/core SnapClient ─ JSONL stdin/stdout ─ cmd/agent-snap
Pi extension ────┘         │                         │
                           │                         ├─ internal/protocol
                           │                         └─ internal/snapshot ─ Git plumbing
                           └─ coalesce + process teardown
```

| Layer | Owns |
| --- | --- |
| `cmd/agent-snap` | CLI parsing and process entry. No Git policy. |
| `internal/protocol` | Strict JSONL parse, unknown-key rejection, dispatch, error codes. |
| `internal/snapshot` | Repo discovery, scratch index, hidden refs, dedup, list/get, diff evidence repository. |
| `packages/core` | Typed client, result validation, cancellation, process-tree teardown. |
| `packages/opencode`, `packages/pi` | Host lifecycle and tools. No Git, no protocol parsing. |

New capability order: spec (`docs/BLINK_SPEC.md`) → `Store` → protocol → `types` / `SnapClient` validation → CLI → host tools last.

## How to plan a change

Before writing code, name all of:

1. Affected phase in `docs/BLINK_DEVELOPMENT_PLAN.md`
2. Safety impact (read-only vs working-tree write)
3. Compatibility impact (CLI, JSONL, npm, refs)
4. API contract (request, result, errors, limits)
5. Test strategy
6. Release / rollback plan

Prioritize: safety and data-loss risk → reliability and cross-platform correctness → evidence and review quality → explicit restore → branding and packaging.

YAGNI: do not implement restore, retention/GC, or Blink renaming until that phase. Do not add a service layer, plugin framework, or generic repository for a single function.

DRY: put shared rules in the owning layer (`Store` for refs, `protocol` for validation, `SnapClient` for transport). Do not extract host formatters until a third consumer exists.

One concern per change set. Do not mix product renaming, restore writes, and process-safety work.

## Patterns that match this repo

Use a pattern only when it already fits the architecture:

- **Facade** — `snapshot.Store` is the only Git boundary.
- **Strict dispatcher** — `protocol.dispatch` is a closed method switch with exact error codes.
- **Port** — `SnapLike` exists so hosts can be tested without a subprocess.
- **Scheduler** — `createCoalescedScheduler` is the idle/settled burst policy.
- **Strategy** — POSIX process-group teardown vs Windows process-tree teardown, isolated in `packages/core/src/teardown.ts`.

Do not introduce a DI container, event bus, generic repository, ORM, or host-side Git wrapper.

## Code standards

Write structured, small, total functions. Prefer explicit data and control flow over cleverness. Names describe the domain (`Create`, `List`, `commitRefs`), not the mechanism (`handleData`, `processManager`).

Comments are reserved for non-obvious safety constraints (scratch-index placement, guardian teardown, CAS retries). Do not narrate what the next line does.

### Go

- Standard library only. A new module dependency needs an explicit, documented reason.
- Wrap errors with `fmt.Errorf("…: %w", err)` and keep the operation in the message.
- Reject unknown protocol keys, missing `cwd`, blank strings, and out-of-range limits. Do not coerce types.
- Tests live in the same package. Use real temporary Git repos for store tests; table-driven cases for protocol/CLI validation.
- Run `gofmt` and `go vet`. Match existing error-string and helper style (`git`, `testRepo`, `serveRequest`).

### TypeScript

- ESM. Node's built-in test runner. No extra test or assertion framework.
- Runtime-validate every RPC success and error payload. Never trust subprocess JSON.
- Honor `AbortSignal` and the default timeout. Teardown must be started on cancel or timeout.
- Keep `SnapLike` as the host dependency. Do not reach into `SnapClient` internals from plugins.
- Match existing formatting, imports, and file layout. Do not add comments that duplicate types or names.

### Performance

- Reuse one `SnapClient` transport while it is healthy. Do not spawn a core process per tool call.
- Coalesce bursty idle/settled events; do not snapshot on every host event.
- Deduplicate identical trees in `Store.Create`. Do not mint a commit when `reused` should be true.
- Bound protocol input (4 MiB line) and list limits (1–100). Diff pages are bounded by FR-14 (200 files, `max_patch_bytes` of hunks). Any new payload must define size and truncation before implementation.
- Scratch indexes are created outside the repo and removed on success and failure.

## Testing and verification

Required local gates:

```sh
test -z "$(gofmt -l .)"
go test ./...
go vet ./...
npm run build
npm run typecheck
npm test
go build -o bin/agent-snap ./cmd/agent-snap
AGENT_SNAP_BIN="$PWD/bin/agent-snap" npm run smoke
AGENT_SNAP_BIN="$PWD/bin/agent-snap" npm run pack-check
```

Cover empty history, invalid IDs, other-worktree refs, SHA-1 and SHA-256 repos, ignored/untracked/staged/unstaged files, symbolic-ref refusal, concurrent creates, cancellation, and malformed protocol responses when those paths exist.

Working-tree writes, if ever added, also need adversarial tests for dirty trees, deletions, renames, nested worktrees, cancellation, and failure between backup and write.

## Do not

- Rename protocol methods, packages, the executable, or ref namespaces without a published migration policy and compatibility tests.
- Expose raw Git commands through host plugins.
- Let lifecycle hooks create restore confirmations.
- Present an inference as a confirmed fact. Review findings must cite a path and hunk from a snapshot diff, in the FR-14 citation form.
- Commit secrets, or log repository file contents in diagnostics.
- Add dependencies, abstractions, or files that are not required by the current phase.

## graphify

This project has a knowledge graph at graphify-out/ with god nodes, community structure, and cross-file relationships.

When the user types `/graphify`, use the installed graphify skill or instructions before doing anything else.

Rules:

- For codebase questions, first run `graphify query "<question>"` when graphify-out/graph.json exists. Use `graphify path "<A>" "<B>"` for relationships and `graphify explain "<concept>"` for focused concepts. These return a scoped subgraph, usually much smaller than GRAPH_REPORT.md or raw grep output.
- Dirty graphify-out/ files are expected after hooks or incremental updates; dirty graph files are not a reason to skip graphify. Only skip graphify if the task is about stale or incorrect graph output, or the user explicitly says not to use it.
- If graphify-out/wiki/index.md exists, use it for broad navigation instead of raw source browsing.
- Read graphify-out/GRAPH_REPORT.md only for broad architecture review when query/path/explain do not surface enough context.
- After modifying code, run `graphify update .` to keep the graph current (AST-only, no API cost).
