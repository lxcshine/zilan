package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

// MemoryService drives the three-layer memory system: L3 fact extraction
// after each QA turn, L2 rolling session summaries, recall for prompt
// injection, and the user-facing memory management surface (GDPR).
type MemoryService interface {
	// EnqueueMemoryExtract dispatches the async extraction task for one
	// completed QA turn. Best-effort: failures are logged, never returned,
	// so the QA path is never blocked by memory.
	EnqueueMemoryExtract(ctx context.Context, payload *types.MemoryExtractPayload)
	// ProcessMemoryExtract is the task handler bound to types.TypeMemoryExtract
	// (asynq server mode and the Lite SyncTaskExecutor both register this).
	ProcessMemoryExtract(ctx context.Context, task *asynq.Task) error

	// Recall scores the user's L2+L3 memories against the query embedding and
	// returns the top-k. Tenant and user are taken from ctx; an empty
	// QueryEmbedding degrades to recency/importance ordering. Recall also
	// bumps access_count on the returned facts (fire-and-forget).
	Recall(ctx context.Context, params *types.MemoryRecallParams) ([]*types.RecalledMemory, error)
	// FormatRecalledForPrompt renders recalled memories as a system-prompt
	// block. Returns "" when there is nothing worth injecting.
	FormatRecalledForPrompt(memories []*types.RecalledMemory) string

	// ListFacts is the user-facing "AI 记住了我什么" listing.
	ListFacts(ctx context.Context, q *types.MemoryFactQuery) ([]*types.MemoryFact, int64, error)
	// UpdateFact applies a user edit (content/object/status) to one fact.
	UpdateFact(ctx context.Context, fact *types.MemoryFact) error
	// DeleteFact removes one fact owned by the ctx user.
	DeleteFact(ctx context.Context, id string) error
	// DeleteAllForUser erases every memory row of the ctx user (GDPR erase).
	DeleteAllForUser(ctx context.Context) (int64, error)
	// DeleteSessionMemory drops the L2 summary bound to a session; called
	// when the session itself is deleted. L3 facts intentionally survive
	// session deletion — that is the point of long-term memory.
	DeleteSessionMemory(ctx context.Context, tenantID uint64, sessionID string)
	// DeleteAllSummariesForUser drops every L2 session summary of the ctx
	// user; called when all sessions of the workspace are deleted.
	DeleteAllSummariesForUser(ctx context.Context) (int64, error)

	// IsEnabled reports whether memory writes/recall are enabled for the
	// user (per-user preference, default on).
	IsEnabled(ctx context.Context, userID string) bool
}

// MemoryRepository persists L3 facts and L2 session summaries.
// All read/write paths are scoped by (tenantID, userID): memories are
// strictly per-user and must never leak across users or tenants.
type MemoryRepository interface {
	// CreateFact inserts a new fact. The unique partial index on
	// (tenant_id, user_id, triple_hash) rejects exact duplicates.
	CreateFact(ctx context.Context, fact *types.MemoryFact) error
	// GetFactByTripleHash loads one active fact by its dedup hash, or nil
	// (without error) when absent. Used by extraction upserts.
	GetFactByTripleHash(ctx context.Context, tenantID uint64, userID, tripleHash string) (*types.MemoryFact, error)
	// GetFactByID loads one fact scoped to the user, or nil when absent.
	GetFactByID(ctx context.Context, tenantID uint64, userID, id string) (*types.MemoryFact, error)
	// UpdateFact updates mutable fields (content/object/status/importance/
	// confidence/due_at/embedding) of a fact scoped to the user.
	// Returns rows affected; 0 means not found or not visible.
	UpdateFact(ctx context.Context, fact *types.MemoryFact) (int64, error)
	// ListFacts returns the filtered, paged user-facing memory listing.
	ListFacts(ctx context.Context, tenantID uint64, userID string, q *types.MemoryFactQuery) ([]*types.MemoryFact, int64, error)
	// ListActiveFactsForRecall returns up to limit active facts updated since
	// the given time, for in-Go semantic rescoring.
	ListActiveFactsForRecall(ctx context.Context, tenantID uint64, userID string, categories []string, since time.Time, limit int) ([]*types.MemoryFact, error)
	// TouchFacts bumps access_count and last_accessed_at for recalled facts.
	TouchFacts(ctx context.Context, tenantID uint64, userID string, ids []string, accessedAt time.Time) error
	// DeleteFact soft-deletes one fact scoped to the user.
	DeleteFact(ctx context.Context, tenantID uint64, userID, id string) (int64, error)
	// DeleteAllFacts soft-deletes every fact of the user (GDPR erase).
	DeleteAllFacts(ctx context.Context, tenantID uint64, userID string) (int64, error)
	// CountActiveFacts counts active facts for the per-user cap check.
	CountActiveFacts(ctx context.Context, tenantID uint64, userID string) (int64, error)
	// ListLowestImportanceFacts returns the N lowest-importance active facts,
	// used for eviction when the per-user cap is exceeded.
	ListLowestImportanceFacts(ctx context.Context, tenantID uint64, userID string, limit int) ([]*types.MemoryFact, error)

	// UpsertSessionSummary inserts or refreshes (summary/topics/embedding/
	// message_count/last_message_at) the L2 row of one session.
	UpsertSessionSummary(ctx context.Context, summary *types.MemorySessionSummary) error
	// GetSessionSummary loads the L2 row of one session, or nil (without
	// error) when absent. Used by extraction to merge the previous summary.
	GetSessionSummary(ctx context.Context, tenantID uint64, sessionID string) (*types.MemorySessionSummary, error)
	// ListSessionSummariesForRecall returns recent summaries of the user for
	// in-Go semantic rescoring.
	ListSessionSummariesForRecall(ctx context.Context, tenantID uint64, userID string, since time.Time, limit int) ([]*types.MemorySessionSummary, error)
	// DeleteSessionSummaryBySession soft-deletes the summary bound to a
	// session (called when the session itself is deleted).
	DeleteSessionSummaryBySession(ctx context.Context, tenantID uint64, sessionID string) (int64, error)
	// DeleteAllSessionSummaries soft-deletes every summary of the user (GDPR).
	DeleteAllSessionSummaries(ctx context.Context, tenantID uint64, userID string) (int64, error)
}
