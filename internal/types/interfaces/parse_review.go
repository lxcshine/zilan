package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// ParseReviewRepository manages parse review queue items.
type ParseReviewRepository interface {
	CreateReviewItem(ctx context.Context, item *types.ParseReviewItem) error
	ListPendingReviews(ctx context.Context, tenantID uint64, kbID string, limit, offset int) ([]*types.ParseReviewItem, int64, error)
	UpdateReviewStatus(ctx context.Context, id string, status, resolution, reviewerID string) error
	GetReviewItem(ctx context.Context, id string) (*types.ParseReviewItem, error)
	DeleteByKnowledgeBase(ctx context.Context, tenantID uint64, kbID string) error
}
