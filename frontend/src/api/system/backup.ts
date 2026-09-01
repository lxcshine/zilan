import { get, post, put, del } from '@/utils/request'

/**
 * Backup & recovery admin API (PRD docs/prd/data-backup-recovery.md §6.2).
 *
 * Every endpoint sits behind RequireSystemAdmin server-side and answers
 * 503 when the subsystem is disabled (WEKNORA_BACKUP_ENABLED=false) —
 * callers surface that state as a dedicated banner instead of an error.
 *
 * Mirrors internal/types/backup.go. The axios interceptor unwraps
 * response.data project-wide, hence the `as unknown as T` casts
 * (see utils/request.ts:97).
 */

/** Backup record row (backup_records table). */
export interface BackupRecord {
  id: string
  trigger_type: 'scheduled' | 'manual' | 'pre-delete'
  status: 'running' | 'succeeded' | 'failed' | 'expired'
  started_at: string
  finished_at: string | null
  base_path: string
  stats: BackupStats
  error: string
  retention_tag: 'daily' | 'weekly' | 'monthly' | ''
  created_at: string
  updated_at: string
}

/** Aggregated counters on a backup record. */
export interface BackupStats {
  workspaces: number
  files: number
  bytes: number
  rows: number
  duration_ms: number
  skipped_files: number
}

/** One workspace entry inside a snapshot manifest. */
export interface BackupManifestTenant {
  tenant_id: number
  metadata: { file: string; sha256: string; rows: number; bytes: number } | null
  files: { count: number; bytes: number; skipped?: string[] } | null
  knowledge_bases: number
}

/** Snapshot manifest — integrity ledger of a backup. */
export interface BackupManifest {
  backup_id: string
  trigger: string
  started_at: string
  finished_at: string
  instance_version: string
  encrypted: boolean
  tenants: BackupManifestTenant[]
  full_dump: { file: string; sha256: string; rows: number; bytes: number } | null
  reindex_plan: Record<string, string[]>
}

/** Restore job row (backup_restore_jobs table). */
export interface BackupRestoreJob {
  id: string
  backup_id: string
  scope: 'instance' | 'tenant'
  tenant_id: number
  conflict_mode: 'overwrite' | 'new' | ''
  status:
    | 'pending'
    | 'verifying'
    | 'restoring'
    | 'reindexing'
    | 'succeeded'
    | 'failed'
    | 'dry-run'
  progress: RestoreProgress
  created_by: string
  created_at: string
  report: RestoreReport | null
  finished_at: string | null
}

/** Live phase counters polled by the restore wizard. */
export interface RestoreProgress {
  phase: string
  rows_restored: number
  files_restored: number
  reindex_total: number
  reindex_done: number
  message?: string
}

/** Terminal restore report. */
export interface RestoreReport {
  rows_restored: number
  files_restored: number
  rows_skipped: number
  conflict_details?: string[]
  reindex_queued: number
  new_tenant_id?: number
  dry_run?: boolean
  would_restore_rows?: number
  would_restore_files?: number
}

/** Records list response (includes the status-card latest pointer). */
export interface BackupRecordsResponse {
  records: BackupRecord[]
  latestSucceeded: BackupRecord | null
}

/** List backup records, newest first. */
export async function listBackupRecords(params?: {
  status?: string
  limit?: number
  offset?: number
}): Promise<BackupRecordsResponse> {
  const query = new URLSearchParams()
  if (params?.status) query.set('status', params.status)
  if (params?.limit) query.set('limit', String(params.limit))
  if (params?.offset) query.set('offset', String(params.offset))
  const suffix = query.toString() ? `?${query.toString()}` : ''
  const response = await get(`/api/v1/system/backup/records${suffix}`)
  const body = response as unknown as {
    data: BackupRecord[]
    latest_succeeded: BackupRecord | null
  }
  return { records: body.data ?? [], latestSucceeded: body.latest_succeeded ?? null }
}

/** Fetch one record plus its manifest summary. */
export async function getBackupRecord(
  id: string,
): Promise<{ record: BackupRecord; manifest: BackupManifest | null }> {
  const response = await get(`/api/v1/system/backup/records/${encodeURIComponent(id)}`)
  const body = response as unknown as {
    data: BackupRecord
    manifest: BackupManifest | null
  }
  return { record: body.data, manifest: body.manifest ?? null }
}

/** Trigger a full backup synchronously. */
export async function runBackup(): Promise<BackupRecord> {
  const response = await post('/api/v1/system/backup/run')
  return (response as unknown as { data: BackupRecord }).data
}

/** Delete a snapshot (record row + storage objects). */
export async function deleteBackup(id: string): Promise<void> {
  await del(`/api/v1/system/backup/records/${encodeURIComponent(id)}`)
}

/** Restore request body. */
export interface StartRestoreRequest {
  backup_id: string
  scope: 'instance' | 'tenant'
  tenant_id?: number
  conflict_mode?: 'overwrite' | 'new'
  dry_run?: boolean
}

/** Launch a restore job (dry-run included). */
export async function startRestore(req: StartRestoreRequest): Promise<BackupRestoreJob> {
  const response = await post('/api/v1/system/backup/restore', req)
  return (response as unknown as { data: BackupRestoreJob }).data
}

/** Poll one restore job. */
export async function getRestoreJob(jobId: string): Promise<BackupRestoreJob> {
  const response = await get(`/api/v1/system/backup/restore/${encodeURIComponent(jobId)}`)
  return (response as unknown as { data: BackupRestoreJob }).data
}

/**
 * Download a workspace export archive (tar.gz). Uses a transient anchor
 * so the browser streams the blob to disk instead of buffering it in
 * the SPA's memory.
 */
export async function exportTenantArchive(tenantId: number): Promise<void> {
  const response = await post(
    `/api/v1/system/backup/tenants/${encodeURIComponent(String(tenantId))}/export`,
    undefined,
    { responseType: 'blob' },
  )
  const blob = response as unknown as Blob
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `workspace-${tenantId}-export.tar.gz`
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  URL.revokeObjectURL(url)
}

/** Admin-facing configuration projection (credentials masked). */
export interface BackupConfigInfo {
  enabled: boolean
  provider: string
  target_configured: boolean
  local_path?: string
  endpoint?: string
  bucket?: string
  path_prefix?: string
  schedule: string
  retention_daily: number
  retention_weekly: number
  retention_monthly: number
  compression: string
  encrypt: boolean
  pre_delete_snapshot: boolean
}

/** Runtime-adjustable configuration subset (nil fields keep current). */
export interface BackupConfigUpdate {
  schedule?: string
  retention_daily?: number
  retention_weekly?: number
  retention_monthly?: number
}

/** Read the effective backup configuration. */
export async function getBackupConfig(): Promise<BackupConfigInfo> {
  const response = await get('/api/v1/system/backup/config')
  return (response as unknown as { data: BackupConfigInfo }).data
}

/** Apply runtime-adjustable settings (retention tiers, schedule). */
export async function updateBackupConfig(
  update: BackupConfigUpdate,
): Promise<BackupConfigInfo> {
  const response = await put('/api/v1/system/backup/config', update)
  return (response as unknown as { data: BackupConfigInfo }).data
}
