package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

// GraphCommunityRepository persists per-KB GraphRAG community summaries.
// The per-KB community set is small (<= types.MaxGraphCommunitiesPerKB), so
// similarity scoring happens in the service layer in Go.
type GraphCommunityRepository interface {
	// UpsertCommunities inserts or refreshes rows keyed by
	// (tenant_id, knowledge_base_id, community_key).
	UpsertCommunities(ctx context.Context, rows []*types.GraphCommunity) error
	// ListCommunities returns the active communities of one KB, ordered by
	// node count descending.
	ListCommunities(ctx context.Context, tenantID uint64, kbID string) ([]*types.GraphCommunity, error)
	// DeleteCommunitiesNotIn soft-deletes every community of the KB whose key
	// is not in keepKeys (stale rows after a rebuild). An empty keepKeys
	// deletes all rows of the KB.
	DeleteCommunitiesNotIn(ctx context.Context, tenantID uint64, kbID string, keepKeys []string) error
	// DeleteByKnowledgeBase soft-deletes all communities of a KB (KB teardown).
	DeleteByKnowledgeBase(ctx context.Context, tenantID uint64, kbID string) error
}

// GraphCommunityService builds and recalls GraphRAG community summaries.
type GraphCommunityService interface {
	// MaybeEnqueueBuild enqueues a debounced community rebuild for the KB.
	// It is a no-op when the graph backend is disabled or the KB has no
	// entity extraction enabled.
	MaybeEnqueueBuild(ctx context.Context, tenantID uint64, kbID, trigger string)
	// ProcessGraphCommunityBuild is the asynq task handler for
	// types.TypeGraphCommunityBuild.
	ProcessGraphCommunityBuild(ctx context.Context, task *asynq.Task) error
	// Recall returns up to topK communities of the given KBs whose summary
	// embedding is at least threshold-similar to queryVec, best first.
	Recall(ctx context.Context, tenantID uint64, kbIDs []string,
		queryVec []float32, topK int, threshold float64) ([]*types.GraphCommunity, error)
	// ListCommunities exposes the current summaries of one KB (handler use).
	ListCommunities(ctx context.Context, kbID string) ([]*types.GraphCommunity, error)
	// EnqueueRebuild validates the KB and enqueues an immediate rebuild
	// (manual trigger endpoint). Returns an error when graph RAG is not
	// available for this KB.
	EnqueueRebuild(ctx context.Context, kbID string) error
}
