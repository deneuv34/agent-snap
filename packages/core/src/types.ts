export interface SnapOrigin {
  source?: string
  host?: string
}

export interface SnapSnapshot {
  id: string
  commit: string
  reason: string
  created_at: string
  source?: string
  host?: string
  changed_files?: string[]
  reused?: boolean
}

export interface SnapSnapshotDetail {
  id: string
  commit: string
  reason: string
  created_at: string
  source?: string
  host?: string
  tree: string
  parent?: string
  parent_id?: string
  changed_files: string[]
  changed_files_truncated?: boolean
}

export type SnapDiffStatus = "added" | "deleted" | "modified" | "renamed" | "type_changed"

export type SnapDiffPatch = "full" | "truncated" | "deferred" | "binary"

export interface SnapDiffSide {
  id: string
  commit: string
}

export interface SnapDiffHunk {
  old_start: number
  old_lines: number
  new_start: number
  new_lines: number
  section?: string
  text: string
}

export interface SnapDiffFile {
  status: SnapDiffStatus
  path: string
  old_path?: string
  similarity?: number
  old_mode?: string
  new_mode?: string
  old_oid?: string
  new_oid?: string
  additions: number
  deletions: number
  patch: SnapDiffPatch
  patch_bytes?: number
  hunks: SnapDiffHunk[]
  lossy?: true
}

export interface SnapDiff {
  from: SnapDiffSide
  to: SnapDiffSide
  offset: number
  next_offset?: number
  totals: { files: number; additions: number; deletions: number }
  files: SnapDiffFile[]
}

export interface SnapDiffPage {
  offset?: number
  maxPatchBytes?: number
}

export type SnapRestoreAction = "create" | "modify" | "type_change" | "delete"

export type SnapRestoreRepoBlocker = "operation_in_progress" | "unmerged_index" | "sparse_checkout"

export type SnapRestorePathBlocker =
  | "unaddressable_path"
  | "unsafe_path"
  | "submodule"
  | "parent_conflict"
  | "index_flag"
  | "name_collision"
  | "untracked_in_the_way"

export interface SnapRestoreSide {
  mode: string
  oid: string
}

export interface SnapRestoreEntry {
  path: string
  action: SnapRestoreAction
  current?: SnapRestoreSide
  target?: SnapRestoreSide
  staged?: true
  unstaged?: true
  untracked?: true
  blocker?: SnapRestorePathBlocker
}

export interface SnapRestoreTotals {
  entries: number
  create: number
  modify: number
  type_change: number
  delete: number
  staged: number
  unstaged: number
  untracked: number
  blocked: number
}

export interface SnapRestorePlan {
  snapshot: { id: string; commit: string; tree: string }
  paths?: string[]
  current_tree: string
  result_tree?: string
  applicable: boolean
  blockers: SnapRestoreRepoBlocker[]
  unmatched: string[]
  offset: number
  next_offset?: number
  totals: SnapRestoreTotals
  entries: SnapRestoreEntry[]
}

export interface SnapRestorePlanOptions {
  paths?: string[]
  offset?: number
  currentTree?: string
}

export type SnapRetentionLatest = "commit" | "not_commit" | "missing_object" | "symbolic" | "broken" | "missing"

export interface SnapRetentionStamp {
  id: string
  created_at: string
}

export interface SnapRetentionNamespace {
  namespace: string
  current: boolean
  worktree?: string
  refs: number
  snapshots: number
  broken: number
  oldest?: SnapRetentionStamp
  newest?: SnapRetentionStamp
  latest: SnapRetentionLatest
  digest: string
}

export interface SnapRetentionTotals {
  namespaces: number
  refs: number
  other: number
}

export interface SnapRetentionReport {
  offset: number
  next_offset?: number
  totals: SnapRetentionTotals
  namespaces: SnapRetentionNamespace[]
}

export interface SnapListResult {
  snapshots: SnapSnapshot[]
}

export interface PingResult {
  status: string
  version: string
}

export interface SnapLike {
  create(cwd: string, reason?: string, signal?: AbortSignal, origin?: SnapOrigin): Promise<SnapSnapshot>
  list(cwd: string, limit?: number, signal?: AbortSignal): Promise<SnapSnapshot[]>
  get(cwd: string, id: string, signal?: AbortSignal): Promise<SnapSnapshotDetail>
  diff(cwd: string, from: string, to: string, page?: SnapDiffPage, signal?: AbortSignal): Promise<SnapDiff>
  restorePlan(cwd: string, id: string, options?: SnapRestorePlanOptions, signal?: AbortSignal): Promise<SnapRestorePlan>
  retentionReport(cwd: string, offset?: number, signal?: AbortSignal): Promise<SnapRetentionReport>
  close(): Promise<void>
}
