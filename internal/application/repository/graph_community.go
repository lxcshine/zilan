package repository

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// graphCommunityRepository implements interfaces.GraphCommunityRepository.
type graphCommunityRepository struct {
	db *gorm.DB
}

// NewGraphCommunityRepository creates a new graph community repository.
func NewGraphCommunityRepository(db *gorm.DB) interfaces.GraphCommunityRepository {
	return &graphCommunityRepository{db: db}
}

func (r *graphCommunityRepository) UpsertCommunities(ctx context.Context, rows []*types.GraphCommunity) error {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now()
	// community_key is deterministic per (tenant, kb, member set), so a
	// rebuild re-detecting the same community refreshes summary/embedding in
	// place instead of accumulating duplicates. Read-then-write (not
	// ON CONFLICT) because the dedup unique index is partial
	// (WHERE deleted_at IS NULL) on both Postgres and SQLite — the same
	// reason memory_facts upserts by lookup.
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			row.UpdatedAt = now
			var existing types.GraphCommunity
			err := tx.Where(
				"tenant_id = ? AND knowledge_base_id = ? AND community_key = ?",
				row.TenantID, row.KnowledgeBaseID, row.CommunityKey,
			).First(&existing).Error
			switch {
			case err == gorm.ErrRecordNotFound:
				if row.CreatedAt.IsZero() {
					row.CreatedAt = now
				}
				if err := tx.Create(row).Error; err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				res := tx.Model(&types.GraphCommunity{}).
					Where("id = ?", existing.ID).
					Updates(map[string]interface{}{
						"title":              row.Title,
						"summary":            row.Summary,
						"node_names":         row.NodeNames,
						"node_count":         row.NodeCount,
						"rel_count":          row.RelCount,
						"summary_model_id":   row.SummaryModelID,
						"embedding_model_id": row.EmbeddingModelID,
						"embedding":          row.Embedding,
						"updated_at":         now,
					})
				if res.Error != nil {
					return res.Error
				}
				row.ID = existing.ID
				row.CreatedAt = existing.CreatedAt
			}
		}
		return nil
	})
}

func (r *graphCommunityRepository) ListCommunities(
	ctx context.Context, tenantID uint64, kbID string,
) ([]*types.GraphCommunity, error) {
	var rows []*types.GraphCommunity
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Order("node_count DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *graphCommunityRepository) DeleteCommunitiesNotIn(
	ctx context.Context, tenantID uint64, kbID string, keepKeys []string,
) error {
	q := r.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID)
	if len(keepKeys) > 0 {
		q = q.Where("community_key NOT IN ?", keepKeys)
	}
	return q.Delete(&types.GraphCommunity{}).Error
}

func (r *graphCommunityRepository) DeleteByKnowledgeBase(
	ctx context.Context, tenantID uint64, kbID string,
) error {
	return r.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Delete(&types.GraphCommunity{}).Error
}
