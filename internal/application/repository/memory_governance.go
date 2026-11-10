package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---------------------------------------------------------------------------
// FR-01 清空时序围栏
// ---------------------------------------------------------------------------

// lockGovernanceRow 在事务内读取治理行；Postgres/MySQL 上加行锁，SQLite 无行级锁
// （写事务本身已串行化），因此按方言降级为普通读取。
func lockGovernanceRow(tx *gorm.DB, tenantID uint64, userID string) (*types.MemoryGovernance, error) {
	query := tx.Where("tenant_id = ? AND user_id = ?", tenantID, userID)
	switch tx.Dialector.Name() {
	case "postgres", "mysql":
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var gov types.MemoryGovernance
	if err := query.First(&gov).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &gov, nil
}

// GetGovernance 读取 (租户, 用户) 的记忆治理状态；从未清空过且无失败计数时返回 nil。
func (r *memoryRepository) GetGovernance(
	ctx context.Context, tenantID uint64, userID string,
) (*types.MemoryGovernance, error) {
	var gov types.MemoryGovernance
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		First(&gov).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &gov, nil
}

// FenceBlocked 判断一次写入是否会被清空围栏拦截（FR-01 规则 3/4）。
//
// 双机制校验：
//  1. 世代令牌——后台任务入队时捕获 generation，落库时比对；不符说明期间发生过
//     清空，任务携带的推断整体失效（in-flight 拦截）。
//  2. 时间截止点——本次写入所依据的对话时间早于 cutoff 即拦截。
//
// 时间比较全部在 Go 侧用 time.Time.Before 完成（瞬时语义，与调用方时区无关）：
// SQLite 按原始偏移存时间戳，SQL 文本比较会把更新的 UTC 截止点判成更早。
// 仓储层的每个写路径与后台任务的落库前预检查共用本方法，避免两套判定漂移。
// 命中拦截时累加 blocked_writes 计数（FR-01 规则 5：计数并记录，不报错）。
func (r *memoryRepository) FenceBlocked(
	ctx context.Context, tenantID uint64, userID string, conversationAt time.Time, generation string,
) (bool, error) {
	gov, err := r.GetGovernance(ctx, tenantID, userID)
	if err != nil {
		return false, err
	}
	if gov == nil {
		return false, nil
	}
	if conversationAt.IsZero() {
		conversationAt = time.Now()
	}
	// 用 Unix 纳秒整数比较：与调用方时区无关，也不受数据库时间列精度影响。
	blocked := (generation != "" && gov.Generation != "" && generation != gov.Generation) ||
		(gov.CutoffNano > 0 && conversationAt.UnixNano() < gov.CutoffNano)
	if blocked {
		r.countBlockedWrite(ctx, tenantID, userID)
	}
	return blocked, nil
}

// checkWriteFence 是所有记忆写路径的唯一收口（FR-01）。被拦截时返回
// types.ErrMemoryWriteFenced，服务层静默吞掉（用户已表达删除意图，不报错）。
// 读取围栏失败按"正确性能力失败必须阻断"处理（PRD §5.3 规则 2）：宁可不写，
// 也不能把用户已清空的记忆写回去。
func (r *memoryRepository) checkWriteFence(
	ctx context.Context, tenantID uint64, userID string, conversationAt time.Time,
) error {
	fence, _ := types.MemoryWriteFenceFromContext(ctx)
	if fence.ConversationAt.IsZero() {
		fence.ConversationAt = conversationAt
	}
	blocked, err := r.FenceBlocked(ctx, tenantID, userID, fence.ConversationAt, fence.Generation)
	if err != nil {
		return err
	}
	if blocked {
		return types.ErrMemoryWriteFenced
	}
	return nil
}

// countBlockedWrite 累加围栏拦截计数（诊断指标，FR-01 规则 5）。尽力而为：
// 计数失败不影响已经做出的拦截决定。
func (r *memoryRepository) countBlockedWrite(ctx context.Context, tenantID uint64, userID string) {
	_ = r.db.WithContext(ctx).Model(&types.MemoryGovernance{}).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Updates(map[string]interface{}{
			"blocked_writes": gorm.Expr("blocked_writes + 1"),
			"updated_at":     time.Now().UTC(),
		}).Error
}

// ClearAllForUser 在同一个事务内物理清空该用户的全部记忆并推进清空围栏。
//
// 顺序是刻意的：先写围栏再删数据，围栏写入失败则整个事务回滚，不会出现
// "删了但没围栏"的中间态（PRD FR-01 异常表 + §5.3 规则 2）。
// 截止点单调不回退——取 max(now, 旧 cutoff)，同一时钟刻度或时钟回拨时按逻辑
// 递增（+1ms）推进，因此连续两次清空的第二次严格晚于第一次。
func (r *memoryRepository) ClearAllForUser(
	ctx context.Context, tenantID uint64, userID string,
) (*types.MemoryClearResult, error) {
	var result types.MemoryClearResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		gov, err := lockGovernanceRow(tx, tenantID, userID)
		if err != nil {
			return err
		}
		// 截止点单调不回退：同一时钟刻度或时钟回拨时按逻辑递增（+1ms）推进，
		// 因此连续两次清空的第二次严格晚于第一次。
		cutoffNano := now.UnixNano()
		if gov != nil && gov.CutoffNano > 0 && cutoffNano <= gov.CutoffNano {
			cutoffNano = gov.CutoffNano + int64(time.Millisecond)
		}
		generation := uuid.NewString()
		if gov == nil {
			gov = &types.MemoryGovernance{TenantID: tenantID, UserID: userID, CreatedAt: now}
		}
		gov.Generation = generation
		gov.CutoffNano = cutoffNano
		gov.UpdatedAt = now
		if err := tx.Save(gov).Error; err != nil {
			return err
		}

		// PRD §3.2：用户清空全部记忆 = 物理移除（不可逆），因此用 Unscoped
		// 越过软删除，避免残留行仍可被恢复。
		facts := tx.Unscoped().
			Where("tenant_id = ? AND user_id = ?", tenantID, userID).
			Delete(&types.MemoryFact{})
		if facts.Error != nil {
			return facts.Error
		}
		summaries := tx.Unscoped().
			Where("tenant_id = ? AND user_id = ?", tenantID, userID).
			Delete(&types.MemorySessionSummary{})
		if summaries.Error != nil {
			return summaries.Error
		}
		// 治理事件是记忆的派生数据，一并清除。
		if err := tx.Where("tenant_id = ? AND user_id = ?", tenantID, userID).
			Delete(&types.MemoryGovernanceEvent{}).Error; err != nil {
			return err
		}

		result = types.MemoryClearResult{
			DeletedFacts:     facts.RowsAffected,
			DeletedSummaries: summaries.RowsAffected,
			Generation:       generation,
			CutoffAt:         time.Unix(0, cutoffNano).UTC(),
			BlockedWrites:    gov.BlockedWrites,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// EnsureGovernance 保证 (租户, 用户) 存在一行治理状态并返回它；不存在时以一个新的
// 世代令牌创建。后台任务入队时调用，使每个在途任务都携带世代令牌——这是拦截
// in-flight 写入的主机制（时间截止点受时钟精度限制，只能作为兜底）。
func (r *memoryRepository) EnsureGovernance(
	ctx context.Context, tenantID uint64, userID string,
) (*types.MemoryGovernance, error) {
	gov := &types.MemoryGovernance{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockGovernanceRow(tx, tenantID, userID)
		if err != nil {
			return err
		}
		if row == nil {
			now := time.Now().UTC()
			row = &types.MemoryGovernance{
				TenantID: tenantID, UserID: userID,
				Generation: uuid.NewString(), CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Save(row).Error; err != nil {
				return err
			}
		}
		*gov = *row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return gov, nil
}

// ---------------------------------------------------------------------------
// FR-02 确认门控 / FR-03 取代链：状态转移原语
// ---------------------------------------------------------------------------

// ConfirmFact 把一条待确认记忆转为 active（PRD §3.2：pending --用户确认--> active）。
// supersedeIDs 是服务层判定出的、应被本条取代的记忆，在同一事务内置为 superseded
// 并记录取代者与取代时间（FR-03 规则 2）——pending 在确认前不取代 active。
func (r *memoryRepository) ConfirmFact(
	ctx context.Context, tenantID uint64, userID, id string, supersedeIDs []string,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := r.scopeUser(tx.Model(&types.MemoryFact{}), tenantID, userID).
			Where("id = ? AND status = ?", id, types.MemoryStatusPending).
			Updates(map[string]interface{}{
				"status":     types.MemoryStatusActive,
				"updated_at": time.Now(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return r.requireFactExists(tx, tenantID, userID, id)
		}
		return r.supersedeInTx(tx, tenantID, userID, supersedeIDs, id)
	})
}

// RejectFact 把一条待确认记忆归档（PRD §3.2：pending --用户拒绝--> archived，可恢复）。
func (r *memoryRepository) RejectFact(ctx context.Context, tenantID uint64, userID, id string) error {
	return r.transitionFact(ctx, tenantID, userID, id,
		[]string{types.MemoryStatusPending}, types.MemoryStatusArchived, nil)
}

// RestoreFact 恢复一条 archived / superseded 记忆为 active（PRD §3.2）。
// supersedeIDs 是与之冲突的当前 active 项：恢复旧值意味着新值被取代
// （FR-03 规则 5：原 active 项转为 superseded）。
func (r *memoryRepository) RestoreFact(
	ctx context.Context, tenantID uint64, userID, id string, supersedeIDs []string,
) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := r.scopeUser(tx.Model(&types.MemoryFact{}), tenantID, userID).
			Where("id = ? AND status IN ?", id,
				[]string{types.MemoryStatusArchived, types.MemoryStatusSuperseded}).
			Updates(map[string]interface{}{
				"status":     types.MemoryStatusActive,
				"updated_at": time.Now(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return r.requireFactExists(tx, tenantID, userID, id)
		}
		return r.supersedeInTx(tx, tenantID, userID, supersedeIDs, id)
	})
}

// transitionFact 按状态机做一次受约束的状态转移；当前状态不在 from 中即冲突。
func (r *memoryRepository) transitionFact(
	ctx context.Context, tenantID uint64, userID, id string,
	from []string, to string, extra map[string]interface{},
) error {
	updates := map[string]interface{}{"status": to, "updated_at": time.Now()}
	for k, v := range extra {
		updates[k] = v
	}
	res := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Where("id = ? AND status IN ?", id, from).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return r.requireFactExistsScope(ctx, tenantID, userID, id)
	}
	return nil
}

// requireFactExists 在事务内区分"记忆不存在"与"状态机拒绝该转移"。
func (r *memoryRepository) requireFactExists(
	tx *gorm.DB, tenantID uint64, userID, id string,
) error {
	var count int64
	if err := r.scopeUser(tx.Model(&types.MemoryFact{}), tenantID, userID).
		Where("id = ?", id).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return gorm.ErrRecordNotFound
	}
	return types.ErrMemoryConflict
}

func (r *memoryRepository) requireFactExistsScope(
	ctx context.Context, tenantID uint64, userID, id string,
) error {
	return r.requireFactExists(r.db.WithContext(ctx), tenantID, userID, id)
}

// supersedeInTx 把一批记忆置为 superseded，记录取代者与取代时间；不删除原文。
func (r *memoryRepository) supersedeInTx(
	tx *gorm.DB, tenantID uint64, userID string, ids []string, supersededBy string,
) error {
	ids = filterEmptyAndSelf(ids, supersededBy)
	if len(ids) == 0 {
		return nil
	}
	now := time.Now()
	return r.scopeUser(tx.Model(&types.MemoryFact{}), tenantID, userID).
		Where("id IN ? AND status IN ?", ids,
			[]string{types.MemoryStatusActive, types.MemoryStatusPending}).
		Updates(map[string]interface{}{
			"status":        types.MemoryStatusSuperseded,
			"superseded_by": supersededBy,
			"superseded_at": now,
			"updated_at":    now,
		}).Error
}

// SupersedeFacts 把一批记忆置为 superseded（FR-03 规则 1/2：保留原文与取代时间）。
func (r *memoryRepository) SupersedeFacts(
	ctx context.Context, tenantID uint64, userID string, ids []string, supersededBy string,
) (int64, error) {
	ids = filterEmptyAndSelf(ids, supersededBy)
	if len(ids) == 0 {
		return 0, nil
	}
	now := time.Now()
	res := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Where("id IN ? AND status IN ?", ids,
			[]string{types.MemoryStatusActive, types.MemoryStatusPending}).
		Updates(map[string]interface{}{
			"status":        types.MemoryStatusSuperseded,
			"superseded_by": supersededBy,
			"superseded_at": now,
			"updated_at":    now,
		})
	return res.RowsAffected, res.Error
}

func filterEmptyAndSelf(ids []string, self string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || id == self {
			continue
		}
		out = append(out, id)
	}
	return out
}

// ArchiveFactsByIDs 把一批记忆归档（FR-04 规则 4：可逆）。fromStatuses 为空表示
// 不限制来源状态；容量治理传 [active]、待确认裁剪传 [pending]，两者互不触及
// （FR-02 规则 6）。
func (r *memoryRepository) ArchiveFactsByIDs(
	ctx context.Context, tenantID uint64, userID string, ids []string, fromStatuses []string,
) (int64, error) {
	ids = filterEmptyAndSelf(ids, "")
	if len(ids) == 0 {
		return 0, nil
	}
	query := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Where("id IN ?", ids)
	if len(fromStatuses) > 0 {
		query = query.Where("status IN ?", fromStatuses)
	}
	res := query.Updates(map[string]interface{}{
		"status":     types.MemoryStatusArchived,
		"updated_at": time.Now(),
	})
	return res.RowsAffected, res.Error
}

// HardDeleteFacts 物理删除一批记忆（PRD §3.2：用户删除 = 物理移除，不可逆）。
func (r *memoryRepository) HardDeleteFacts(
	ctx context.Context, tenantID uint64, userID string, ids []string,
) (int64, error) {
	ids = filterEmptyAndSelf(ids, "")
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("id IN ?", ids).
		Unscoped().
		Delete(&types.MemoryFact{})
	return res.RowsAffected, res.Error
}

// ListFactsByConflictKey 返回同一矛盾判定键下的全部记忆（任意状态），用于记忆
// 详情页展示取代链：当前值、历史值、变更时间（FR-03 规则 4）。
func (r *memoryRepository) ListFactsByConflictKey(
	ctx context.Context, tenantID uint64, userID, normalizedKey string, limit int,
) ([]*types.MemoryFact, error) {
	if normalizedKey == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = types.MemoryRecallCandidateLimit
	}
	facts := make([]*types.MemoryFact, 0)
	err := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("normalized_key = ?", normalizedKey).
		Order("created_at DESC").
		Limit(limit).
		Find(&facts).Error
	if err != nil {
		return nil, err
	}
	return facts, nil
}

// ---------------------------------------------------------------------------
// FR-04 容量治理
// ---------------------------------------------------------------------------

// CountFactsByStatus 按状态统计记忆条数（待确认角标、容量判定共用一次分组查询）。
func (r *memoryRepository) CountFactsByStatus(
	ctx context.Context, tenantID uint64, userID string,
) (map[string]int64, error) {
	var rows []struct {
		Status string `gorm:"column:status"`
		Count  int64  `gorm:"column:count"`
	}
	err := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Select("status, COUNT(*) as count").
		Group("status").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Status] = row.Count
	}
	return out, nil
}

// ListActiveFactRanks 返回容量治理排名所需的最小投影（不含 embedding）。
// 排名本身在 Go 侧用 types.MemoryRetentionRank 计算：口径与召回评分同源，
// 且 SQLite 与 Postgres 都没有稳定的 ln/exp 表达式可用。
func (r *memoryRepository) ListActiveFactRanks(
	ctx context.Context, tenantID uint64, userID string,
) ([]types.MemoryFactRank, error) {
	ranks := make([]types.MemoryFactRank, 0)
	err := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Select("id, category, content, importance, confidence, access_count, "+
			"last_accessed_at, updated_at, created_at").
		Where("status = ?", types.MemoryStatusActive).
		Limit(types.MemoryMaxItemsHardCap).
		Find(&ranks).Error
	if err != nil {
		return nil, err
	}
	return ranks, nil
}

// TrimPendingFacts 归档超出上限的最旧待确认项，返回被归档的条目用于一次性提示。
// 只碰 pending，绝不触及 active（FR-02 规则 6 / FR-04 规则 3）。
func (r *memoryRepository) TrimPendingFacts(
	ctx context.Context, tenantID uint64, userID string, limit int,
) ([]*types.MemoryFact, error) {
	if limit < 0 {
		limit = 0
	}
	var count int64
	if err := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Where("status = ?", types.MemoryStatusPending).Count(&count).Error; err != nil {
		return nil, err
	}
	if count <= int64(limit) {
		return nil, nil
	}
	excess := make([]*types.MemoryFact, 0)
	// created_at ASC = 最旧者优先归档。
	if err := r.scopeUser(r.db.WithContext(ctx), tenantID, userID).
		Where("status = ?", types.MemoryStatusPending).
		Order("created_at ASC").Order("id ASC").
		Limit(int(count - int64(limit))).
		Find(&excess).Error; err != nil {
		return nil, err
	}
	if len(excess) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(excess))
	for _, fact := range excess {
		ids = append(ids, fact.ID)
	}
	if _, err := r.ArchiveFactsByIDs(ctx, tenantID, userID, ids,
		[]string{types.MemoryStatusPending}); err != nil {
		return nil, err
	}
	return excess, nil
}

// ---------------------------------------------------------------------------
// FR-04 归档失败计数 / 一次性提示事件
// ---------------------------------------------------------------------------

// RecordArchiveFailure 递增归档连续失败计数；达到阈值时暂停自动抽取（FR-04 异常）。
// 返回更新后的治理状态，供服务层判断是否需要通知管理员。
func (r *memoryRepository) RecordArchiveFailure(
	ctx context.Context, tenantID uint64, userID string,
) (*types.MemoryGovernance, error) {
	var gov *types.MemoryGovernance
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := lockGovernanceRow(tx, tenantID, userID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if row == nil {
			row = &types.MemoryGovernance{
				TenantID: tenantID, UserID: userID,
				Generation: uuid.NewString(), CreatedAt: now,
			}
		}
		row.ArchiveFailures++
		if row.ArchiveFailures >= types.MemoryArchiveFailureThreshold && row.ExtractionPausedAt == nil {
			row.ExtractionPausedAt = &now
		}
		row.UpdatedAt = now
		gov = row
		return tx.Save(row).Error
	})
	if err != nil {
		return nil, err
	}
	return gov, nil
}

// ResetArchiveFailures 在归档成功后清零连续失败计数并解除抽取暂停。
func (r *memoryRepository) ResetArchiveFailures(ctx context.Context, tenantID uint64, userID string) error {
	return r.db.WithContext(ctx).Model(&types.MemoryGovernance{}).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		Updates(map[string]interface{}{
			"archive_failures":     0,
			"extraction_paused_at": nil,
			"updated_at":           time.Now().UTC(),
		}).Error
}

// CreateGovernanceEvent 写入一条治理事件。
func (r *memoryRepository) CreateGovernanceEvent(
	ctx context.Context, event *types.MemoryGovernanceEvent,
) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	return r.db.WithContext(ctx).Create(event).Error
}

// ListUnacknowledgedGovernanceEvents 返回尚未提示过的治理事件（新→旧）。
func (r *memoryRepository) ListUnacknowledgedGovernanceEvents(
	ctx context.Context, tenantID uint64, userID string, limit int,
) ([]*types.MemoryGovernanceEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	events := make([]*types.MemoryGovernanceEvent, 0)
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ? AND acknowledged_at IS NULL", tenantID, userID).
		Order("id DESC").
		Limit(limit).
		Find(&events).Error
	if err != nil {
		return nil, err
	}
	return events, nil
}

// AcknowledgeGovernanceEvents 标记治理事件已提示，保证提示只出现一次。
func (r *memoryRepository) AcknowledgeGovernanceEvents(
	ctx context.Context, tenantID uint64, userID string, ids []int64,
) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	now := time.Now()
	res := r.db.WithContext(ctx).Model(&types.MemoryGovernanceEvent{}).
		Where("tenant_id = ? AND user_id = ? AND id IN ? AND acknowledged_at IS NULL", tenantID, userID, ids).
		Update("acknowledged_at", now)
	return res.RowsAffected, res.Error
}
