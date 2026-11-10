package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// ---------------------------------------------------------------------------
// FR-09 记忆管理控制台（管理员侧）
//
// 准入：仅工作区管理员（Admin / Owner）。路由层已挂 g.Admin()，这里再校验一次，
// 因为 RequireRole 对 API Key 主体是短路的（机器主体由 APIKeyGate 单独授权），
// 而控制台没有"机器管理员"这个概念——上游 memoryAdminCaller 出于同样的理由拒绝
// API Key 身份，这里照搬。
//
// 可见面：只有聚合计数。批量归档 / 批量删除待确认项复用既有治理原语，
// 不重写任何删除逻辑，因此 FR-01 围栏、FR-02 状态机的约束一处生效、处处生效。
// ---------------------------------------------------------------------------

// memoryAdminCaller 校验调用者是本工作区的管理员，返回 (租户, 操作者)。
func memoryAdminCaller(ctx context.Context) (uint64, string, error) {
	// API Key 身份没有工作区角色语义，一律拒绝（PRD §5.1：仅工作区管理员）。
	if _, ok := types.TenantAPIKeyScopeFromContext(ctx); ok {
		return 0, "", types.ErrMemoryForbidden
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return 0, "", errMemoryScopeUnavailable
	}
	actorID := strings.TrimSpace(types.SessionOwnerIDFromContext(ctx))
	if actorID == "" {
		return 0, "", errMemoryScopeUnavailable
	}
	if !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin) {
		return 0, "", types.ErrMemoryForbidden
	}
	return tenantID, actorID, nil
}

// memoryAdminTargetScope 在管理员身份之上解析被操作成员的作用域。
// userID 必填：管理员批量操作必须指名目标，不接受"作用于调用者自己"的隐式语义
// （memoryTargetScope 的空值分支是给本人操作留的，控制台不走那条路）。
func memoryAdminTargetScope(ctx context.Context, userID string) (uint64, string, error) {
	if _, _, err := memoryAdminCaller(ctx); err != nil {
		return 0, "", err
	}
	targetID := strings.TrimSpace(userID)
	if targetID == "" {
		return 0, "", fmt.Errorf("%w: user_id is required", types.ErrMemoryAdminBadRequest)
	}
	// memoryTargetScope 内部会再校验一次管理员角色，双重保险。
	tenantID, scopedID, err := memoryTargetScope(ctx, targetID)
	if err != nil {
		return 0, "", err
	}
	return tenantID, scopedID, nil
}

// AdminMemoryOverview 返回本工作区的记忆聚合总览（FR-09 规则 1）。
func (s *memoryService) AdminMemoryOverview(ctx context.Context) (*types.MemoryAdminOverview, error) {
	tenantID, _, err := memoryAdminCaller(ctx)
	if err != nil {
		return nil, err
	}
	byStatus, err := s.memoryRepo.CountTenantFactsByStatus(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	byCategory, err := s.memoryRepo.CountTenantFactsByCategory(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	userCount, err := s.memoryRepo.CountTenantMemoryUsers(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	overview := &types.MemoryAdminOverview{
		TenantID:    tenantID,
		ByStatus:    byStatus,
		ByCategory:  byCategory,
		UserCount:   userCount,
		GeneratedAt: time.Now().UTC(),
	}
	// 总量取各状态之和而不是再跑一条 COUNT：两者口径必然一致，也少一次往返。
	for _, count := range byStatus {
		overview.Total += count
	}
	cfg := s.memoryConfig(ctx, tenantID)
	overview.WriteMode = cfg.EffectiveWriteMode()
	overview.MaxActiveItems = cfg.EffectiveMaxActiveItems()
	overview.MaxPendingItems = cfg.EffectiveMaxPendingItems()
	return overview, nil
}

// AdminListMemoryUsers 按用户维度分页返回记忆规模（FR-09 规则 2）。
// 规模 desc 排序，异常增长的成员自然浮到第一页。
func (s *memoryService) AdminListMemoryUsers(
	ctx context.Context, limit, offset int,
) ([]*types.MemoryUserSummary, int64, error) {
	tenantID, _, err := memoryAdminCaller(ctx)
	if err != nil {
		return nil, 0, err
	}
	limit, offset = types.MemoryAdminPage(limit, offset)
	return s.memoryRepo.ListTenantMemoryUsers(ctx, tenantID, limit, offset)
}

// AdminGetMemoryUser 返回单个成员的规模明细：状态计数 + 类型分布 + 治理状态。
//
// 该成员在本工作区没有任何记忆时返回 types.ErrMemoryAdminTargetNotFound；
// 由于查询始终带 tenant_id，其他工作区的成员也走同一个分支，handler 因此能对
// "不存在"与"不属于本工作区"返回同一个 404（FR-09 规则 4 防探测）。
func (s *memoryService) AdminGetMemoryUser(
	ctx context.Context, userID string,
) (*types.MemoryAdminUserDetail, error) {
	tenantID, targetID, err := memoryAdminTargetScope(ctx, userID)
	if err != nil {
		return nil, err
	}
	summary, err := s.memoryRepo.GetTenantMemoryUser(ctx, tenantID, targetID)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, types.ErrMemoryAdminTargetNotFound
	}
	byCategory, err := s.memoryRepo.CountUserFactsByCategory(ctx, tenantID, targetID)
	if err != nil {
		return nil, err
	}

	detail := &types.MemoryAdminUserDetail{
		MemoryUserSummary: *summary,
		ByCategory:        byCategory,
	}
	cfg := s.memoryConfig(ctx, tenantID)
	detail.WriteMode = cfg.EffectiveWriteMode()
	detail.MaxActiveItems = cfg.EffectiveMaxActiveItems()
	detail.MaxPendingItems = cfg.EffectiveMaxPendingItems()
	// 治理状态读不到不阻断明细：这是诊断信息（PRD §5.3 规则 1），
	// 规模计数才是控制台的主数据。
	gov, govErr := s.memoryRepo.GetGovernance(ctx, tenantID, targetID)
	if govErr != nil {
		logger.Warnf(ctx, "[Memory] admin read governance of %s failed: %v", targetID, govErr)
	} else if gov != nil {
		detail.ExtractionPaused = gov.ExtractionPaused()
		detail.BlockedWrites = gov.BlockedWrites
	}
	return detail, nil
}

// AdminBatchArchiveFacts 批量归档成员记忆（FR-09 规则 3）。
// 归档是可逆的：条目转为 archived，成员仍可在「已归档」分组恢复。
func (s *memoryService) AdminBatchArchiveFacts(
	ctx context.Context, userID string, ids []string,
) (*types.MemoryAdminBatchResponse, error) {
	_, targetID, err := memoryAdminTargetScope(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids = normalizeIDs(ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: ids is required", types.ErrMemoryAdminBadRequest)
	}
	if len(ids) > types.MemoryAdminBatchLimit {
		return nil, fmt.Errorf("%w: at most %d ids per request",
			types.ErrMemoryAdminBadRequest, types.MemoryAdminBatchLimit)
	}
	// 复用既有批量原语（内部再走一次 memoryTargetScope + ArchiveFactsByIDs），
	// 不在控制台里另写一套归档逻辑。
	result, err := s.BatchUpdateFacts(ctx, targetID, types.MemoryBatchActionArchive, ids)
	if err != nil {
		return nil, err
	}
	return &types.MemoryAdminBatchResponse{
		Action:    types.MemoryBatchActionArchive,
		UserID:    targetID,
		Requested: result.Requested,
		Affected:  result.Affected,
	}, nil
}

// AdminBatchDeletePendingFacts 批量删除待确认项（FR-09 规则 3）。
//
// 物理删除、不可逆，因此作用范围被钉死在 status=pending：先按状态过滤出真正
// 可删的 ID，再交给既有的 HardDeleteFacts。请求里混入的 active / archived /
// superseded 条目一律落进 Skipped，绝不会被删除（FR-02 规则 6：待确认裁剪绝不
// 触及 active）。ids 为空表示该成员的全部待确认项，仍受 MemoryAdminBatchLimit 约束。
func (s *memoryService) AdminBatchDeletePendingFacts(
	ctx context.Context, userID string, ids []string,
) (*types.MemoryAdminBatchResponse, error) {
	tenantID, targetID, err := memoryAdminTargetScope(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids = normalizeIDs(ids)
	if len(ids) > types.MemoryAdminBatchLimit {
		return nil, fmt.Errorf("%w: at most %d ids per request",
			types.ErrMemoryAdminBadRequest, types.MemoryAdminBatchLimit)
	}
	pendingIDs, err := s.memoryRepo.ListFactIDsByStatus(
		ctx, tenantID, targetID, types.MemoryStatusPending, ids, types.MemoryAdminBatchLimit)
	if err != nil {
		return nil, err
	}
	affected, err := s.memoryRepo.HardDeleteFacts(ctx, tenantID, targetID, pendingIDs)
	if err != nil {
		return nil, err
	}

	requested := len(ids)
	if requested == 0 {
		// 不指定 ID 时，"请求范围"就是解析出的全部待确认项。
		requested = len(pendingIDs)
	}
	return &types.MemoryAdminBatchResponse{
		Action:    types.MemoryBatchActionDelete,
		UserID:    targetID,
		Requested: requested,
		Affected:  affected,
		Skipped:   skippedPendingIDs(ids, pendingIDs),
	}, nil
}

// skippedPendingIDs 返回请求里没被判定为 pending 的 ID：这些条目被拒绝删除，
// 回传给管理员以便解释"为什么请求了 N 条只删掉 M 条"。
func skippedPendingIDs(requested, pending []string) []string {
	if len(requested) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(pending))
	for _, id := range pending {
		allowed[id] = struct{}{}
	}
	skipped := make([]string, 0, len(requested)-len(pending))
	for _, id := range requested {
		if _, ok := allowed[id]; !ok {
			skipped = append(skipped, id)
		}
	}
	return skipped
}
