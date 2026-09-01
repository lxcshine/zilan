package types

import (
	"time"
)

// Backup & recovery models (PRD docs/prd/data-backup-recovery.md).
//
// The backup subsystem layers user data into three tiers with different
// strategies:
//   - metadata (business DB rows): daily full + per-workspace jsonl.gz export
//   - files (object storage): per-workspace prefix copy into the backup store
//   - vector index: NOT backed up — rebuilt from chunks after restore
//
// Terminology follows the project convention: "workspace" (空间), never the
// legacy term, in user-facing strings.

// Backup record states.
const (
	BackupStatusRunning   = "running"
	BackupStatusSucceeded = "succeeded"
	BackupStatusFailed    = "failed"
	BackupStatusExpired   = "expired"
)

// Backup trigger sources.
const (
	BackupTriggerScheduled = "scheduled"
	BackupTriggerManual    = "manual"
	BackupTriggerPreDelete = "pre-delete"
)

// GFS retention tags.
const (
	BackupRetentionDaily   = "daily"
	BackupRetentionWeekly  = "weekly"
	BackupRetentionMonthly = "monthly"
)

// Restore job states.
const (
	RestoreStatusPending    = "pending"
	RestoreStatusVerifying  = "verifying"
	RestoreStatusRestoring  = "restoring"
	RestoreStatusReindexing = "reindexing"
	RestoreStatusSucceeded  = "succeeded"
	RestoreStatusFailed     = "failed"
	RestoreStatusDryRun     = "dry-run"
)

// Restore scopes.
const (
	RestoreScopeInstance = "instance"
	RestoreScopeTenant   = "tenant"
)

// Restore conflict strategies (per-workspace restore only).
const (
	RestoreConflictOverwrite = "overwrite"
	RestoreConflictNew       = "new"
)

// BackupRecord is one completed (or in-flight) backup snapshot.
type BackupRecord struct {
	// ID is a human-readable snapshot id: bk_YYYYMMDD_HHMMSS.
	ID string `json:"id"                   gorm:"type:varchar(64);primaryKey"`
	// TriggerType: scheduled | manual | pre-delete.
	TriggerType string `json:"trigger_type"        gorm:"type:varchar(16);not null"`
	// Status: running | succeeded | failed | expired.
	Status string `json:"status"              gorm:"type:varchar(16);not null"`
	// StartedAt / FinishedAt bracket the run (UTC).
	StartedAt  time.Time  `json:"started_at"          gorm:"not null"`
	FinishedAt *time.Time `json:"finished_at"`
	// BasePath is the snapshot root inside the backup storage
	// (e.g. "backups/20260831").
	BasePath string `json:"base_path"           gorm:"type:varchar(512);not null"`
	// Stats aggregates per-workspace and global counters
	// (workspaces, files, bytes, rows, duration_ms …).
	Stats JSON `json:"stats"                gorm:"type:json;not null;default:'{}'"`
	// Error holds the failure reason when Status == failed.
	Error string `json:"error"               gorm:"type:text"`
	// RetentionTag: daily | weekly | monthly (GFS tier assignment).
	RetentionTag string    `json:"retention_tag"       gorm:"type:varchar(8)"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName overrides the default GORM table name.
func (BackupRecord) TableName() string { return "backup_records" }

// BackupRestoreJob tracks one restore operation (full instance or per
// workspace), including the post-restore index rebuild phase.
type BackupRestoreJob struct {
	// ID: rs_YYYYMMDD_HHMMSS_<rand>.
	ID string `json:"id"                   gorm:"type:varchar(80);primaryKey"`
	// BackupID references the source snapshot.
	BackupID string `json:"backup_id"          gorm:"type:varchar(64);not null;index"`
	// Scope: instance | tenant.
	Scope string `json:"scope"              gorm:"type:varchar(16);not null"`
	// TenantID is the source workspace for scope == tenant.
	TenantID uint64 `json:"tenant_id"`
	// ConflictMode: overwrite | new (tenant scope only).
	ConflictMode string `json:"conflict_mode"       gorm:"type:varchar(16)"`
	// Status: pending | verifying | restoring | reindexing | succeeded |
	// failed | dry-run.
	Status string `json:"status"             gorm:"type:varchar(16);not null"`
	// Progress carries phase-by-phase counters for the UI (JSON).
	Progress JSON `json:"progress"           gorm:"type:json;not null;default:'{}'"`
	// CreatedBy is the acting system-admin user id.
	CreatedBy string `json:"created_by"          gorm:"type:varchar(64);not null"`
	CreatedAt time.Time `json:"created_at"`
	// Report holds the final restore report (rows/files restored,
	// conflicts skipped, per-KB rebuild states).
	Report    JSON       `json:"report"             gorm:"type:json"`
	FinishedAt *time.Time `json:"finished_at"`
}

// TableName overrides the default GORM table name.
func (BackupRestoreJob) TableName() string { return "backup_restore_jobs" }

// BackupManifest is the integrity manifest stored at the snapshot root.
// Every referenced object carries a SHA-256 digest; restore verifies all
// digests before writing anything.
type BackupManifest struct {
	// BackupID mirrors BackupRecord.ID.
	BackupID string `json:"backup_id"`
	// Trigger mirrors BackupRecord.TriggerType.
	Trigger string `json:"trigger"`
	// StartedAt / FinishedAt (RFC3339).
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	// InstanceVersion is the app version that produced the snapshot.
	InstanceVersion string `json:"instance_version"`
	// Encrypted marks AES-256-GCM envelope encryption of metadata blobs.
	Encrypted bool `json:"encrypted"`
	// Tenants lists per-workspace entries.
	Tenants []*BackupManifestTenant `json:"tenants"`
	// FullDump describes the instance-wide metadata dump (all tables).
	FullDump *BackupManifestObject `json:"full_dump"`
	// ReindexPlan lists knowledge bases needing index rebuild after
	// restore (workspace → KB ids).
	ReindexPlan map[string][]string `json:"reindex_plan"`
}

// BackupManifestTenant is the per-workspace section of the manifest.
type BackupManifestTenant struct {
	// TenantID is the workspace id.
	TenantID uint64 `json:"tenant_id"`
	// Metadata describes the per-workspace jsonl.gz export.
	Metadata *BackupManifestObject `json:"metadata"`
	// Files counts the copied objects and their total size.
	Files *BackupManifestFiles `json:"files"`
	// KnowledgeBases is the KB count at snapshot time.
	KnowledgeBases int `json:"knowledge_bases"`
}

// BackupManifestObject is one metadata blob with its digest.
type BackupManifestObject struct {
	// File is the path inside the snapshot (forward slashes).
	File string `json:"file"`
	// SHA256 hex digest of the (plaintext) content.
	SHA256 string `json:"sha256"`
	// Rows in the jsonl stream.
	Rows int64 `json:"rows"`
	// Bytes of the (plaintext) content.
	Bytes int64 `json:"bytes"`
}

// BackupManifestFiles aggregates the file tier for one workspace.
type BackupManifestFiles struct {
	// Count of objects copied.
	Count int64 `json:"count"`
	// Bytes is the summed object size.
	Bytes int64 `json:"bytes"`
	// Skipped lists source objects that could not be read (recorded,
	// not fatal — see PRD §5.1 graceful degradation).
	Skipped []string `json:"skipped,omitempty"`
}

// BackupFileEntry is one file-tier object in the per-workspace
// _filelist.json — the per-object SHA-256 ledger that powers restore-time
// integrity verification (DoD §11.2 文件 SHA-256 逐一一致) without
// bloating the global manifest.
type BackupFileEntry struct {
	// Path is the source storage reference (provider://…, resource://…,
	// or bare path) exactly as the business tables reference it.
	Path string `json:"path"`
	// Key is the object key inside the snapshot (files/{tenantID}/…).
	Key string `json:"key"`
	// SHA256 hex digest of the copied content.
	SHA256 string `json:"sha256"`
	// Bytes of the object.
	Bytes int64 `json:"bytes"`
	// SourceHash is the business-side content fingerprint recorded with
	// the reference (knowledge.file_hash for documents). The incremental
	// sync skips re-reading the primary object when this fingerprint is
	// unchanged since the previous snapshot (PRD §5.1 step 3).
	SourceHash string `json:"source_hash,omitempty"`
}

// BackupFileList is the _filelist.json document for one workspace.
type BackupFileList struct {
	// TenantID is the workspace the list belongs to.
	TenantID uint64 `json:"tenant_id"`
	// Entries lists every copied object.
	Entries []*BackupFileEntry `json:"entries"`
}

// BackupStats is the Stats JSON payload on BackupRecord.
type BackupStats struct {
	// Workspaces is the number of workspaces snapshotted.
	Workspaces int `json:"workspaces"`
	// Files is the total object count copied.
	Files int64 `json:"files"`
	// Bytes is the total object size copied.
	Bytes int64 `json:"bytes"`
	// Rows is the total metadata rows exported (all tables).
	Rows int64 `json:"rows"`
	// DurationMS is the wall-clock duration in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// SkippedFiles counts source objects that failed to copy.
	SkippedFiles int64 `json:"skipped_files"`
}

// RestoreProgress is the Progress JSON payload on BackupRestoreJob.
type RestoreProgress struct {
	// Phase mirrors the job status (verifying/restoring/reindexing…).
	Phase string `json:"phase"`
	// RowsRestored / FilesRestored counters.
	RowsRestored  int64 `json:"rows_restored"`
	FilesRestored int64 `json:"files_restored"`
	// ReindexTotal / ReindexDone track the KB rebuild phase.
	ReindexTotal int `json:"reindex_total"`
	ReindexDone  int `json:"reindex_done"`
	// Message is a human-readable phase note (English; UI localizes).
	Message string `json:"message,omitempty"`
}

// RestoreReport is the Report JSON payload on BackupRestoreJob.
type RestoreReport struct {
	// RowsRestored / FilesRestored totals.
	RowsRestored  int64 `json:"rows_restored"`
	FilesRestored int64 `json:"files_restored"`
	// RowsSkipped: rows dropped due to PK conflicts (reported, not fatal).
	RowsSkipped int64 `json:"rows_skipped"`
	// ConflictDetails lists up to N skipped rows for operator review.
	ConflictDetails []string `json:"conflict_details,omitempty"`
	// ReindexQueued: knowledge bases enqueued for reparse.
	ReindexQueued int `json:"reindex_queued"`
	// NewTenantID: when conflict mode == new, the freshly created
	// workspace that received the data.
	NewTenantID uint64 `json:"new_tenant_id,omitempty"`
	// DryRun marks a verification-only run.
	DryRun bool `json:"dry_run,omitempty"`
	// WouldRestore counters a dry-run would have applied.
	WouldRestoreRows  int64 `json:"would_restore_rows,omitempty"`
	WouldRestoreFiles int64 `json:"would_restore_files,omitempty"`
}

// BackupConfigInfo is the admin-facing projection of the backup
// configuration (PRD §6.2 GET/PUT /system/backup/config). Credentials are
// never echoed — only whether the target is configured.
type BackupConfigInfo struct {
	Enabled           bool   `json:"enabled"`
	Provider          string `json:"provider"`
	TargetConfigured  bool   `json:"target_configured"`
	LocalPath         string `json:"local_path,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	Bucket            string `json:"bucket,omitempty"`
	PathPrefix        string `json:"path_prefix,omitempty"`
	Schedule          string `json:"schedule"`
	RetentionDaily    int    `json:"retention_daily"`
	RetentionWeekly   int    `json:"retention_weekly"`
	RetentionMonthly  int    `json:"retention_monthly"`
	Compression       string `json:"compression"`
	Encrypt           bool   `json:"encrypt"`
	PreDeleteSnapshot bool   `json:"pre_delete_snapshot"`
}

// BackupConfigUpdate carries the runtime-adjustable subset (retention
// tiers and the schedule window). Nil fields keep the current value;
// storage target and encryption changes require a restart by design —
// rotating those mid-flight would split a snapshot chain across stores.
type BackupConfigUpdate struct {
	Schedule         *string `json:"schedule,omitempty"`
	RetentionDaily   *int    `json:"retention_daily,omitempty"`
	RetentionWeekly  *int    `json:"retention_weekly,omitempty"`
	RetentionMonthly *int    `json:"retention_monthly,omitempty"`
}
