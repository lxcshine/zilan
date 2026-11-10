package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// memoryFakeTenantRepo 提供工作区级记忆配置（FR-04 规则 1：上限可按工作区配置）。
type memoryFakeTenantRepo struct {
	interfaces.TenantRepository
	tenants map[uint64]*types.Tenant
}

func (f *memoryFakeTenantRepo) GetTenantByID(ctx context.Context, id uint64) (*types.Tenant, error) {
	if tenant, ok := f.tenants[id]; ok {
		return tenant, nil
	}
	return nil, fmt.Errorf("tenant not found")
}

// withTenantMemoryConfig 给测试服务挂上工作区记忆配置。
func withTenantMemoryConfig(svc *memoryService, tenantID uint64, cfg *types.TenantMemoryConfig) {
	svc.tenantRepo = &memoryFakeTenantRepo{tenants: map[uint64]*types.Tenant{
		tenantID: {ID: tenantID, MemoryConfig: cfg},
	}}
}

// failingArchiveRepo 让容量归档失败，用于验证 FR-04 的失败降级路径。
type failingArchiveRepo struct {
	interfaces.MemoryRepository
	fail bool
}

func (r *failingArchiveRepo) ArchiveFactsByIDs(
	ctx context.Context, tenantID uint64, userID string, ids []string, fromStatuses []string,
) (int64, error) {
	if r.fail {
		return 0, fmt.Errorf("archive unavailable")
	}
	return r.MemoryRepository.ArchiveFactsByIDs(ctx, tenantID, userID, ids, fromStatuses)
}

// seedGovernanceFact 直接落一条指定状态的记忆，绕开服务层的门控。
func seedGovernanceFact(t *testing.T, svc *memoryService, category, subject, predicate, object, status string) *types.MemoryFact {
	t.Helper()
	fact := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-1", MessageID: "u1",
		Category: category, Subject: subject, Predicate: predicate, Object: object,
		Content: subject + predicate + object, Status: status,
		Origin:        types.MemoryOriginExtracted,
		NormalizedKey: types.ComputeNormalizedKey(category, subject, predicate),
		Confidence:    0.8, Importance: 0.5,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(memoryTestCtx(), fact))
	return fact
}

// runExtractionTask 用给定的 LLM 输出跑一次后台抽取任务。
func runExtractionTask(t *testing.T, svc *memoryService, chatModel *memoryFakeChat,
	msgRepo *memoryFakeMessageRepo, llmJSON string, payload *types.MemoryExtractPayload) {
	t.Helper()
	chatModel.response = llmJSON
	msgRepo.messages = []*types.Message{{ID: "u1", SessionID: payload.SessionID, Role: "user", Content: "用户说了一句话"}}
	payloadBytes, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, svc.ProcessMemoryExtract(memoryTestCtx(), asynq.NewTask(types.TypeMemoryExtract, payloadBytes)))
}

const governanceExtractionJSON = `{"memories":[
  {"category":"fact","subject":"用户","predicate":"岗位","object":"平台架构师","content":"用户现在是平台架构师","confidence":0.9,"importance":0.7,"due_at":""}
],"session_summary":"","key_topics":[]}`

// ---------------------------------------------------------------------------
// FR-01 清空时序围栏（服务层：后台抽取任务）
// ---------------------------------------------------------------------------

// TestMemoryExtractTaskFencedByClear 覆盖"清空与抽取任务并发"：任务在清空前入队、
// 清空后执行，其携带的世代已失效，必须被静默丢弃且不调用模型。
func TestMemoryExtractTaskFencedByClear(t *testing.T) {
	svc, _, chatModel, msgRepo, _, enqueuer := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	payload := &types.MemoryExtractPayload{
		TenantID: 1, UserID: "alice", SessionID: "s-1",
		UserMessageID: "u1", AssistantMessageID: "a1",
	}
	svc.EnqueueMemoryExtract(ctx, payload)
	require.Len(t, enqueuer.tasks, 1)

	// 用户清空全部记忆：围栏世代轮换。
	_, err := svc.DeleteAllForUser(ctx)
	require.NoError(t, err)

	chatModel.response = governanceExtractionJSON
	msgRepo.messages = []*types.Message{{ID: "u1", SessionID: "s-1", Role: "user", Content: "我现在是架构师"}}
	require.NoError(t, svc.ProcessMemoryExtract(ctx, asynq.NewTask(types.TypeMemoryExtract, enqueuer.tasks[0].Payload())))

	require.Zero(t, chatModel.calls, "被围栏拦截的任务不应再调用模型")
	_, total, err := svc.memoryRepo.ListFacts(ctx, 1, "alice", &types.MemoryFactQuery{Status: "all"})
	require.NoError(t, err)
	require.EqualValues(t, 0, total, "清空后记忆列表必须保持为空")

	info, err := svc.GetStatusInfo(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, info.BlockedWrites, "被拦截的写入需计数（诊断指标）")
}

// TestMemoryExtractConversationBeforeCutoffDropped 覆盖"对话时间早于截止点"：
// 即使世代令牌缺失（老任务），时间型围栏同样拦截。
func TestMemoryExtractConversationBeforeCutoffDropped(t *testing.T) {
	svc, _, chatModel, msgRepo, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	cleared, err := svc.DeleteAllForUser(ctx)
	require.NoError(t, err)

	runExtractionTask(t, svc, chatModel, msgRepo, governanceExtractionJSON, &types.MemoryExtractPayload{
		TenantID: 1, UserID: "alice", SessionID: "s-1",
		UserMessageID: "u1", AssistantMessageID: "a1",
		ConversationAt: cleared.CutoffAt.Add(-time.Hour),
	})
	require.Zero(t, chatModel.calls)
	_, total, err := svc.memoryRepo.ListFacts(ctx, 1, "alice", &types.MemoryFactQuery{Status: "all"})
	require.NoError(t, err)
	require.EqualValues(t, 0, total)

	// 对话时间晚于截止点则正常写入（进 pending）。
	runExtractionTask(t, svc, chatModel, msgRepo, governanceExtractionJSON, &types.MemoryExtractPayload{
		TenantID: 1, UserID: "alice", SessionID: "s-2",
		UserMessageID: "u2", AssistantMessageID: "a2",
		ConversationAt: cleared.CutoffAt.Add(time.Hour),
	})
	require.Equal(t, 1, chatModel.calls)
	_, total, err = svc.memoryRepo.ListFacts(ctx, 1, "alice", &types.MemoryFactQuery{Status: "all"})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
}

// ---------------------------------------------------------------------------
// FR-02 确认门控
// ---------------------------------------------------------------------------

// TestMemoryPendingNeverRecalled 覆盖 FR-02 规则 4：pending 在常驻与语义两条
// 注入路径都必须被过滤。
func TestMemoryPendingNeverRecalled(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	// 常驻类目（preference）与语义类目（fact）各放一条 pending。
	seedGovernanceFact(t, svc, types.MemoryCategoryPreference, "用户", "偏好", "Vim", types.MemoryStatusPending)
	seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "负责", "Project X", types.MemoryStatusPending)
	// 一条 active 作为对照。
	active := seedGovernanceFact(t, svc, types.MemoryCategorySoul, "assistant", "称呼", "张工", types.MemoryStatusActive)

	got, err := svc.Recall(ctx, &types.MemoryRecallParams{Query: "用户偏好什么", Now: time.Now()})
	require.NoError(t, err)
	require.Len(t, got, 1, "只有 active 记忆可注入")
	require.Equal(t, active.ID, got[0].Fact.ID)

	block := svc.FormatRecalledForPrompt([]*types.RecalledMemory{
		{Kind: "fact", Fact: &types.MemoryFact{
			Category: types.MemoryCategoryPreference, Content: "用户偏好 Vim",
			Status: types.MemoryStatusPending,
		}},
		{Kind: "fact", Fact: &types.MemoryFact{
			Category: types.MemoryCategoryFact, Content: "用户曾是运维工程师",
			Status: types.MemoryStatusSuperseded,
		}},
		{Kind: "fact", Fact: &types.MemoryFact{
			Category: types.MemoryCategorySoul, Content: "称呼用户为张工",
			Status: types.MemoryStatusActive,
		}},
	})
	require.Contains(t, block, "称呼用户为张工")
	require.NotContains(t, block, "用户偏好 Vim", "pending 永不注入 prompt")
	require.NotContains(t, block, "运维工程师", "superseded 永不注入 prompt")
}

// TestMemoryManualCreateIsActive 覆盖 PRD §3.2：来源 manual 直接 active。
func TestMemoryManualCreateIsActive(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	require.NoError(t, svc.CreateFact(ctx, &types.MemoryFact{
		Category: types.MemoryCategoryProfile, Subject: "用户", Predicate: "是", Object: "运维负责人",
		Content: "用户是运维负责人", Importance: 0.8,
	}))

	facts, total, err := svc.ListFacts(ctx, &types.MemoryFactQuery{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, types.MemoryStatusActive, facts[0].Status)
	require.Equal(t, types.MemoryOriginManual, facts[0].Origin)
	require.NotEmpty(t, facts[0].NormalizedKey)

	// 完全相同的记忆再次创建 => 冲突（唯一索引语义），不产生重复行。
	require.ErrorIs(t, svc.CreateFact(ctx, &types.MemoryFact{
		Category: types.MemoryCategoryProfile, Subject: "用户", Predicate: "是", Object: "运维负责人",
		Content: "用户是运维负责人",
	}), types.ErrMemoryConflict)
}

// TestMemoryConfirmRejectRestoreFlow 覆盖状态机的确认/拒绝/恢复转移。
func TestMemoryConfirmRejectRestoreFlow(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	proposal := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "负责", "Project Y",
		types.MemoryStatusPending)

	// 确认 => active，且立刻可被召回注入。
	result, err := svc.ConfirmFacts(ctx, []string{proposal.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Affected)
	got, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", proposal.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusActive, got.Status)

	// 拒绝 => archived（可恢复）。
	second := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "负责", "Project Z",
		types.MemoryStatusPending)
	result, err = svc.RejectFacts(ctx, []string{second.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Affected)
	got, err = svc.memoryRepo.GetFactByID(ctx, 1, "alice", second.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusArchived, got.Status)

	require.NoError(t, svc.RestoreFact(ctx, second.ID))
	got, err = svc.memoryRepo.GetFactByID(ctx, 1, "alice", second.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusActive, got.Status)

	// 对已 active 的记忆再确认 => 状态机冲突，不静默成功。
	result, err = svc.ConfirmFacts(ctx, []string{proposal.ID})
	require.NoError(t, err)
	require.Zero(t, result.Affected)
	require.Equal(t, []string{proposal.ID}, result.Failed)

	// 批量：混合有效与无效 ID，单条冲突不阻塞其余条目。
	p1 := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "提及", "A", types.MemoryStatusPending)
	p2 := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "提及", "B", types.MemoryStatusPending)
	result, err = svc.ConfirmFacts(ctx, []string{p1.ID, "missing", p2.ID})
	require.NoError(t, err)
	require.EqualValues(t, 2, result.Affected)
	require.Equal(t, []string{"missing"}, result.Failed)
}

// TestMemoryPendingCapIndependentOfActive 覆盖 FR-02 规则 6：待确认超限只归档
// 最旧 pending，绝不触及 active。
func TestMemoryPendingCapIndependentOfActive(t *testing.T) {
	svc, db, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()
	withTenantMemoryConfig(svc, 1, &types.TenantMemoryConfig{MaxPendingItems: 2, MaxActiveItems: 200})

	active := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "负责", "active", types.MemoryStatusActive)
	var pendings []*types.MemoryFact
	for i, object := range []string{"oldest", "middle", "newest"} {
		fact := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "提及", object, types.MemoryStatusPending)
		createdAt := time.Now().Add(-time.Duration(3-i) * time.Hour)
		require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", fact.ID).Update("created_at", createdAt).Error)
		pendings = append(pendings, fact)
	}

	svc.enforceMemoryCapacity(ctx, 1, "alice")

	got, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", pendings[0].ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusArchived, got.Status, "最旧的 pending 被归档")
	for _, fact := range pendings[1:] {
		got, err = svc.memoryRepo.GetFactByID(ctx, 1, "alice", fact.ID)
		require.NoError(t, err)
		require.Equal(t, types.MemoryStatusPending, got.Status)
	}
	got, err = svc.memoryRepo.GetFactByID(ctx, 1, "alice", active.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusActive, got.Status, "active 不受待确认容量影响")

	notices, err := svc.ListGovernanceNotices(ctx)
	require.NoError(t, err)
	require.Len(t, notices, 1)
	require.Equal(t, types.MemoryEventPendingTrimmed, notices[0].Kind)

	acked, err := svc.AcknowledgeGovernanceNotices(ctx, []int64{notices[0].ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, acked)
	notices, err = svc.ListGovernanceNotices(ctx)
	require.NoError(t, err)
	require.Empty(t, notices, "提示只出现一次")
}

// TestMemoryWriteModeExplicitOnlySkipsExtraction 覆盖 FR-02 规则 7。
func TestMemoryWriteModeExplicitOnlySkipsExtraction(t *testing.T) {
	svc, _, _, _, _, enqueuer := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()
	withTenantMemoryConfig(svc, 1, &types.TenantMemoryConfig{WriteMode: types.MemoryWriteExplicitOnly})

	svc.EnqueueMemoryExtract(ctx, &types.MemoryExtractPayload{
		TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1",
	})
	require.Empty(t, enqueuer.tasks, "仅显式模式不发起后台抽取，因此不产生 pending")

	// 切回自动模式后恢复入队。
	withTenantMemoryConfig(svc, 1, &types.TenantMemoryConfig{WriteMode: types.MemoryWriteAuto})
	svc.EnqueueMemoryExtract(ctx, &types.MemoryExtractPayload{
		TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1",
	})
	require.Len(t, enqueuer.tasks, 1)
	var got types.MemoryExtractPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &got))
	require.False(t, got.ConversationAt.IsZero(), "入队时捕获对话时间")
}

// ---------------------------------------------------------------------------
// FR-03 取代链
// ---------------------------------------------------------------------------

// TestMemorySupersedeOnConfirmedConflict 覆盖：抽取产物与既有 active 记忆矛盾时
// 先落 pending（不立即取代），用户确认后才把旧值置为 superseded。
func TestMemorySupersedeOnConfirmedConflict(t *testing.T) {
	svc, _, chatModel, msgRepo, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	old := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "岗位", "运维工程师",
		types.MemoryStatusActive)

	runExtractionTask(t, svc, chatModel, msgRepo, governanceExtractionJSON, &types.MemoryExtractPayload{
		TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1", AssistantMessageID: "a1",
		ConversationAt: time.Now(),
	})

	pending, total, err := svc.ListFacts(ctx, &types.MemoryFactQuery{
		Status: types.MemoryStatusPending, Page: 1, PageSize: 10,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, old.ID, pending[0].ReplacesID, "pending 记下拟取代目标，但不立即取代")

	got, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", old.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusActive, got.Status, "确认前旧值仍然生效")

	// 用户确认 => 旧值转 superseded，新值转 active。
	_, err = svc.ConfirmFacts(ctx, []string{pending[0].ID})
	require.NoError(t, err)
	got, err = svc.memoryRepo.GetFactByID(ctx, 1, "alice", old.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusSuperseded, got.Status)
	require.Equal(t, pending[0].ID, got.SupersededBy)
	require.NotNil(t, got.SupersededAt)
	require.Equal(t, "用户岗位运维工程师", got.Content, "保留原文")

	// 取代链：当前值 + 历史值。
	chain, err := svc.GetFactChain(ctx, pending[0].ID)
	require.NoError(t, err)
	require.Len(t, chain, 2)
	require.Equal(t, types.MemoryStatusActive, chain[0].Status, "当前值排在链首")
	require.Equal(t, types.MemoryStatusSuperseded, chain[1].Status)

	// superseded 不再注入。
	recalled, err := svc.Recall(ctx, &types.MemoryRecallParams{Query: "用户岗位", Now: time.Now()})
	require.NoError(t, err)
	for _, m := range recalled {
		if m.Fact == nil {
			continue
		}
		require.NotEqual(t, old.ID, m.Fact.ID)
	}
}

// TestMemoryConflictThresholds 覆盖 FR-03 的矛盾判定分档：同键 => 确定矛盾；
// 相似度 >= 上阈值 => 确定矛盾；落在阈值边界 => 不自动取代，交用户裁决。
func TestMemoryConflictThresholds(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	// 候选直接落库（带显式向量）：服务层创建会用真实 embedder 覆盖向量，
	// 这里要精确控制相似度，因此绕开服务层。
	seed := func(subject, predicate, object string, embedding []float32) *types.MemoryFact {
		fact := &types.MemoryFact{
			TenantID: 1, UserID: "alice", SessionID: "s-1",
			Category: types.MemoryCategoryFact, Subject: subject, Predicate: predicate, Object: object,
			Content: object, Status: types.MemoryStatusActive, Origin: types.MemoryOriginManual,
			NormalizedKey: types.ComputeNormalizedKey(types.MemoryCategoryFact, subject, predicate),
			Embedding:     types.VectorBlob(embedding),
		}
		require.NoError(t, svc.memoryRepo.CreateFact(ctx, fact))
		return fact
	}

	sameKey := seed("用户", "岗位", "运维工程师", nil)
	highSim := seed("用户", "常用数据库", "MySQL", []float32{0, 1, 0, 0})
	edgeSim := seed("用户", "常用编辑器", "Vim", []float32{1, 0, 0, 0})
	lowSim := seed("用户", "所在城市", "杭州", []float32{0, 0, 1, 0})

	// 同 (category, subject, predicate)、不同 object => 确定矛盾。
	conflict := svc.detectConflicts(ctx, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact,
		Subject: "用户", Predicate: "岗位", Object: "平台架构师",
		NormalizedKey: types.ComputeNormalizedKey(types.MemoryCategoryFact, "用户", "岗位"),
	})
	require.Equal(t, []string{sameKey.ID}, conflict.certain)
	require.Empty(t, conflict.ambiguous)

	// 相似度 1.0 => 确定矛盾；0.8 落在边界 => 交用户裁决；0.0 => 无关。
	conflict = svc.detectConflicts(ctx, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact,
		Subject: "助手", Predicate: "备注", Object: "样本",
		NormalizedKey: types.ComputeNormalizedKey(types.MemoryCategoryFact, "助手", "备注"),
		Embedding:     types.VectorBlob([]float32{0.8, 0.6, 0, 0}),
	})
	require.Equal(t, []string{edgeSim.ID}, conflict.ambiguous, "阈值边界不自动取代")
	require.Empty(t, conflict.certain)
	require.Equal(t, edgeSim.ID, conflict.first(), "边界疑似矛盾转由用户在 pending 中裁决")

	conflict = svc.detectConflicts(ctx, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact,
		Subject: "助手", Predicate: "备注", Object: "样本",
		NormalizedKey: types.ComputeNormalizedKey(types.MemoryCategoryFact, "助手", "备注"),
		Embedding:     types.VectorBlob([]float32{0, 1, 0, 0}),
	})
	require.Equal(t, []string{highSim.ID}, conflict.certain)

	conflict = svc.detectConflicts(ctx, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact,
		Subject: "助手", Predicate: "备注", Object: "样本",
		NormalizedKey: types.ComputeNormalizedKey(types.MemoryCategoryFact, "助手", "备注"),
		Embedding:     types.VectorBlob([]float32{0, 0, 0, 1}),
	})
	require.Empty(t, conflict.certain)
	require.Empty(t, conflict.ambiguous)
	require.NotEmpty(t, lowSim.ID)
}

// TestMemoryManualCreateSupersedesSameKey 覆盖 FR-03 规则 1/2/3：写入确定矛盾的
// active 记忆时，旧值置 superseded（保留原文与取代时间），且不注入 prompt。
func TestMemoryManualCreateSupersedesSameKey(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	old := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "岗位", "运维工程师",
		types.MemoryStatusActive)
	require.NoError(t, svc.CreateFact(ctx, &types.MemoryFact{
		Category: types.MemoryCategoryFact, Subject: "用户", Predicate: "岗位", Object: "平台架构师",
		Content: "用户现在是平台架构师",
	}))

	got, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", old.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusSuperseded, got.Status)
	require.NotNil(t, got.SupersededAt)
	require.NotEmpty(t, got.SupersededBy)
	require.Equal(t, "用户岗位运维工程师", got.Content, "旧值原文保留，不删除")

	// superseded 不注入 prompt。
	recalled, err := svc.Recall(ctx, &types.MemoryRecallParams{Query: "用户岗位", Now: time.Now()})
	require.NoError(t, err)
	for _, m := range recalled {
		if m.Fact == nil {
			continue
		}
		require.NotEqual(t, old.ID, m.Fact.ID)
	}

	// 用户可以物理删除被取代的旧值（FR-03 规则 5）。
	require.NoError(t, svc.DeleteFact(ctx, old.ID, true))
	got, err = svc.memoryRepo.GetFactByID(ctx, 1, "alice", old.ID)
	require.NoError(t, err)
	require.Nil(t, got)
}

// ---------------------------------------------------------------------------
// FR-04 容量治理
// ---------------------------------------------------------------------------

// TestMemoryCapacityArchivesLowestRanked 覆盖 FR-04 规则 1/2/4/5：超限按排名归档
// 最低者（可逆，不物理删除），并给出一次性提示。
func TestMemoryCapacityArchivesLowestRanked(t *testing.T) {
	svc, db, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()
	withTenantMemoryConfig(svc, 1, &types.TenantMemoryConfig{MaxActiveItems: 2})

	// 高价值：近期命中 + 高重要度；低价值：陈旧 + 从未命中 + 低重要度。
	keeper := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "负责", "hot", types.MemoryStatusActive)
	mid := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "负责", "mid", types.MemoryStatusActive)
	cold := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "负责", "cold", types.MemoryStatusActive)

	stale := time.Now().Add(-200 * 24 * time.Hour)
	require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", cold.ID).
		Updates(map[string]interface{}{"updated_at": stale, "created_at": stale, "importance": 0.1}).Error)
	require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", mid.ID).
		Update("importance", 0.4).Error)
	require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", keeper.ID).
		Updates(map[string]interface{}{"importance": 0.9, "access_count": 5, "last_accessed_at": time.Now()}).Error)

	svc.enforceMemoryCapacity(ctx, 1, "alice")

	got, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", cold.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusArchived, got.Status, "排名最低者被归档而非删除")
	require.Equal(t, "用户负责cold", got.Content, "归档保留原文，可恢复")
	for _, id := range []string{keeper.ID, mid.ID} {
		got, err = svc.memoryRepo.GetFactByID(ctx, 1, "alice", id)
		require.NoError(t, err)
		require.Equal(t, types.MemoryStatusActive, got.Status)
	}

	notices, err := svc.ListGovernanceNotices(ctx)
	require.NoError(t, err)
	require.Len(t, notices, 1)
	require.Equal(t, types.MemoryEventCapacityArchived, notices[0].Kind)
	require.Equal(t, 1, notices[0].ItemCount)
	require.Equal(t, []string{cold.ID}, []string(notices[0].ItemIDs))
	require.Contains(t, notices[0].Reason, "上限 2 条")
}

// TestMemoryCapacityArchiveFailureDoesNotBlockWrites 覆盖 FR-04 异常：归档失败
// 不阻塞新写入，改为记录待重试；连续失败达阈值暂停自动抽取并通知管理员。
func TestMemoryCapacityArchiveFailureDoesNotBlockWrites(t *testing.T) {
	svc, _, _, _, _, enqueuer := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()
	withTenantMemoryConfig(svc, 1, &types.TenantMemoryConfig{MaxActiveItems: 1})

	failing := &failingArchiveRepo{MemoryRepository: svc.memoryRepo, fail: true}
	svc.memoryRepo = failing

	for i := 0; i < 3; i++ {
		// 每条用不同的谓语，避免矛盾判定把它们置为 superseded（本测试只验容量）。
		seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", fmt.Sprintf("提及%d", i),
			fmt.Sprintf("f%d", i), types.MemoryStatusActive)
	}
	// 归档一直失败，但写入照常成功（不阻塞主流程）。
	require.NoError(t, svc.CreateFact(ctx, &types.MemoryFact{
		Category: types.MemoryCategoryFact, Subject: "助手", Predicate: "备注", Object: "still-written",
		Content: "归档失败也不阻塞新写入", Origin: types.MemoryOriginManual,
	}))
	// 连续失败达阈值（CreateFact 已触发一次，这里再补两次）。
	for i := 0; i < types.MemoryArchiveFailureThreshold-1; i++ {
		svc.enforceMemoryCapacity(ctx, 1, "alice")
	}

	info, err := svc.GetStatusInfo(ctx)
	require.NoError(t, err)
	require.True(t, info.ExtractionPaused, "连续失败达阈值 => 暂停自动抽取")
	require.EqualValues(t, 4, info.ActiveCount, "归档失败时超限条目仍在，未被物理删除")

	// 暂停后不再入队抽取任务。
	svc.EnqueueMemoryExtract(ctx, &types.MemoryExtractPayload{
		TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1",
	})
	require.Empty(t, enqueuer.tasks)

	kinds := map[string]int{}
	notices, err := svc.ListGovernanceNotices(ctx)
	require.NoError(t, err)
	for _, notice := range notices {
		kinds[notice.Kind]++
	}
	require.NotZero(t, kinds[types.MemoryEventArchiveFailed], "失败被记录为待重试事件")
	require.Equal(t, 1, kinds[types.MemoryEventExtractionPaused], "暂停只通知一次")

	// 归档恢复后：计数清零、暂停解除、超限条目被归档。
	failing.fail = false
	svc.enforceMemoryCapacity(ctx, 1, "alice")
	info, err = svc.GetStatusInfo(ctx)
	require.NoError(t, err)
	require.False(t, info.ExtractionPaused)
	require.EqualValues(t, 1, info.ActiveCount, "恢复后按上限归档")
}

// TestMemoryBatchAndAdminPermissions 覆盖 PRD §5.1：批量归档/删除自己的记忆可用；
// 管理员代成员操作需要工作区管理员角色；确认/拒绝不允许代他人执行。
func TestMemoryBatchAndAdminPermissions(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	f1 := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "提及", "A", types.MemoryStatusPending)
	f2 := seedGovernanceFact(t, svc, types.MemoryCategoryFact, "用户", "提及", "B", types.MemoryStatusPending)

	// 本人批量确认（FR-02 规则 5）。
	result, err := svc.BatchUpdateFacts(ctx, "", types.MemoryBatchActionConfirm, []string{f1.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Affected)

	// 本人批量归档。
	result, err = svc.BatchUpdateFacts(ctx, "", types.MemoryBatchActionArchive, []string{f2.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Affected)
	got, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", f2.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusArchived, got.Status)

	// 本人批量物理删除。
	result, err = svc.BatchUpdateFacts(ctx, "", types.MemoryBatchActionDelete, []string{f2.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Affected)
	got, err = svc.memoryRepo.GetFactByID(ctx, 1, "alice", f2.ID)
	require.NoError(t, err)
	require.Nil(t, got)

	// 普通成员（无管理员角色）代他人操作 => 拒绝。
	_, err = svc.BatchUpdateFacts(ctx, "bob", types.MemoryBatchActionArchive, []string{f1.ID})
	require.ErrorIs(t, err, types.ErrMemoryForbidden)
	_, err = svc.ClearMemoryForTarget(ctx, "bob")
	require.ErrorIs(t, err, types.ErrMemoryForbidden)

	// 工作区管理员可以代成员批量归档与清空。
	adminCtx := context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleAdmin)
	bobFact := &types.MemoryFact{
		TenantID: 1, UserID: "bob", Category: types.MemoryCategoryFact,
		Subject: "bob", Predicate: "提及", Object: "x", Content: "bob 的记忆",
		Status: types.MemoryStatusPending,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(ctx, bobFact))

	result, err = svc.BatchUpdateFacts(adminCtx, "bob", types.MemoryBatchActionArchive, []string{bobFact.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Affected)

	// 管理员也不能代成员确认待确认项（仅记忆归属用户本人）。
	_, err = svc.BatchUpdateFacts(adminCtx, "bob", types.MemoryBatchActionConfirm, []string{bobFact.ID})
	require.ErrorIs(t, err, types.ErrMemoryForbidden)

	cleared, err := svc.ClearMemoryForTarget(adminCtx, "bob")
	require.NoError(t, err)
	require.EqualValues(t, 1, cleared.Deleted())
	require.NotEmpty(t, cleared.Generation, "管理员清空同样生成围栏")
}

// TestMemoryRetentionRankAlignsWithRecall 校验容量排名口径与召回评分同源：
// 近期命中、命中次数多、重要度高的记忆排名更高。
func TestMemoryRetentionRankAlignsWithRecall(t *testing.T) {
	now := time.Now()
	hot := types.MemoryRetentionRank(0.9, 0.9, 8, now, now)
	recent := types.MemoryRetentionRank(0.5, 0.8, 0, now.Add(-24*time.Hour), now)
	stale := types.MemoryRetentionRank(0.5, 0.8, 0, now.Add(-300*24*time.Hour), now)

	require.Greater(t, hot, recent)
	require.Greater(t, recent, stale, "时间衰减口径与召回一致")
	require.Greater(t,
		types.MemoryRetentionRank(0.9, 0.9, 0, now, now),
		types.MemoryRetentionRank(0.1, 0.1, 0, now, now),
		"重要度与置信度作为查询无关先验参与排名")
}
