package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// verificationCodeRepository persists ownership-proof codes (P0-4 §4.2).
// The table doubles as the audit + frequency-control source of truth:
// the resend interval and daily cap are answered by counting rows rather
// than by a separate counter store, which keeps the send endpoint honest
// across restarts.
type verificationCodeRepository struct {
	db *gorm.DB
}

// NewVerificationCodeRepository creates a verification code repository.
func NewVerificationCodeRepository(db *gorm.DB) interfaces.VerificationCodeRepository {
	return &verificationCodeRepository{db: db}
}

// Create inserts a new code record.
func (r *verificationCodeRepository) Create(ctx context.Context, record *types.VerificationCode) error {
	return r.db.WithContext(ctx).Create(record).Error
}

// LatestOutstanding returns the newest unconsumed record for
// (channel, target, purpose), or nil when there is none.
func (r *verificationCodeRepository) LatestOutstanding(ctx context.Context, channel, target, purpose string) (*types.VerificationCode, error) {
	var record types.VerificationCode
	err := r.db.WithContext(ctx).
		Where("channel = ? AND target = ? AND purpose = ? AND consumed_at IS NULL", channel, target, purpose).
		Order("created_at DESC").
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// CountSentSince counts records for (channel, target) created at or after
// since. Answers both the resend interval (count > 0 within the window)
// and the daily per-target cap.
func (r *verificationCodeRepository) CountSentSince(ctx context.Context, channel, target string, since time.Time) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&types.VerificationCode{}).
		Where("channel = ? AND target = ? AND created_at >= ?", channel, target, since).
		Count(&count).Error
	return count, err
}

// Update persists attempt/consumption transitions.
func (r *verificationCodeRepository) Update(ctx context.Context, record *types.VerificationCode) error {
	return r.db.WithContext(ctx).Save(record).Error
}
