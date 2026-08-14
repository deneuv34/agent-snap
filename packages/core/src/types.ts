export interface SnapSnapshot {
  id: string
  commit: string
  reason: string
  created_at: string
  changed_files?: string[]
  reused?: boolean
}

export interface SnapListResult {
  snapshots: SnapSnapshot[]
}

export interface PingResult {
  status: string
  version: string
}

export interface SnapLike {
  create(cwd: string, reason?: string, signal?: AbortSignal): Promise<SnapSnapshot>
  list(cwd: string, limit?: number, signal?: AbortSignal): Promise<SnapSnapshot[]>
  close(): Promise<void>
}
