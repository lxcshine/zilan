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
	// CreateFact 在记忆管理页创建一条记忆（来源 manual，直接 active，PRD §3.2）。
	CreateFact(ctx context.Context, fact *types.MemoryFact) error
	// UpdateFact applies a user edit (content/object/status) to one fact.
	UpdateFact(ctx context.Context, fact *types.MemoryFact) error
	// DeleteFact removes one fact owned by the ctx user. hard=true 时物理移除
	// （PRD §3.2：用户删除 = 物理移除），否则沿用软删除。
	DeleteFact(ctx context.Context, id string, hard bool) error
	// ConfirmFacts / RejectFacts 是待确认记忆的逐条与批量确认门控（FR-02 规则 5）。
	// 仅记忆归属用户本人可调用（PRD §5.1）。单条状态冲突不阻塞其余条目，
	// 冲突 ID 通过 MemoryBatchResult.Failed 回传。
	ConfirmFacts(ctx context.Context, ids []string) (*types.MemoryBatchResult, error)
	RejectFacts(ctx context.Context, ids []string) (*types.MemoryBatchResult, error)
	// RestoreFact 恢复一条 archived / superseded 记忆为 active；恢复 superseded
	// 时原 active 项转为 superseded（FR-03 规则 5）。
	RestoreFact(ctx context.Context, id string) error
	// BatchUpdateFacts 批量确认/拒绝/归档/删除。targetUserID 为空表示操作本人记忆，
	// 非空表示工作区管理员对成员记忆操作（PRD §5.1：批量归档/删除待确认项）。
	BatchUpdateFacts(ctx context.Context, targetUserID, action string, ids []string) (*types.MemoryBatchResult, error)
	// GetFactChain 返回一条记忆的取代链（当前值 + 历史值 + 变更时间，FR-03 规则 4）。
	GetFactChain(ctx context.Context, id string) ([]*types.MemoryFact, error)
	// GetStatusInfo 返回记忆管理页入口的状态摘要（含待确认角标计数）。
	GetStatusInfo(ctx context.Context) (*types.MemoryStatusInfo, error)
	// ListGovernanceNotices / AcknowledgeGovernanceNotices 支撑容量自动归档的
	// 一次性提示（FR-04 规则 5）。
	ListGovernanceNotices(ctx context.Context) ([]*types.MemoryGovernanceEvent, error)
	AcknowledgeGovernanceNotices(ctx context.Context, ids []int64) (int64, error)
	// DeleteAllForUser erases every memory row of the ctx user (GDPR erase)，
	// 并在同一事务内推进清空时序围栏（FR-01）。
	DeleteAllForUser(ctx context.Context) (*types.MemoryClearResult, error)
	// ClearMemoryForTarget 由工作区管理员清空成员记忆（PRD §5.1），同样触发围栏。
	ClearMemoryForTarget(ctx context.Context, targetUserID string) (*types.MemoryClearResult, error)
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

	// GetModuleOverview returns the four-module (soul/user/memory/agent)
	// counts for the memory page navigation badges.
	GetModuleOverview(ctx context.Context) ([]*types.MemoryModuleOverview, error)
	// GetSoulCard returns the global persona plus the user's style
	// directives (category=soul).
	GetSoulCard(ctx context.Context) (*types.SoulCard, error)
	// GetProfileCard returns the structured user profile with per-section
	// grouping and a completeness score.
	GetProfileCard(ctx context.Context) (*types.ProfileCard, error)
	// GetAgentTipsCard returns distilled skills plus the raw feedback wall
	// with feedback→skill upgrade links. The feedback wall is paged by
	// (page, pageSize).
	GetAgentTipsCard(ctx context.Context, page, pageSize int) (*types.AgentTipsCard, error)

	// -------------------------------------------------------------------
	// FR-09 记忆管理控制台（管理员侧，仅工作区管理员）
	//
	// 全部方法自行校验管理员身份并拒绝 API Key 主体，失败返回
	// types.ErrMemoryForbidden。返回值只有聚合计数与操作结果，不含记忆正文。
	// -------------------------------------------------------------------

	// AdminMemoryOverview 返回本工作区的记忆总量、按状态与按类型的分布（FR-09 规则 1）。
	AdminMemoryOverview(ctx context.Context) (*types.MemoryAdminOverview, error)
	// AdminListMemoryUsers 按用户维度分页返回记忆规模，用于识别异常增长（FR-09 规则 2）。
	AdminListMemoryUsers(ctx context.Context, limit, offset int) ([]*types.MemoryUserSummary, int64, error)
	// AdminGetMemoryUser 返回单个成员的规模明细；目标不在本工作区时返回
	// types.ErrMemoryAdminTargetNotFound（与"不存在"同一个错误，防探测）。
	AdminGetMemoryUser(ctx context.Context, userID string) (*types.MemoryAdminUserDetail, error)
	// AdminBatchArchiveFacts 批量归档成员记忆（可逆，FR-09 规则 3）。
	AdminBatchArchiveFacts(ctx context.Context, userID string, ids []string) (*types.MemoryAdminBatchResponse, error)
	// AdminBatchDeletePendingFacts 批量物理删除待确认项，只作用于 status=pending
	// （FR-09 规则 3 / FR-02 规则 6）；ids 为空表示该成员的全部待确认项。
	AdminBatchDeletePendingFacts(ctx context.Context, userID string, ids []string) (*types.MemoryAdminBatchResponse, error)
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
	// CountActiveFactsByCategory returns the active fact count per category
	// for the four-module overview aggregation (one grouped query).
	CountActiveFactsByCategory(ctx context.Context, tenantID uint64, userID string) (map[string]int64, error)
	// CountSessionSummaries counts the user's active L2 session summaries
	// for the memory-module overview badge.
	CountSessionSummaries(ctx context.Context, tenantID uint64, userID string) (int64, error)
	// ListLowestImportanceFacts returns the N lowest-importance active facts,
	// used for eviction when the per-user cap is exceeded.
	ListLowestImportanceFacts(ctx context.Context, tenantID uint64, userID string, limit int) ([]*types.MemoryFact, error)

	// -------------------------------------------------------------------
	// 记忆治理（FR-01 清空围栏 / FR-02 确认门控 / FR-03 取代链 / FR-04 容量）
	// -------------------------------------------------------------------

	// GetGovernance 读取 (租户, 用户) 的治理状态（围栏世代、截止点、失败计数）；
	// 从未产生过治理状态时返回 nil（不视为错误）。
	GetGovernance(ctx context.Context, tenantID uint64, userID string) (*types.MemoryGovernance, error)
	// EnsureGovernance 保证治理状态行存在（不存在则以新世代令牌创建）并返回。
	// 后台任务入队时调用，使每个在途任务都携带世代令牌。
	EnsureGovernance(ctx context.Context, tenantID uint64, userID string) (*types.MemoryGovernance, error)
	// FenceBlocked 报告一次写入是否会被围栏拦截：conversationAt 是写入所依据的
	// 对话时间，generation 是后台任务入队时捕获的世代令牌（空表示不做世代校验）。
	// 后台抽取任务在调用模型前先做这次预检查，避免为注定被丢弃的结果付费。
	FenceBlocked(ctx context.Context, tenantID uint64, userID string, conversationAt time.Time, generation string) (bool, error)
	// ClearAllForUser 在同一事务内物理清空该用户全部记忆并推进清空围栏。
	// 围栏生成失败必须整体回滚（PRD §5.3 规则 2）。
	ClearAllForUser(ctx context.Context, tenantID uint64, userID string) (*types.MemoryClearResult, error)
	// ConfirmFact 把一条 pending 记忆转为 active，并在同一事务内把 supersedeIDs
	// 置为 superseded（记录取代者与取代时间）。
	ConfirmFact(ctx context.Context, tenantID uint64, userID, id string, supersedeIDs []string) error
	// RejectFact 把一条 pending 记忆归档（可恢复）。
	RejectFact(ctx context.Context, tenantID uint64, userID, id string) error
	// RestoreFact 把一条 archived / superseded 记忆恢复为 active，并把 supersedeIDs
	// （当前 active 的冲突项）置为 superseded。
	RestoreFact(ctx context.Context, tenantID uint64, userID, id string, supersedeIDs []string) error
	// SupersedeFacts 把一批记忆置为 superseded，保留原文与取代时间。
	SupersedeFacts(ctx context.Context, tenantID uint64, userID string, ids []string, supersededBy string) (int64, error)
	// ArchiveFactsByIDs 归档一批记忆（可逆）；fromStatuses 限定来源状态，
	// 用于保证 active 与 pending 两类容量互不触及。
	ArchiveFactsByIDs(ctx context.Context, tenantID uint64, userID string, ids []string, fromStatuses []string) (int64, error)
	// HardDeleteFacts 物理删除一批记忆（不可逆）。
	HardDeleteFacts(ctx context.Context, tenantID uint64, userID string, ids []string) (int64, error)
	// ListFactsByConflictKey 返回同一矛盾判定键下的全部记忆（任意状态），
	// 用于渲染取代链。
	ListFactsByConflictKey(ctx context.Context, tenantID uint64, userID, normalizedKey string, limit int) ([]*types.MemoryFact, error)
	// CountFactsByStatus 按状态统计条数（待确认角标、容量判定）。
	CountFactsByStatus(ctx context.Context, tenantID uint64, userID string) (map[string]int64, error)
	// ListActiveFactRanks 返回容量治理排名所需的最小投影（不含 embedding）。
	ListActiveFactRanks(ctx context.Context, tenantID uint64, userID string) ([]types.MemoryFactRank, error)
	// TrimPendingFacts 归档超出上限的最旧待确认项，返回被归档条目；不触及 active。
	TrimPendingFacts(ctx context.Context, tenantID uint64, userID string, limit int) ([]*types.MemoryFact, error)
	// RecordArchiveFailure 递增归档连续失败计数，达阈值时暂停自动抽取。
	RecordArchiveFailure(ctx context.Context, tenantID uint64, userID string) (*types.MemoryGovernance, error)
	// ResetArchiveFailures 归档成功后清零失败计数并解除抽取暂停。
	ResetArchiveFailures(ctx context.Context, tenantID uint64, userID string) error
	// CreateGovernanceEvent 写入一条治理事件（一次性提示 / 异常通知）。
	CreateGovernanceEvent(ctx context.Context, event *types.MemoryGovernanceEvent) error
	// ListUnacknowledgedGovernanceEvents 返回尚未提示过的治理事件。
	ListUnacknowledgedGovernanceEvents(ctx context.Context, tenantID uint64, userID string, limit int) ([]*types.MemoryGovernanceEvent, error)
	// AcknowledgeGovernanceEvents 标记治理事件已提示。
	AcknowledgeGovernanceEvents(ctx context.Context, tenantID uint64, userID string, ids []int64) (int64, error)

	// -------------------------------------------------------------------
	// FR-09 记忆管理控制台：工作区级聚合
	//
	// 下列方法只带 tenantID（或 tenantID + userID），返回的永远是计数与 ID，
	// 不返回记忆正文。每条查询都写死 tenant_id 条件，聚合口径不越过工作区边界。
	// -------------------------------------------------------------------

	// CountTenantFactsByStatus 统计整个工作区按状态的记忆分布。
	CountTenantFactsByStatus(ctx context.Context, tenantID uint64) (map[string]int64, error)
	// CountTenantFactsByCategory 统计整个工作区按类型（category）的记忆分布。
	CountTenantFactsByCategory(ctx context.Context, tenantID uint64) (map[string]int64, error)
	// CountUserFactsByCategory 统计单个成员按类型的记忆分布（全状态）。
	CountUserFactsByCategory(ctx context.Context, tenantID uint64, userID string) (map[string]int64, error)
	// CountTenantMemoryUsers 统计工作区内至少有一条记忆的成员数。
	CountTenantMemoryUsers(ctx context.Context, tenantID uint64) (int64, error)
	// ListTenantMemoryUsers 按用户维度分页返回记忆规模（按规模倒序），
	// 附带成员总数。
	ListTenantMemoryUsers(ctx context.Context, tenantID uint64, limit, offset int) ([]*types.MemoryUserSummary, int64, error)
	// GetTenantMemoryUser 返回单个成员的规模计数；该成员在本工作区没有任何记忆时
	// 返回 (nil, nil)，调用方据此与"外部工作区"同返 404。
	GetTenantMemoryUser(ctx context.Context, tenantID uint64, userID string) (*types.MemoryUserSummary, error)
	// ListFactIDsByStatus 返回指定状态的记忆 ID；ids 非空时进一步收敛到这批 ID 内，
	// 为空表示该成员的全部该状态条目。批量删除待确认项据此把作用范围钉死在 pending。
	ListFactIDsByStatus(ctx context.Context, tenantID uint64, userID, status string, ids []string, limit int) ([]string, error)

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
