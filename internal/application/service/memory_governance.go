package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// errMemoryFactNotFound 与既有 handler 的字符串匹配保持兼容（handler 据此返回 404）。
var errMemoryFactNotFound = errors.New("memory fact not found")

// errMemoryScopeUnavailable 表示请求上下文缺少 (租户, 用户) 记忆作用域。
var errMemoryScopeUnavailable = errors.New("memory scope unavailable")

// memoryGovernanceNoticeItemLimit 限制单条治理事件携带的条目明细数量：提示只需
// 说明"归档了哪些"，不需要把上千条内容塞进一行 jsonb。
const memoryGovernanceNoticeItemLimit = 20

// ---------------------------------------------------------------------------
// 作用域与权限（PRD §5.1）
// ---------------------------------------------------------------------------

// memorySelfScope 解析"仅记忆归属用户本人"的操作作用域。
func memorySelfScope(ctx context.Context) (uint64, string, error) {
	tenantID, userID, ok := memoryScopeFromContext(ctx)
	if !ok {
		return 0, "", errMemoryScopeUnavailable
	}
	return tenantID, userID, nil
}

// memoryTargetScope 解析可能作用于其他成员的操作作用域：targetUserID 为空表示
// 本人；非空表示工作区管理员代成员操作，需管理员及以上角色（PRD §5.1）。
// 跨工作区准入沿用既有中间件规则，这里只校验工作区内角色。
func memoryTargetScope(ctx context.Context, targetUserID string) (uint64, string, error) {
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return 0, "", errMemoryScopeUnavailable
	}
	if strings.TrimSpace(targetUserID) == "" {
		return memorySelfScope(ctx)
	}
	if !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin) {
		return 0, "", types.ErrMemoryForbidden
	}
	return tenantID, strings.TrimSpace(targetUserID), nil
}

// ---------------------------------------------------------------------------
// 工作区级记忆配置（FR-02 规则 7 / FR-04 规则 1）
// ---------------------------------------------------------------------------

// memoryConfig 读取工作区级记忆配置；未配置或读取失败时返回 nil，
// 由各 Effective* 方法回落到全局默认（PRD §5.4：存量工作区无需迁移）。
func (s *memoryService) memoryConfig(ctx context.Context, tenantID uint64) *types.TenantMemoryConfig {
	if s.tenantRepo == nil || tenantID == 0 {
		return nil
	}
	tenant, err := s.tenantRepo.GetTenantByID(ctx, tenantID)
	if err != nil || tenant == nil {
		return nil
	}
	return tenant.MemoryConfig
}

// autoExtractAllowed 报告是否允许为该主体发起后台抽取：
//   - 写入模式为「仅显式」时不抽取，因此不产生 pending 项（FR-02 规则 7）；
//   - 容量归档连续失败达阈值后暂停抽取（FR-04 异常）。
func (s *memoryService) autoExtractAllowed(ctx context.Context, tenantID uint64, userID string) bool {
	if !s.memoryConfig(ctx, tenantID).AutoExtractEnabled() {
		return false
	}
	gov, err := s.memoryRepo.GetGovernance(ctx, tenantID, userID)
	if err != nil {
		// 暂停状态读不到时不阻断抽取：这是增强能力的失败（PRD §5.3 规则 1），
		// 真正的正确性闸门是写入前的围栏校验。
		logger.Warnf(ctx, "[Memory] read governance state failed: %v", err)
		return true
	}
	return !gov.ExtractionPaused()
}

// captureFence 在任务入队时捕获围栏世代令牌与对话时间（FR-01 规则 4）。
// 治理行不存在时会创建一行：只有让每个在途任务都带上世代令牌，"入队后发生
// 首次清空"这种情况才拦得住（时间截止点受时钟精度限制，只能作为兜底）。
func (s *memoryService) captureFence(
	ctx context.Context, tenantID uint64, userID string, conversationAt time.Time,
) types.MemoryWriteFence {
	fence := types.MemoryWriteFence{ConversationAt: conversationAt}
	if fence.ConversationAt.IsZero() {
		fence.ConversationAt = time.Now()
	}
	gov, err := s.memoryRepo.EnsureGovernance(ctx, tenantID, userID)
	if err != nil {
		logger.Warnf(ctx, "[Memory] capture fence generation failed: %v", err)
		return fence
	}
	if gov != nil {
		fence.Generation = gov.Generation
	}
	return fence
}

// fenceBlocks 是后台任务落库前的围栏预检查（FR-01 规则 4）。命中时静默返回 true：
// 用户已明确表达删除意图，被拦截的写入只计数、不报错（FR-01 规则 5）。
func (s *memoryService) fenceBlocks(
	ctx context.Context, tenantID uint64, userID string, fence types.MemoryWriteFence,
) bool {
	blocked, err := s.memoryRepo.FenceBlocked(ctx, tenantID, userID, fence.ConversationAt, fence.Generation)
	if err != nil {
		// 读不到围栏就不写：正确性优先（PRD §5.3 规则 2）。
		logger.Warnf(ctx, "[Memory] fence precheck failed, dropping write: %v", err)
		return true
	}
	return blocked
}

// ---------------------------------------------------------------------------
// FR-02 确认门控
// ---------------------------------------------------------------------------

// CreateFact 在记忆管理页创建一条记忆：来源 manual，按 PRD §3.2 直接进入 active。
func (s *memoryService) CreateFact(ctx context.Context, fact *types.MemoryFact) error {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return err
	}
	if fact == nil {
		return fmt.Errorf("memory fact is required")
	}
	content := strings.TrimSpace(fact.Content)
	if content == "" {
		return fmt.Errorf("memory content is required")
	}
	if !validMemoryCategory(fact.Category) {
		return fmt.Errorf("invalid memory category")
	}

	fact.ID = ""
	fact.TenantID = tenantID
	fact.UserID = userID
	fact.Content = content
	fact.Subject = strings.TrimSpace(fact.Subject)
	fact.Predicate = strings.TrimSpace(fact.Predicate)
	fact.Object = strings.TrimSpace(fact.Object)
	fact.Confidence = clamp01(fact.Confidence, 1)
	fact.Importance = clamp01(fact.Importance, 0.5)
	fact.Origin = types.MemoryOriginManual
	fact.Status = types.MemoryStatusForOrigin(fact.Origin)
	fact.TripleHash = types.ComputeTripleHash(fact.Category, fact.Subject, fact.Predicate, fact.Object)
	fact.NormalizedKey = types.ComputeNormalizedKey(fact.Category, fact.Subject, fact.Predicate)

	existing, err := s.memoryRepo.GetFactByTripleHash(ctx, tenantID, userID, fact.TripleHash)
	if err != nil {
		return err
	}
	if existing != nil {
		// (租户, 用户, triple_hash) 上有唯一索引：完全相同的记忆已存在，
		// 让用户去编辑而不是留一条重复行。
		return types.ErrMemoryConflict
	}

	// 语义向量尽力而为：失败不影响创建，召回退化为时间/重要度排序。
	if emb := s.resolveEmbedder(ctx, ""); emb != nil {
		if vec, embErr := emb.Embed(ctx, fact.Content); embErr == nil {
			fact.Embedding = types.VectorBlob(vec)
		}
	}

	conflict := s.detectConflicts(ctx, fact)
	if err := s.memoryRepo.CreateFact(ctx, fact); err != nil {
		if errors.Is(err, types.ErrMemoryWriteFenced) {
			logger.Debugf(ctx, "[Memory] manual fact dropped by clear fence (user %s)", userID)
			return nil
		}
		return err
	}
	s.applySupersession(ctx, fact, conflict)
	s.enforceMemoryCapacity(ctx, tenantID, userID)
	return nil
}

// ConfirmFacts 逐条或批量确认待确认记忆（FR-02 规则 5）。单条状态冲突不阻塞
// 其余条目，冲突 ID 回传到 Failed 供前端提示。
func (s *memoryService) ConfirmFacts(ctx context.Context, ids []string) (*types.MemoryBatchResult, error) {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return nil, err
	}
	result := &types.MemoryBatchResult{Action: types.MemoryBatchActionConfirm, Requested: len(ids)}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if err := s.confirmOne(ctx, tenantID, userID, id); err != nil {
			if errors.Is(err, types.ErrMemoryConflict) || errors.Is(err, errMemoryFactNotFound) {
				result.Failed = append(result.Failed, id)
				continue
			}
			return result, err
		}
		result.Affected++
	}
	if result.Affected > 0 {
		// 确认会把 pending 转成 active，active 容量随之需要复核。
		s.enforceMemoryCapacity(ctx, tenantID, userID)
	}
	return result, nil
}

// confirmOne 执行 PRD §3.2 的 pending --用户确认--> active 转移，并在同一事务内
// 落地取代链（FR-03：pending 在确认前不取代 active）。
func (s *memoryService) confirmOne(ctx context.Context, tenantID uint64, userID, id string) error {
	fact, err := s.memoryRepo.GetFactByID(ctx, tenantID, userID, id)
	if err != nil {
		return err
	}
	if fact == nil {
		return errMemoryFactNotFound
	}
	if fact.Status != types.MemoryStatusPending {
		return types.ErrMemoryConflict
	}
	supersedeIDs := s.conflictIDsFor(ctx, fact)
	if err := s.memoryRepo.ConfirmFact(ctx, tenantID, userID, id, supersedeIDs); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errMemoryFactNotFound
		}
		return err
	}
	return nil
}

// RejectFacts 逐条或批量拒绝待确认记忆：pending --> archived（可恢复，FR-02 规则 5）。
func (s *memoryService) RejectFacts(ctx context.Context, ids []string) (*types.MemoryBatchResult, error) {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return nil, err
	}
	result := &types.MemoryBatchResult{Action: types.MemoryBatchActionReject, Requested: len(ids)}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		if err := s.memoryRepo.RejectFact(ctx, tenantID, userID, id); err != nil {
			if errors.Is(err, types.ErrMemoryConflict) || errors.Is(err, gorm.ErrRecordNotFound) {
				result.Failed = append(result.Failed, id)
				continue
			}
			return result, err
		}
		result.Affected++
	}
	return result, nil
}

// RestoreFact 恢复一条 archived / superseded 记忆为 active（PRD §3.2）。
// 恢复被取代的旧值时，当前 active 的冲突项转为 superseded（FR-03 规则 5）。
func (s *memoryService) RestoreFact(ctx context.Context, id string) error {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return err
	}
	fact, err := s.memoryRepo.GetFactByID(ctx, tenantID, userID, id)
	if err != nil {
		return err
	}
	if fact == nil {
		return errMemoryFactNotFound
	}
	if fact.Status != types.MemoryStatusArchived && fact.Status != types.MemoryStatusSuperseded {
		return types.ErrMemoryConflict
	}
	supersedeIDs := s.conflictIDsFor(ctx, fact)
	if err := s.memoryRepo.RestoreFact(ctx, tenantID, userID, id, supersedeIDs); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errMemoryFactNotFound
		}
		return err
	}
	s.enforceMemoryCapacity(ctx, tenantID, userID)
	return nil
}

// BatchUpdateFacts 批量确认/拒绝/归档/删除。targetUserID 非空时为工作区管理员
// 代成员操作，此时只允许归档与删除（PRD §5.1：确认/拒绝仅记忆归属用户本人）。
func (s *memoryService) BatchUpdateFacts(
	ctx context.Context, targetUserID, action string, ids []string,
) (*types.MemoryBatchResult, error) {
	tenantID, userID, err := memoryTargetScope(ctx, targetUserID)
	if err != nil {
		return nil, err
	}
	ids = normalizeIDs(ids)
	switch action {
	case types.MemoryBatchActionConfirm:
		if userID != types.SessionOwnerIDFromContext(ctx) {
			return nil, types.ErrMemoryForbidden
		}
		return s.ConfirmFacts(ctx, ids)
	case types.MemoryBatchActionReject:
		if userID != types.SessionOwnerIDFromContext(ctx) {
			return nil, types.ErrMemoryForbidden
		}
		return s.RejectFacts(ctx, ids)
	case types.MemoryBatchActionArchive:
		affected, err := s.memoryRepo.ArchiveFactsByIDs(ctx, tenantID, userID, ids, nil)
		if err != nil {
			return nil, err
		}
		return &types.MemoryBatchResult{
			Action: action, Requested: len(ids), Affected: affected,
		}, nil
	case types.MemoryBatchActionDelete:
		// PRD §3.2：用户删除 = 物理移除，不可逆。
		affected, err := s.memoryRepo.HardDeleteFacts(ctx, tenantID, userID, ids)
		if err != nil {
			return nil, err
		}
		return &types.MemoryBatchResult{
			Action: action, Requested: len(ids), Affected: affected,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported batch action %q", action)
	}
}

func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// GetFactChain 返回一条记忆的取代链：当前值、历史值与变更时间（FR-03 规则 4）。
func (s *memoryService) GetFactChain(ctx context.Context, id string) ([]*types.MemoryFact, error) {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return nil, err
	}
	fact, err := s.memoryRepo.GetFactByID(ctx, tenantID, userID, id)
	if err != nil {
		return nil, err
	}
	if fact == nil {
		return nil, errMemoryFactNotFound
	}
	key := fact.NormalizedKey
	if key == "" {
		key = types.ComputeNormalizedKey(fact.Category, fact.Subject, fact.Predicate)
	}
	if key == "" {
		return []*types.MemoryFact{fact}, nil
	}
	chain, err := s.memoryRepo.ListFactsByConflictKey(ctx, tenantID, userID, key, 50)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return []*types.MemoryFact{fact}, nil
	}
	// 当前值排在最前，历史值按取代时间（缺失时用更新时间）倒序。
	sort.SliceStable(chain, func(i, j int) bool {
		return chainRankWeight(chain[i]) > chainRankWeight(chain[j])
	})
	return chain, nil
}

// chainRankWeight 让 active 排在链首，其余按最近变更时间倒序。
func chainRankWeight(fact *types.MemoryFact) float64 {
	reference := fact.UpdatedAt
	if fact.SupersededAt != nil && fact.SupersededAt.After(reference) {
		reference = *fact.SupersededAt
	}
	weight := float64(reference.Unix())
	if fact.Status == types.MemoryStatusActive {
		weight += float64(1 << 32)
	}
	return weight
}

// GetStatusInfo 返回记忆管理页入口的状态摘要（含待确认角标，FR-02 结果）。
func (s *memoryService) GetStatusInfo(ctx context.Context) (*types.MemoryStatusInfo, error) {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return nil, err
	}
	cfg := s.memoryConfig(ctx, tenantID)
	counts, err := s.memoryRepo.CountFactsByStatus(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	info := &types.MemoryStatusInfo{
		Enabled:         s.IsEnabled(ctx, userID),
		WriteMode:       cfg.EffectiveWriteMode(),
		ActiveCount:     counts[types.MemoryStatusActive],
		PendingCount:    counts[types.MemoryStatusPending],
		SupersededCount: counts[types.MemoryStatusSuperseded],
		ArchivedCount:   counts[types.MemoryStatusArchived],
		MaxActiveItems:  cfg.EffectiveMaxActiveItems(),
		MaxPendingItems: cfg.EffectiveMaxPendingItems(),
	}
	info.FactCount = info.ActiveCount
	if gov, govErr := s.memoryRepo.GetGovernance(ctx, tenantID, userID); govErr == nil && gov != nil {
		info.ExtractionPaused = gov.ExtractionPaused()
		info.BlockedWrites = gov.BlockedWrites
	}
	return info, nil
}

// ListGovernanceNotices 返回尚未提示过的治理事件（FR-04 规则 5：一次性提示）。
func (s *memoryService) ListGovernanceNotices(ctx context.Context) ([]*types.MemoryGovernanceEvent, error) {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return nil, err
	}
	return s.memoryRepo.ListUnacknowledgedGovernanceEvents(ctx, tenantID, userID, 20)
}

// AcknowledgeGovernanceNotices 标记治理事件已提示，保证提示只出现一次。
func (s *memoryService) AcknowledgeGovernanceNotices(ctx context.Context, ids []int64) (int64, error) {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return 0, err
	}
	return s.memoryRepo.AcknowledgeGovernanceEvents(ctx, tenantID, userID, ids)
}

// ---------------------------------------------------------------------------
// FR-01 清空全部记忆
// ---------------------------------------------------------------------------

// DeleteAllForUser 清空当前用户的全部记忆，并在同一事务内推进清空围栏。
func (s *memoryService) DeleteAllForUser(ctx context.Context) (*types.MemoryClearResult, error) {
	tenantID, userID, err := memorySelfScope(ctx)
	if err != nil {
		return nil, err
	}
	return s.memoryRepo.ClearAllForUser(ctx, tenantID, userID)
}

// ClearMemoryForTarget 由工作区管理员清空成员记忆（PRD §5.1），同样触发围栏。
func (s *memoryService) ClearMemoryForTarget(
	ctx context.Context, targetUserID string,
) (*types.MemoryClearResult, error) {
	tenantID, userID, err := memoryTargetScope(ctx, targetUserID)
	if err != nil {
		return nil, err
	}
	return s.memoryRepo.ClearAllForUser(ctx, tenantID, userID)
}

// ---------------------------------------------------------------------------
// FR-03 矛盾判定与取代链
// ---------------------------------------------------------------------------

// memoryConflict 是一次矛盾判定的结果。
type memoryConflict struct {
	// certain 是确定矛盾的既有记忆 ID：应被新记忆取代。
	certain []string
	// ambiguous 是落在相似度阈值边界的既有记忆 ID：不自动取代，交用户裁决。
	ambiguous []string
}

// first 返回优先用于 replaces_id 的目标：确定矛盾优先于边界疑似矛盾。
func (c memoryConflict) first() string {
	if len(c.certain) > 0 {
		return c.certain[0]
	}
	if len(c.ambiguous) > 0 {
		return c.ambiguous[0]
	}
	return ""
}

// detectConflicts 复用召回侧的语义相似度能力判定矛盾（FR-03 规则 1）：
//   - 同一矛盾判定键（category|subject|predicate）出现不同取值 => 确定矛盾。
//     完全相同的三元组早已被 triple_hash 去重成 upsert，不会走到这里。
//   - 语义相似度 >= MemoryConflictSimilarityHigh => 确定矛盾。
//   - [Low, High) 之间 => 阈值边界，不自动取代（FR-03 异常）。
//
// 候选集取同类目的 active 记忆（召回同款查询，上限 MemoryRecallCandidateLimit）。
// 判定失败只记日志、不阻断写入：这是增强能力（PRD §5.3 规则 1）。
func (s *memoryService) detectConflicts(
	ctx context.Context, fact *types.MemoryFact,
) memoryConflict {
	var conflict memoryConflict
	key := fact.NormalizedKey
	if key == "" {
		key = types.ComputeNormalizedKey(fact.Category, fact.Subject, fact.Predicate)
	}
	candidates, err := s.memoryRepo.ListActiveFactsForRecall(
		ctx, fact.TenantID, fact.UserID, []string{fact.Category}, time.Time{}, types.MemoryRecallCandidateLimit)
	if err != nil {
		logger.Warnf(ctx, "[Memory] conflict detection load failed: %v", err)
		return conflict
	}
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == fact.ID {
			continue
		}
		candidateKey := candidate.NormalizedKey
		if candidateKey == "" {
			candidateKey = types.ComputeNormalizedKey(candidate.Category, candidate.Subject, candidate.Predicate)
		}
		if key != "" && candidateKey == key {
			conflict.certain = append(conflict.certain, candidate.ID)
			continue
		}
		if len(fact.Embedding) == 0 || len(candidate.Embedding) == 0 {
			continue
		}
		switch similarity := types.Cosine(fact.Embedding, candidate.Embedding); {
		case similarity >= types.MemoryConflictSimilarityHigh:
			conflict.certain = append(conflict.certain, candidate.ID)
		case similarity >= types.MemoryConflictSimilarityLow:
			conflict.ambiguous = append(conflict.ambiguous, candidate.ID)
		}
	}
	return conflict
}

// applySupersession 在一条 active 记忆落库后落地取代链（FR-03 规则 1/2）：
// 旧值置 superseded 并记录取代者与取代时间，不删除。pending 记忆不在这里取代
// 任何 active 项——PRD §3.2 中 pending 永不注入 prompt，取代要等用户确认。
func (s *memoryService) applySupersession(ctx context.Context, fact *types.MemoryFact, conflict memoryConflict) {
	if fact.Status != types.MemoryStatusActive {
		return
	}
	if len(conflict.certain) > 0 {
		if _, err := s.memoryRepo.SupersedeFacts(
			ctx, fact.TenantID, fact.UserID, conflict.certain, fact.ID); err != nil {
			logger.Warnf(ctx, "[Memory] supersede %d conflicts failed: %v", len(conflict.certain), err)
		}
	}
	if len(conflict.ambiguous) > 0 {
		// 阈值边界不确定：保留双方 active，避免系统误判压制正确记忆，
		// 由用户在记忆管理页裁决（FR-03 异常）。
		logger.Infof(ctx, "[Memory] %d ambiguous conflicts left for user adjudication (fact %s)",
			len(conflict.ambiguous), fact.ID)
	}
}

// conflictIDsFor 返回确认/恢复一条记忆时应被取代的 active 记忆 ID：
// 拟取代目标（replaces_id）+ 同矛盾键或语义确定矛盾的 active 项。
func (s *memoryService) conflictIDsFor(ctx context.Context, fact *types.MemoryFact) []string {
	ids := make([]string, 0, 2)
	if fact.ReplacesID != "" && fact.ReplacesID != fact.ID {
		ids = append(ids, fact.ReplacesID)
	}
	for _, id := range s.detectConflicts(ctx, fact).certain {
		if id != fact.ID {
			ids = append(ids, id)
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// FR-04 容量治理
// ---------------------------------------------------------------------------

// enforceMemoryCapacity 在每次写入后复核 active 与 pending 两类容量。
// 两类上限互相独立：待确认超限只归档最旧 pending，绝不触及 active（FR-02 规则 6）。
func (s *memoryService) enforceMemoryCapacity(ctx context.Context, tenantID uint64, userID string) {
	cfg := s.memoryConfig(ctx, tenantID)
	s.trimPendingCapacity(ctx, tenantID, userID, cfg.EffectiveMaxPendingItems())
	s.trimActiveCapacity(ctx, tenantID, userID, cfg.EffectiveMaxActiveItems())
}

// trimPendingCapacity 归档超出上限的最旧待确认项。
func (s *memoryService) trimPendingCapacity(ctx context.Context, tenantID uint64, userID string, limit int) {
	trimmed, err := s.memoryRepo.TrimPendingFacts(ctx, tenantID, userID, limit)
	if err != nil {
		s.handleArchiveFailure(ctx, tenantID, userID,
			fmt.Sprintf("待确认容量归档失败（上限 %d 条）", limit), err)
		return
	}
	if len(trimmed) == 0 {
		return
	}
	ids := make([]string, 0, len(trimmed))
	contents := make([]string, 0, len(trimmed))
	for _, fact := range trimmed {
		ids = append(ids, fact.ID)
		if len(contents) < memoryGovernanceNoticeItemLimit {
			contents = append(contents, fact.Content)
		}
	}
	s.recordGovernanceEvent(ctx, tenantID, userID, types.MemoryEventPendingTrimmed,
		fmt.Sprintf("待确认记忆超过上限 %d 条，已归档最旧的 %d 条（可在「已归档」分组恢复）", limit, len(trimmed)),
		ids, contents)
}

// trimActiveCapacity 把 active 超限部分按排名归档（FR-04 规则 1/2/4）。
// 排名口径见 types.MemoryRetentionRank：与召回评分同源，避免"召回认为重要、
// 容量认为不重要"的矛盾。归档是可逆的，用户可在「已归档」分组恢复。
func (s *memoryService) trimActiveCapacity(ctx context.Context, tenantID uint64, userID string, limit int) {
	counts, err := s.memoryRepo.CountFactsByStatus(ctx, tenantID, userID)
	if err != nil {
		logger.Warnf(ctx, "[Memory] count facts by status failed: %v", err)
		return
	}
	active := counts[types.MemoryStatusActive]
	if int(active) <= limit {
		return
	}
	ranks, err := s.memoryRepo.ListActiveFactRanks(ctx, tenantID, userID)
	if err != nil {
		s.handleArchiveFailure(ctx, tenantID, userID,
			fmt.Sprintf("容量归档失败（上限 %d 条）", limit), err)
		return
	}
	now := time.Now()
	for i := range ranks {
		ranks[i].Rank = types.MemoryRetentionRank(
			ranks[i].Importance, ranks[i].Confidence, ranks[i].AccessCount, ranks[i].ReferenceTime(), now)
	}
	// 排名最低者先归档。
	sort.Slice(ranks, func(i, j int) bool { return ranks[i].Rank < ranks[j].Rank })

	excess := int(active) - limit
	if excess > len(ranks) {
		excess = len(ranks)
	}
	if excess <= 0 {
		return
	}
	victims := ranks[:excess]
	ids := make([]string, 0, len(victims))
	contents := make([]string, 0, len(victims))
	for _, rank := range victims {
		ids = append(ids, rank.ID)
		if len(contents) < memoryGovernanceNoticeItemLimit {
			contents = append(contents, rank.Content)
		}
	}
	archived, err := s.memoryRepo.ArchiveFactsByIDs(ctx, tenantID, userID, ids,
		[]string{types.MemoryStatusActive})
	if err != nil {
		// 归档失败不阻塞新写入：记录待重试，下一次写入会再试（FR-04 异常）。
		s.handleArchiveFailure(ctx, tenantID, userID,
			fmt.Sprintf("容量归档失败（上限 %d 条，待归档 %d 条）", limit, excess), err)
		return
	}
	_ = s.memoryRepo.ResetArchiveFailures(ctx, tenantID, userID)
	s.recordGovernanceEvent(ctx, tenantID, userID, types.MemoryEventCapacityArchived,
		fmt.Sprintf("生效记忆超过上限 %d 条，已按排名归档最低的 %d 条（可在「已归档」分组恢复）", limit, archived),
		ids, contents)
	logger.Infof(ctx, "[Memory] archived %d lowest-ranked facts for user %s (cap %d)", archived, userID, limit)
}

// handleArchiveFailure 记录一次归档失败：累加连续失败计数，达阈值时暂停自动
// 抽取并通过审计日志通知管理员（FR-04 异常）。
func (s *memoryService) handleArchiveFailure(
	ctx context.Context, tenantID uint64, userID, reason string, cause error,
) {
	logger.Warnf(ctx, "[Memory] %s: %v", reason, cause)
	gov, err := s.memoryRepo.RecordArchiveFailure(ctx, tenantID, userID)
	if err != nil {
		logger.Warnf(ctx, "[Memory] record archive failure failed: %v", err)
		return
	}
	s.recordGovernanceEvent(ctx, tenantID, userID, types.MemoryEventArchiveFailed,
		reason+"，已记录待重试", nil, nil)
	if gov == nil || gov.ArchiveFailures != types.MemoryArchiveFailureThreshold {
		return
	}
	s.recordGovernanceEvent(ctx, tenantID, userID, types.MemoryEventExtractionPaused,
		fmt.Sprintf("容量归档连续失败 %d 次，已暂停该用户的自动记忆抽取，待归档恢复后自动解除",
			gov.ArchiveFailures), nil, nil)
	s.writeAudit(ctx, &types.AuditLog{
		TenantID:     tenantID,
		Action:       types.AuditActionMemoryExtractionPaused,
		Outcome:      types.AuditOutcomeFailed,
		ScopeType:    "memory",
		ScopeID:      userID,
		TargetType:   "memory_user",
		TargetID:     userID,
		TargetUserID: userID,
		Details: types.NewJSON(map[string]any{
			"reason":           reason,
			"archive_failures": gov.ArchiveFailures,
		}),
	})
	logger.Errorf(ctx, "[Memory] auto extraction paused for user %s after %d consecutive archive failures",
		userID, gov.ArchiveFailures)
}

// recordGovernanceEvent 写入一条治理事件（记忆管理页的一次性提示）。
// 事件写入失败不影响主流程：提示属于增强能力（PRD §5.3 规则 1）。
func (s *memoryService) recordGovernanceEvent(
	ctx context.Context, tenantID uint64, userID, kind, reason string, ids, contents []string,
) {
	event := &types.MemoryGovernanceEvent{
		TenantID:     tenantID,
		UserID:       userID,
		Kind:         kind,
		Reason:       reason,
		ItemCount:    len(ids),
		ItemIDs:      types.StringArray(ids),
		ItemContents: types.StringArray(contents),
	}
	if err := s.memoryRepo.CreateGovernanceEvent(ctx, event); err != nil {
		logger.Warnf(ctx, "[Memory] record governance event %s failed: %v", kind, err)
	}
}

// writeAudit 落一条审计日志（PRD §5.2）。审计服务未注入或写入失败都不阻断业务。
func (s *memoryService) writeAudit(ctx context.Context, entry *types.AuditLog) {
	if s.audit == nil || entry == nil {
		return
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if err := s.audit.Log(ctx, entry); err != nil {
		logger.Warnf(ctx, "[Memory] audit log write failed: %v", err)
	}
}
