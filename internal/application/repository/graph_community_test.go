package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newGraphCommunityRepositoryForTest(t *testing.T) (interfaces.GraphCommunityRepository, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.GraphCommunity{}))
	// The dedup partial unique index lives in the migration SQL, not in GORM
	// tags, so replicate it here to exercise the real schema semantics.
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX uq_graph_communities_key
		ON graph_communities(tenant_id, knowledge_base_id, community_key)
		WHERE deleted_at IS NULL`).Error)

	return NewGraphCommunityRepository(db), db
}

func communityRowForTest(tenantID uint64, kbID, key string, nodeCount int) *types.GraphCommunity {
	return &types.GraphCommunity{
		TenantID:        tenantID,
		KnowledgeBaseID: kbID,
		CommunityKey:    key,
		Title:           "title-" + key,
		Summary:         "summary-" + key,
		NodeNames:       types.StringArray{"a", "b"},
		NodeCount:       nodeCount,
		RelCount:        1,
		Embedding:       types.VectorBlob{0.1, 0.2},
	}
}

func TestGraphCommunityUpsertInsertsThenRefreshesInPlace(t *testing.T) {
	repo, _ := newGraphCommunityRepositoryForTest(t)
	ctx := context.Background()

	row := communityRowForTest(1, "kb-1", "key-1", 3)
	require.NoError(t, repo.UpsertCommunities(ctx, []*types.GraphCommunity{row}))
	require.NotEmpty(t, row.ID)
	firstID := row.ID
	firstCreatedAt := row.CreatedAt

	// Rebuild re-detecting the same member set must update the same row.
	refreshed := communityRowForTest(1, "kb-1", "key-1", 5)
	refreshed.Title = "refreshed"
	refreshed.Summary = "refreshed summary"
	require.NoError(t, repo.UpsertCommunities(ctx, []*types.GraphCommunity{refreshed}))
	require.Equal(t, firstID, refreshed.ID, "upsert must keep the stable row id")
	require.Equal(t, firstCreatedAt.Unix(), refreshed.CreatedAt.Unix(), "created_at must survive the refresh")

	rows, err := repo.ListCommunities(ctx, 1, "kb-1")
	require.NoError(t, err)
	require.Len(t, rows, 1, "upsert must not duplicate the community")
	require.Equal(t, "refreshed", rows[0].Title)
	require.Equal(t, 5, rows[0].NodeCount)
}

func TestGraphCommunityListScopedAndOrdered(t *testing.T) {
	repo, _ := newGraphCommunityRepositoryForTest(t)
	ctx := context.Background()

	require.NoError(t, repo.UpsertCommunities(ctx, []*types.GraphCommunity{
		communityRowForTest(1, "kb-1", "small", 3),
		communityRowForTest(1, "kb-1", "large", 9),
		communityRowForTest(1, "kb-2", "other-kb", 7),
		communityRowForTest(2, "kb-1", "other-tenant", 8),
	}))

	rows, err := repo.ListCommunities(ctx, 1, "kb-1")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "large", rows[0].CommunityKey, "node_count DESC ordering")
	require.Equal(t, "small", rows[1].CommunityKey)
}

func TestGraphCommunityDeleteNotInPrunesStaleOnly(t *testing.T) {
	repo, _ := newGraphCommunityRepositoryForTest(t)
	ctx := context.Background()

	require.NoError(t, repo.UpsertCommunities(ctx, []*types.GraphCommunity{
		communityRowForTest(1, "kb-1", "keep", 3),
		communityRowForTest(1, "kb-1", "stale", 4),
		communityRowForTest(1, "kb-2", "keep", 5),
	}))

	require.NoError(t, repo.DeleteCommunitiesNotIn(ctx, 1, "kb-1", []string{"keep"}))

	rows, err := repo.ListCommunities(ctx, 1, "kb-1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "keep", rows[0].CommunityKey)

	// other KB untouched
	rows, err = repo.ListCommunities(ctx, 1, "kb-2")
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// empty keepKeys wipes the KB's communities
	require.NoError(t, repo.DeleteCommunitiesNotIn(ctx, 1, "kb-1", nil))
	rows, err = repo.ListCommunities(ctx, 1, "kb-1")
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestGraphCommunityDeleteByKnowledgeBase(t *testing.T) {
	repo, _ := newGraphCommunityRepositoryForTest(t)
	ctx := context.Background()

	require.NoError(t, repo.UpsertCommunities(ctx, []*types.GraphCommunity{
		communityRowForTest(1, "kb-1", "k1", 3),
		communityRowForTest(2, "kb-1", "k2", 3),
	}))

	require.NoError(t, repo.DeleteByKnowledgeBase(ctx, 1, "kb-1"))

	rows, err := repo.ListCommunities(ctx, 1, "kb-1")
	require.NoError(t, err)
	require.Empty(t, rows)

	// tenant 2's rows of the same KB id are not collaterally deleted
	rows, err = repo.ListCommunities(ctx, 2, "kb-1")
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestComputeGraphCommunityKeyDeterministic(t *testing.T) {
	a := types.ComputeGraphCommunityKey("kb-1", []string{"Bob", " alice "})
	b := types.ComputeGraphCommunityKey("kb-1", []string{"ALICE", "bob"})
	require.Equal(t, a, b, "order/case/whitespace must not change the key")
	require.NotEqual(t, a, types.ComputeGraphCommunityKey("kb-2", []string{"ALICE", "bob"}))
	require.NotEqual(t, a, types.ComputeGraphCommunityKey("kb-1", []string{"alice"}))
}
