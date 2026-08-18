package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newMemoryRepositoryForTest(t *testing.T) (interfaces.MemoryRepository, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.MemoryFact{}, &types.MemorySessionSummary{}))
	// The dedup/partial unique indexes live in the migration SQL, not in GORM
	// tags, so replicate them here to exercise the real schema semantics.
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX uq_memory_facts_triple
		ON memory_facts(tenant_id, user_id, triple_hash)
		WHERE deleted_at IS NULL AND triple_hash <> ''`).Error)
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX uq_memory_session_summaries_session
		ON memory_session_summaries(tenant_id, session_id) WHERE deleted_at IS NULL`).Error)

	return NewMemoryRepository(db), db
}

func createFactForTest(t *testing.T, repo interfaces.MemoryRepository, tenantID uint64, userID, category, content string) *types.MemoryFact {
	t.Helper()
	fact := &types.MemoryFact{
		TenantID:  tenantID,
		UserID:    userID,
		SessionID: "s-1",
		Category:  category,
		Subject:   "用户",
		Predicate: "提及",
		Object:    content,
		Content:   content,
	}
	require.NoError(t, repo.CreateFact(context.Background(), fact))
	require.NotEmpty(t, fact.ID)
	return fact
}

func TestMemoryRepositoryFactCRUDAndUserScope(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	alice := createFactForTest(t, repo, 1, "alice", types.MemoryCategoryFact, "alice 负责 Project X")
	_ = createFactForTest(t, repo, 1, "bob", types.MemoryCategoryFact, "bob 偏好 Python")
	_ = createFactForTest(t, repo, 2, "alice", types.MemoryCategoryFact, "other tenant fact")

	// GetByID honors tenant+user scope.
	got, err := repo.GetFactByID(ctx, 1, "alice", alice.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, alice.ID, got.ID)

	cross, err := repo.GetFactByID(ctx, 1, "bob", alice.ID)
	require.NoError(t, err)
	require.Nil(t, cross, "bob must not see alice's fact")

	crossTenant, err := repo.GetFactByID(ctx, 2, "alice", alice.ID)
	require.NoError(t, err)
	require.Nil(t, crossTenant)

	// ListFacts defaults to active-only, paged.
	facts, total, err := repo.ListFacts(ctx, 1, "alice", &types.MemoryFactQuery{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, facts, 1)

	// Update scoped to owner.
	alice.Content = "alice 负责 Project X 与 Project Y"
	rows, err := repo.UpdateFact(ctx, alice)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	alice.UserID = "bob"
	rows, err = repo.UpdateFact(ctx, alice)
	require.NoError(t, err)
	require.Zero(t, rows, "bob must not update alice's fact")

	// Delete scoped to owner.
	rows, err = repo.DeleteFact(ctx, 1, "bob", alice.ID)
	require.NoError(t, err)
	require.Zero(t, rows)
	rows, err = repo.DeleteFact(ctx, 1, "alice", alice.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
}

func TestMemoryRepositoryTripleHashDedup(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	fact := createFactForTest(t, repo, 1, "alice", types.MemoryCategoryPreference, "偏好 Python")
	require.Equal(t,
		types.ComputeTripleHash(fact.Category, fact.Subject, fact.Predicate, fact.Object),
		fact.TripleHash)

	// Same triple => same hash, and the unique index rejects a second row.
	dup := &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: fact.Category,
		Subject: fact.Subject, Predicate: fact.Predicate, Object: fact.Object,
		Content: "偏好 Python（重复）",
	}
	err := repo.CreateFact(ctx, dup)
	require.Error(t, err, "duplicate triple must be rejected by the unique index")

	got, err := repo.GetFactByTripleHash(ctx, 1, "alice", fact.TripleHash)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, fact.ID, got.ID)

	// After soft-delete the hash is free again (partial unique index).
	_, err = repo.DeleteFact(ctx, 1, "alice", fact.ID)
	require.NoError(t, err)
	require.NoError(t, repo.CreateFact(ctx, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: fact.Category,
		Subject: fact.Subject, Predicate: fact.Predicate, Object: fact.Object,
		Content: "偏好 Python",
	}))
}

func TestMemoryRepositoryRecallCandidatesAndTouch(t *testing.T) {
	repo, db := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	recent := createFactForTest(t, repo, 1, "alice", types.MemoryCategoryFact, "最近的事实")
	old := createFactForTest(t, repo, 1, "alice", types.MemoryCategoryFact, "陈旧的事实")
	done := createFactForTest(t, repo, 1, "alice", types.MemoryCategoryTodo, "已完成的待办")

	// Age the "old" row beyond the recall window, mark the todo done.
	require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", old.ID).
		Update("updated_at", time.Now().Add(-180*24*time.Hour)).Error)
	require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", done.ID).
		Update("status", types.MemoryStatusDone).Error)

	since := time.Now().Add(-90 * 24 * time.Hour)
	facts, err := repo.ListActiveFactsForRecall(ctx, 1, "alice", nil, since, 100)
	require.NoError(t, err)
	require.Len(t, facts, 1)
	require.Equal(t, recent.ID, facts[0].ID, "only the recent active fact is a recall candidate")

	// Category filter.
	_, err = repo.ListActiveFactsForRecall(ctx, 1, "alice", []string{types.MemoryCategoryTodo}, since, 100)
	require.NoError(t, err)

	// Touch bumps access_count and last_accessed_at.
	now := time.Now()
	require.NoError(t, repo.TouchFacts(ctx, 1, "alice", []string{recent.ID}, now))
	got, err := repo.GetFactByID(ctx, 1, "alice", recent.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.AccessCount)
	require.NotNil(t, got.LastAccessedAt)

	// Touch with foreign user scope is a no-op.
	require.NoError(t, repo.TouchFacts(ctx, 1, "bob", []string{recent.ID}, now))
	got, err = repo.GetFactByID(ctx, 1, "alice", recent.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.AccessCount)
}

func TestMemoryRepositoryCapEvictionOrder(t *testing.T) {
	repo, db := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	low := createFactForTest(t, repo, 1, "alice", types.MemoryCategoryFact, "低重要性")
	high := createFactForTest(t, repo, 1, "alice", types.MemoryCategoryFact, "高重要性")
	require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", low.ID).Update("importance", 0.1).Error)
	require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", high.ID).Update("importance", 0.9).Error)

	count, err := repo.CountActiveFacts(ctx, 1, "alice")
	require.NoError(t, err)
	require.EqualValues(t, 2, count)

	evict, err := repo.ListLowestImportanceFacts(ctx, 1, "alice", 1)
	require.NoError(t, err)
	require.Len(t, evict, 1)
	require.Equal(t, low.ID, evict[0].ID)

	rows, err := repo.DeleteAllFacts(ctx, 1, "alice")
	require.NoError(t, err)
	require.EqualValues(t, 2, rows)
	count, err = repo.CountActiveFacts(ctx, 1, "alice")
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestMemoryRepositorySessionSummaryUpsertAndScope(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	s := &types.MemorySessionSummary{
		TenantID: 1, UserID: "alice", SessionID: "sess-1",
		Title: "项目讨论", Summary: "讨论了 Project X 的里程碑", MessageCount: 4,
	}
	require.NoError(t, repo.UpsertSessionSummary(ctx, s))
	require.NotEmpty(t, s.ID)

	// Second upsert refreshes in place instead of duplicating.
	s.Summary = "讨论了 Project X 的里程碑与风险"
	s.MessageCount = 8
	require.NoError(t, repo.UpsertSessionSummary(ctx, s))

	list, err := repo.ListSessionSummariesForRecall(ctx, 1, "alice", time.Now().Add(-time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "讨论了 Project X 的里程碑与风险", list[0].Summary)
	require.Equal(t, 8, list[0].MessageCount)

	// Recall is user-scoped.
	list, err = repo.ListSessionSummariesForRecall(ctx, 1, "bob", time.Now().Add(-time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, list)

	// Old summaries fall out of the recall window.
	list, err = repo.ListSessionSummariesForRecall(ctx, 1, "alice", time.Now().Add(time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, list)

	rows, err := repo.DeleteSessionSummaryBySession(ctx, 1, "sess-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	rows, err = repo.DeleteAllSessionSummaries(ctx, 1, "alice")
	require.NoError(t, err)
	require.Zero(t, rows)
}
