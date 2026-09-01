// Package interfaces — backup & recovery contracts
// (PRD docs/prd/data-backup-recovery.md §6).
package interfaces

import (
	"context"
	"io"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// BackupRepository persists backup records and restore jobs.
type BackupRepository interface {
	// CreateRecord inserts a new backup record.
	CreateRecord(ctx context.Context, record *types.BackupRecord) error
	// UpdateRecord persists mutable fields (status, stats, finished_at,
	// error, retention_tag).
	UpdateRecord(ctx context.Context, record *types.BackupRecord) error
	// GetRecord fetches one record by id.
	GetRecord(ctx context.Context, id string) (*types.BackupRecord, error)
	// ListRecords returns records newest-first with optional status filter.
	ListRecords(ctx context.Context, status string, limit, offset int) ([]*types.BackupRecord, error)
	// ListSucceededBefore returns succeeded records finished before the
	// cutoff, oldest-first (retention sweep input).
	ListSucceededBefore(ctx context.Context, cutoff time.Time) ([]*types.BackupRecord, error)
	// GetLatestSucceeded returns the most recent succeeded record (nil
	// when none exists) — the incremental-sync baseline and the status
	// card's "last successful backup" timestamp.
	GetLatestSucceeded(ctx context.Context) (*types.BackupRecord, error)
	// DeleteRecord removes a record row (storage objects are pruned by
	// the service).
	DeleteRecord(ctx context.Context, id string) error
	// HasRunning reports whether a backup is currently in flight — the
	// single-flight guard (PRD §5.1 任务锁 backup:full:lock).
	HasRunning(ctx context.Context) (bool, error)

	// CreateRestoreJob inserts a restore job.
	CreateRestoreJob(ctx context.Context, job *types.BackupRestoreJob) error
	// UpdateRestoreJob persists job progress/status/report.
	UpdateRestoreJob(ctx context.Context, job *types.BackupRestoreJob) error
	// GetRestoreJob fetches one job.
	GetRestoreJob(ctx context.Context, id string) (*types.BackupRestoreJob, error)
}

// BackupService is the top-level backup & recovery facade consumed by
// handlers and the scheduler.
type BackupService interface {
	// Enabled reports whether the subsystem is active.
	Enabled() bool
	// StartScheduler registers the daily cron (no-op when disabled or
	// invalid — failures are logged, never fatal to boot). The returned
	// func stops the scheduler for graceful shutdown.
	StartScheduler(ctx context.Context) func()
	// RunBackup executes one full snapshot synchronously; trigger is
	// scheduled | manual | pre-delete.
	RunBackup(ctx context.Context, trigger string) (*types.BackupRecord, error)
	// ListRecords returns records newest-first.
	ListRecords(ctx context.Context, status string, limit, offset int) ([]*types.BackupRecord, error)
	// GetRecord returns one record with its manifest summary.
	GetRecord(ctx context.Context, id string) (*types.BackupRecord, *types.BackupManifest, error)
	// GetLatestSucceeded returns the newest succeeded record (nil when
	// none) — drives the status card's RPO countdown.
	GetLatestSucceeded(ctx context.Context) (*types.BackupRecord, error)
	// DeleteBackup prunes a snapshot (record row + storage objects).
	DeleteBackup(ctx context.Context, id string) error
	// ExportTenant streams a single-workspace export archive (jsonl.gz +
	// files) to w. Returns the archive size.
	ExportTenant(ctx context.Context, tenantID uint64, w io.Writer) (int64, error)
	// StartRestore validates and launches a restore job asynchronously;
	// the returned job is in its initial state.
	StartRestore(ctx context.Context, req *types.BackupRestoreJob) (*types.BackupRestoreJob, error)
	// GetRestoreJob returns one job for progress polling.
	GetRestoreJob(ctx context.Context, id string) (*types.BackupRestoreJob, error)
	// SnapshotTenantBeforeDelete is the pre-delete hook (PRD §7):
	// when pre_delete_snapshot is enabled, captures the workspace then
	// returns; failures are logged, never block the deletion.
	SnapshotTenantBeforeDelete(ctx context.Context, tenantID uint64)
	// GetConfig returns the admin-facing configuration projection
	// (credentials masked).
	GetConfig(ctx context.Context) (*types.BackupConfigInfo, error)
	// UpdateConfig applies runtime-adjustable settings (retention tiers,
	// schedule window) and re-arms the scheduler when the schedule changes.
	UpdateConfig(ctx context.Context, update *types.BackupConfigUpdate) (*types.BackupConfigInfo, error)
}
