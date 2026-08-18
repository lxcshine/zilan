package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newParseReviewRepoForTest(t *testing.T) (*parseReviewRepository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.ParseReviewItem{}))
	return &parseReviewRepository{db: db}, db
}

func TestCreateAndListReviewItem(t *testing.T) {
	repo, _ := newParseReviewRepoForTest(t)
	ctx := context.Background()

	item := &types.ParseReviewItem{
		TenantID:        1,
		KnowledgeID:    "kb-test-001",
		KnowledgeBaseID: "kb-base-001",
		FileName:        "test.pdf",
		FileType:        "pdf",
		FileSize:        1024,
		EngineUsed:      "builtin",
		QualityScore:    0.45,
		GarbleRate:      0.25,
		RetryReason:     "garble_rate=0.25",
		Status:          types.ParseReviewStatusPending,
	}

	require.NoError(t, repo.CreateReviewItem(ctx, item))
	require.NotEmpty(t, item.ID)

	// List pending
	items, total, err := repo.ListPendingReviews(ctx, 1, "kb-base-001", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, items, 1)
	require.Equal(t, "test.pdf", items[0].FileName)
}

func TestUpdateReviewStatus(t *testing.T) {
	repo, _ := newParseReviewRepoForTest(t)
	ctx := context.Background()

	item := &types.ParseReviewItem{
		TenantID:        1,
		KnowledgeID:    "kb-test-002",
		KnowledgeBaseID: "kb-base-002",
		FileName:        "bad.pdf",
		Status:          types.ParseReviewStatusPending,
	}
	require.NoError(t, repo.CreateReviewItem(ctx, item))

	// Update to resolved
	err := repo.UpdateReviewStatus(ctx, item.ID, types.ParseReviewStatusResolved,
		types.ParseReviewResolutionReparse, "reviewer-1")
	require.NoError(t, err)

	// Verify update
	updated, err := repo.GetReviewItem(ctx, item.ID)
	require.NoError(t, err)
	require.Equal(t, types.ParseReviewStatusResolved, updated.Status)
	require.Equal(t, types.ParseReviewResolutionReparse, updated.Resolution)
	require.Equal(t, "reviewer-1", updated.ReviewerID)
	require.NotNil(t, updated.ReviewedAt)

	// Pending list should be empty now
	items, total, err := repo.ListPendingReviews(ctx, 1, "kb-base-002", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), total)
	require.Empty(t, items)
}

func TestDeleteByKnowledgeBase(t *testing.T) {
	repo, _ := newParseReviewRepoForTest(t)
	ctx := context.Background()

	require.NoError(t, repo.CreateReviewItem(ctx, &types.ParseReviewItem{
		TenantID:        1,
		KnowledgeID:    "k1",
		KnowledgeBaseID: "kb-del-1",
		FileName:        "a.pdf",
		Status:          types.ParseReviewStatusPending,
	}))
	require.NoError(t, repo.CreateReviewItem(ctx, &types.ParseReviewItem{
		TenantID:        2,
		KnowledgeID:    "k2",
		KnowledgeBaseID: "kb-del-1",
		FileName:        "b.pdf",
		Status:          types.ParseReviewStatusPending,
	}))

	require.NoError(t, repo.DeleteByKnowledgeBase(ctx, 1, "kb-del-1"))

	items, total, err := repo.ListPendingReviews(ctx, 1, "kb-del-1", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), total)
	require.Empty(t, items)

	// Tenant 2's item should still exist
	items, total, err = repo.ListPendingReviews(ctx, 2, "kb-del-1", 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, items, 1)
}
