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
  close(): Promise<void>
}
