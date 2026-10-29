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
  close(): Promise<void>
}
