package repository

import (
	"context"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// memoryRepository implements interfaces.MemoryRepository.
type memoryRepository struct {
	db *gorm.DB
}

// NewMemoryRepository creates a new memory repository instance.
func NewMemoryRepository(db *gorm.DB) interfaces.MemoryRepository {
	return &memoryRepository{db: db}
}

// scopeUser restricts a query to one user's memory rows. Unlike sessions
// there are no legacy shared rows: an empty userID only matches empty-user
// rows, never the whole tenant.
func (r *memoryRepository) scopeUser(db *gorm.DB, tenantID uint64, userID string) *gorm.DB {
	return db.Where("tenant_id = ? AND user_id = ?", tenantID, userID)
}

func (r *memoryRepository) CreateFact(ctx context.Context, fact *types.MemoryFact) error {
	now := time.Now()
	fact.CreatedAt = now
	fact.UpdatedAt = now
	if fact.Status == "" {
		fact.Status = types.MemoryStatusActive
	}
	if fact.TripleHash == "" {
		fact.TripleHash = types.ComputeTripleHash(fact.Category, fact.Subject, fact.Predicate, fact.Object)
	}
	return r.db.WithContext(ctx).Create(fact).Error
}

func (r *memoryRepository) GetFactByTripleHash(
	ctx context.Context, tenantID uint64, userID, tripleHash string,
) (*types.MemoryFact, error) {
	var fact types.MemoryFact
	err := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("triple_hash = ?", tripleHash).
		First(&fact).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &fact, nil
}

func (r *memoryRepository) GetFactByID(
	ctx context.Context, tenantID uint64, userID, id string,
) (*types.MemoryFact, error) {
	var fact types.MemoryFact
	err := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("id = ?", id).
		First(&fact).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &fact, nil
}

func (r *memoryRepository) UpdateFact(ctx context.Context, fact *types.MemoryFact) (int64, error) {
	updates := map[string]interface{}{
		"content":    fact.Content,
		"object":     fact.Object,
		"status":     fact.Status,
		"importance": fact.Importance,
		"confidence": fact.Confidence,
		"due_at":     fact.DueAt,
		"updated_at": time.Now(),
	}
	if fact.Embedding != nil {
		updates["embedding"] = fact.Embedding
	}
	// Re-derive the dedup hash when the triple itself is edited.
	if fact.TripleHash != "" {
		updates["triple_hash"] = fact.TripleHash
		updates["subject"] = fact.Subject
		updates["predicate"] = fact.Predicate
	}
	res := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), fact.TenantID, fact.UserID).
		Where("id = ?", fact.ID).
		Updates(updates)
	return res.RowsAffected, res.Error
}

func (r *memoryRepository) ListFacts(
	ctx context.Context, tenantID uint64, userID string, q *types.MemoryFactQuery,
) ([]*types.MemoryFact, int64, error) {
	applyFilters := func(db *gorm.DB) *gorm.DB {
		db = r.scopeUser(db, tenantID, userID)
		if q == nil {
			db = db.Where("status = ?", types.MemoryStatusActive)
			return db
		}
		switch q.Status {
		case "all":
			// no status filter
		case "":
			db = db.Where("status = ?", types.MemoryStatusActive)
		default:
			db = db.Where("status = ?", q.Status)
		}
		if q.Category != "" {
			db = db.Where("category = ?", q.Category)
		}
		if kw := strings.TrimSpace(q.Keyword); kw != "" {
			like := "LOWER(content) LIKE LOWER(?)"
			if r.db.Dialector.Name() == "postgres" {
				like = "content ILIKE ?"
			}
			db = db.Where(like, "%"+escapeLikeKeyword(kw)+"%")
		}
		return db
	}

	var total int64
	if err := applyFilters(r.db.WithContext(ctx).Model(&types.MemoryFact{})).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	page, size := 1, 20
	if q != nil {
		if q.Page > 0 {
			page = q.Page
		}
		if q.PageSize > 0 {
			size = q.PageSize
		}
	}

	facts := make([]*types.MemoryFact, 0)
	err := applyFilters(r.db.WithContext(ctx).Model(&types.MemoryFact{})).
		Order("updated_at DESC").
		Offset((page - 1) * size).
		Limit(size).
		Find(&facts).Error
	if err != nil {
		return nil, 0, err
	}
	return facts, total, nil
}

func (r *memoryRepository) ListActiveFactsForRecall(
	ctx context.Context, tenantID uint64, userID string, categories []string, since time.Time, limit int,
) ([]*types.MemoryFact, error) {
	if limit <= 0 {
		limit = types.MemoryRecallCandidateLimit
	}
	db := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("status = ?", types.MemoryStatusActive).
		Where("updated_at >= ?", since)
	if len(categories) > 0 {
		db = db.Where("category IN ?", categories)
	}
	facts := make([]*types.MemoryFact, 0)
	// Pre-order by importance so an over-large candidate set still keeps the
	// most valuable rows after the limit is applied.
	err := db.Order("importance DESC").Order("updated_at DESC").Limit(limit).Find(&facts).Error
	if err != nil {
		return nil, err
	}
	return facts, nil
}

func (r *memoryRepository) TouchFacts(
	ctx context.Context, tenantID uint64, userID string, ids []string, accessedAt time.Time,
) error {
	if len(ids) == 0 {
		return nil
	}
	return r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Where("id IN ?", ids).
		Updates(map[string]interface{}{
			"access_count":     gorm.Expr("access_count + 1"),
			"last_accessed_at": accessedAt,
		}).Error
}

func (r *memoryRepository) DeleteFact(ctx context.Context, tenantID uint64, userID, id string) (int64, error) {
	res := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("id = ?", id).
		Delete(&types.MemoryFact{})
	return res.RowsAffected, res.Error
}

func (r *memoryRepository) DeleteAllFacts(ctx context.Context, tenantID uint64, userID string) (int64, error) {
	res := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Delete(&types.MemoryFact{})
	return res.RowsAffected, res.Error
}

func (r *memoryRepository) CountActiveFacts(ctx context.Context, tenantID uint64, userID string) (int64, error) {
	var count int64
	err := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Where("status = ?", types.MemoryStatusActive).
		Count(&count).Error
	return count, err
}

// categoryCountRow is the grouped-count projection of
// CountActiveFactsByCategory.
type categoryCountRow struct {
	Category string `gorm:"column:category"`
	Count    int64  `gorm:"column:count"`
}

func (r *memoryRepository) CountActiveFactsByCategory(
	ctx context.Context, tenantID uint64, userID string,
) (map[string]int64, error) {
	var rows []categoryCountRow
	err := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Where("status = ?", types.MemoryStatusActive).
		Select("category, COUNT(*) as count").
		Group("category").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Category] = row.Count
	}
	return out, nil
}

func (r *memoryRepository) CountSessionSummaries(ctx context.Context, tenantID uint64, userID string) (int64, error) {
	var count int64
	err := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemorySessionSummary{}), tenantID, userID).
		Count(&count).Error
	return count, err
}

func (r *memoryRepository) ListLowestImportanceFacts(
	ctx context.Context, tenantID uint64, userID string, limit int,
) ([]*types.MemoryFact, error) {
	facts := make([]*types.MemoryFact, 0)
	// SQLite (Lite build) does not support NULLS FIRST; its default ASC order
	// already sorts NULLs first, which is exactly what eviction wants (never
	// accessed facts are evicted before recently accessed ones).
	nullsClause := "last_accessed_at ASC NULLS FIRST"
	if r.db.Dialector.Name() != "postgres" {
		nullsClause = "last_accessed_at ASC"
	}
	err := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("status = ?", types.MemoryStatusActive).
		Order("importance ASC").Order(nullsClause).Order("updated_at ASC").
		Limit(limit).
		Find(&facts).Error
	if err != nil {
		return nil, err
	}
	return facts, nil
}

func (r *memoryRepository) UpsertSessionSummary(ctx context.Context, summary *types.MemorySessionSummary) error {
	now := time.Now()
	summary.UpdatedAt = now

	var existing types.MemorySessionSummary
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ?", summary.TenantID, summary.SessionID).
		First(&existing).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return err
	}
	if err == gorm.ErrRecordNotFound {
		summary.CreatedAt = now
		return r.db.WithContext(ctx).Create(summary).Error
	}
	updates := map[string]interface{}{
		"title":           summary.Title,
		"summary":         summary.Summary,
		"key_topics":      summary.KeyTopics,
		"message_count":   summary.MessageCount,
		"last_message_at": summary.LastMessageAt,
		"updated_at":      now,
	}
	if summary.Embedding != nil {
		updates["embedding"] = summary.Embedding
	}
	// The row key is (tenant_id, session_id); do not let the caller move it
	// across users silently — enforce the stored owner on the update.
	return r.db.WithContext(ctx).Model(&types.MemorySessionSummary{}).
		Where("tenant_id = ? AND session_id = ? AND user_id = ?", summary.TenantID, summary.SessionID, existing.UserID).
		Updates(updates).Error
}

func (r *memoryRepository) GetSessionSummary(
	ctx context.Context, tenantID uint64, sessionID string,
) (*types.MemorySessionSummary, error) {
	var summary types.MemorySessionSummary
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ?", tenantID, sessionID).
		First(&summary).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &summary, nil
}

func (r *memoryRepository) ListSessionSummariesForRecall(
	ctx context.Context, tenantID uint64, userID string, since time.Time, limit int,
) ([]*types.MemorySessionSummary, error) {
	if limit <= 0 {
		limit = types.MemoryRecallCandidateLimit
	}
	summaries := make([]*types.MemorySessionSummary, 0)
	err := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("updated_at >= ?", since).
		Order("updated_at DESC").
		Limit(limit).
		Find(&summaries).Error
	if err != nil {
		return nil, err
	}
	return summaries, nil
}

func (r *memoryRepository) DeleteSessionSummaryBySession(
	ctx context.Context, tenantID uint64, sessionID string,
) (int64, error) {
	res := r.db.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ?", tenantID, sessionID).
		Delete(&types.MemorySessionSummary{})
	return res.RowsAffected, res.Error
}

func (r *memoryRepository) DeleteAllSessionSummaries(
	ctx context.Context, tenantID uint64, userID string,
) (int64, error) {
	res := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Delete(&types.MemorySessionSummary{})
	return res.RowsAffected, res.Error
}
