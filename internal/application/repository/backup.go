package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// backupRepository persists backup_records / backup_restore_jobs.
//
// The record table doubles as the single-flight lock (PRD §5.1): a row
// in status "running" means a snapshot is in flight, so HasRunning is
// the guard both the cron and the manual-trigger API consult before
// starting another one. Rows are written by the backup service only;
// there is no soft-delete — retention deletes are physical.
type backupRepository struct {
	db *gorm.DB
}

// NewBackupRepository constructs the production backup repository.
func NewBackupRepository(db *gorm.DB) interfaces.BackupRepository {
	return &backupRepository{db: db}
}

// backupListLimitMax bounds list responses regardless of caller input.
const backupListLimitMax = 200

func (r *backupRepository) CreateRecord(ctx context.Context, record *types.BackupRecord) error {
	return r.db.WithContext(ctx).Create(record).Error
}

func (r *backupRepository) UpdateRecord(ctx context.Context, record *types.BackupRecord) error {
	return r.db.WithContext(ctx).Save(record).Error
}

func (r *backupRepository) GetRecord(ctx context.Context, id string) (*types.BackupRecord, error) {
	var record types.BackupRecord
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

func (r *backupRepository) ListRecords(
	ctx context.Context, status string, limit, offset int,
) ([]*types.BackupRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > backupListLimitMax {
		limit = backupListLimitMax
	}
	if offset < 0 {
		offset = 0
	}
	tx := r.db.WithContext(ctx).Model(&types.BackupRecord{})
	if status != "" {
		tx = tx.Where("status = ?", status)
	}
	var records []*types.BackupRecord
	if err := tx.Order("started_at DESC").Limit(limit).Offset(offset).Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

func (r *backupRepository) ListSucceededBefore(
	ctx context.Context, cutoff time.Time,
) ([]*types.BackupRecord, error) {
	var records []*types.BackupRecord
	err := r.db.WithContext(ctx).
		Where("status = ? AND COALESCE(finished_at, started_at) < ?", types.BackupStatusSucceeded, cutoff).
		Order("COALESCE(finished_at, started_at) ASC").
		Find(&records).Error
	return records, err
}

func (r *backupRepository) GetLatestSucceeded(ctx context.Context) (*types.BackupRecord, error) {
	var record types.BackupRecord
	err := r.db.WithContext(ctx).
		Where("status = ?", types.BackupStatusSucceeded).
		Order("COALESCE(finished_at, started_at) DESC").
		First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

func (r *backupRepository) DeleteRecord(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).
		Where("id = ?", id).
		Delete(&types.BackupRecord{}).Error
}

// HasRunning is the backup:full:lock equivalent. Multi-instance
// deployments still race between the check and the insert, but the
// unique snapshot id (bk_YYYYMMDD_HHMMSS) collapses same-second races
// to a duplicate-key error on CreateRecord, which the service maps to
// "already running". Different-second overlap on one instance is
// prevented by this check; cross-instance overlap requires the DB-level
// unique window, documented in the service.
func (r *backupRepository) HasRunning(ctx context.Context) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).
		Model(&types.BackupRecord{}).
		Where("status = ?", types.BackupStatusRunning).
		Count(&count).Error
	return count > 0, err
}

func (r *backupRepository) CreateRestoreJob(ctx context.Context, job *types.BackupRestoreJob) error {
	return r.db.WithContext(ctx).Create(job).Error
}

func (r *backupRepository) UpdateRestoreJob(ctx context.Context, job *types.BackupRestoreJob) error {
	return r.db.WithContext(ctx).Save(job).Error
}

func (r *backupRepository) GetRestoreJob(ctx context.Context, id string) (*types.BackupRestoreJob, error) {
	var job types.BackupRestoreJob
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}
