package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

type parseReviewRepository struct {
	db *gorm.DB
}

func NewParseReviewRepository(db *gorm.DB) interfaces.ParseReviewRepository {
	return &parseReviewRepository{db: db}
}

func (r *parseReviewRepository) CreateReviewItem(ctx context.Context, item *types.ParseReviewItem) error {
	if item.ID == "" {
		item.ID = generateUUID()
	}
	return r.db.WithContext(ctx).Create(item).Error
}

func (r *parseReviewRepository) ListPendingReviews(
	ctx context.Context, tenantID uint64, kbID string, limit, offset int,
) ([]*types.ParseReviewItem, int64, error) {
	var items []*types.ParseReviewItem
	var total int64

	q := r.db.WithContext(ctx).Model(&types.ParseReviewItem{}).
		Where("tenant_id = ? AND status = ?", tenantID, types.ParseReviewStatusPending)

	if kbID != "" {
		q = q.Where("knowledge_base_id = ?", kbID)
	}

	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	err := q.Order("created_at DESC").
		Limit(limit).Offset(offset).
		Find(&items).Error
	if err != nil {
		return nil, 0, err
	}

	return items, total, nil
}

func (r *parseReviewRepository) UpdateReviewStatus(
	ctx context.Context, id string, status, resolution, reviewerID string,
) error {
	updates := map[string]interface{}{
		"status":     status,
		"updated_at": time.Now(),
	}
	if resolution != "" {
		updates["resolution"] = resolution
	}
	if reviewerID != "" {
		updates["reviewer_id"] = reviewerID
	}
	if status == types.ParseReviewStatusResolved || status == types.ParseReviewStatusIgnored {
		now := time.Now()
		updates["reviewed_at"] = &now
	}

	result := r.db.WithContext(ctx).Model(&types.ParseReviewItem{}).
		Where("id = ?", id).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("parse review item not found: %s", id)
	}
	return nil
}

func (r *parseReviewRepository) GetReviewItem(ctx context.Context, id string) (*types.ParseReviewItem, error) {
	var item types.ParseReviewItem
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&item).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("parse review item not found: %s", id)
		}
		return nil, err
	}
	return &item, nil
}

func (r *parseReviewRepository) DeleteByKnowledgeBase(ctx context.Context, tenantID uint64, kbID string) error {
	return r.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Delete(&types.ParseReviewItem{}).Error
}

func generateUUID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
