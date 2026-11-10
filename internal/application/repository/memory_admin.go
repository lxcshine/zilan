package repository

import (
	"context"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// FR-09 记忆管理控制台：工作区级聚合查询
//
// 与 memory_governance.go 的 CountFactsByStatus（按 (租户, 用户) 聚合）不同，
// 这里所有查询都只带 tenant_id 条件、**不带 user_id 过滤**：控制台要看的是整个
// 工作区的规模分布。tenant_id 在每一条查询里都显式写死，聚合口径永远不越过工作区
// 边界（FR-09 规则 4）；跨工作区聚合没有对应方法，因此也没有被误用的入口。
//
// 全部查询走 Model(&types.MemoryFact{})，软删除行由 GORM 默认作用域自动排除，
// 与用户侧列表的口径一致。
// ---------------------------------------------------------------------------

// 聚合分组列名。只接受本文件内的常量，不接受外部输入，因此不存在拼接注入面。
const (
	memoryFactStatusColumn   = "status"
	memoryFactCategoryColumn = "category"
	memoryFactUserColumn     = "user_id"
)

// memoryUserSummaryRow 是用户维度聚合的行投影。
//
// 单独定义行结构而不直接 Scan 进 types.MemoryUserSummary：GORM 的 Scan 在
// Model 与 Dest 属于不同 schema 时的列匹配行为依赖版本实现，用本文件内的私有
// 行结构再显式搬运，结果确定且与 CountFactsByStatus 的既有写法一致。
//
// LastActivityAt 以字符串接收：SQLite 驱动拒绝把聚合出来的 datetime 自动转成
// time.Time（聚合结果丢失了列类型信息），而 Postgres 会正常返回 time.Time——
// database/sql 的 convertAssign 又能把 time.Time 落到 *string（RFC3339Nano），
// 所以统一按字符串读回、在 Go 侧解析，两种方言共用同一条 SQL。
// 这与 service/knowledge_housekeeping.go 读取 MAX(updated_at) 的处理一致。
type memoryUserSummaryRow struct {
	UserID          string `gorm:"column:user_id"`
	ItemCount       int64  `gorm:"column:item_count"`
	ActiveCount     int64  `gorm:"column:active_count"`
	PendingCount    int64  `gorm:"column:pending_count"`
	SupersededCount int64  `gorm:"column:superseded_count"`
	ArchivedCount   int64  `gorm:"column:archived_count"`
	LastActivityAt  string `gorm:"column:last_activity_at"`
}

// memoryAggregateTimeLayouts 是 Postgres 与 SQLite 经 MAX() 读回时间戳时
// 可能产出的格式，按命中概率排列。
var memoryAggregateTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseMemoryAggregateTime 解析聚合时间列；解析不出时返回 nil。
// 最近活动时间只是控制台上的参考信息，解析失败退化为"未知"即可，
// 不值得让整个聚合查询失败（PRD §5.3 规则 1）。
func parseMemoryAggregateTime(value string) *time.Time {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	for _, layout := range memoryAggregateTimeLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return &parsed
		}
	}
	return nil
}

// toSummary 把行投影搬进对外类型。
func (r memoryUserSummaryRow) toSummary() *types.MemoryUserSummary {
	return &types.MemoryUserSummary{
		UserID:          r.UserID,
		ItemCount:       r.ItemCount,
		ActiveCount:     r.ActiveCount,
		PendingCount:    r.PendingCount,
		SupersededCount: r.SupersededCount,
		ArchivedCount:   r.ArchivedCount,
		LastActivityAt:  parseMemoryAggregateTime(r.LastActivityAt),
	}
}

// memoryUserSummarySelect 是一次分组查询里的状态计数投影：用 CASE WHEN 把四个
// 状态压进同一条 SQL，避免为每个状态各跑一次 COUNT。
const memoryUserSummarySelect = "user_id, COUNT(*) AS item_count, " +
	"SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS active_count, " +
	"SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS pending_count, " +
	"SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS superseded_count, " +
	"SUM(CASE WHEN status = ? THEN 1 ELSE 0 END) AS archived_count, " +
	"MAX(updated_at) AS last_activity_at"

// memoryUserSummaryArgs 是上面四个占位符的绑定值，顺序必须与 SQL 一致。
func memoryUserSummaryArgs() []interface{} {
	return []interface{}{
		types.MemoryStatusActive,
		types.MemoryStatusPending,
		types.MemoryStatusSuperseded,
		types.MemoryStatusArchived,
	}
}

// scopeTenant 把查询限定在一个工作区内。控制台的每一条查询都必须经过它。
func (r *memoryRepository) scopeTenant(db *gorm.DB, tenantID uint64) *gorm.DB {
	return db.Where("tenant_id = ?", tenantID)
}

// groupFactCounts 按给定列做一次 GROUP BY 计数，返回 列值 -> 条数。
// 别名用 group_key / group_count 而不是 key / count：key 在 MySQL 里是保留字，
// 加引号又与 Postgres / SQLite 的标识符规则不一致，换名最省事。
func (r *memoryRepository) groupFactCounts(db *gorm.DB, column string) (map[string]int64, error) {
	var rows []struct {
		Key   string `gorm:"column:group_key"`
		Count int64  `gorm:"column:group_count"`
	}
	selectExpr := column + " AS group_key, COUNT(*) AS group_count"
	if err := db.Select(selectExpr).Group(column).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Key] = row.Count
	}
	return out, nil
}

// CountTenantFactsByStatus 统计整个工作区按状态的记忆分布（FR-09 规则 1）。
func (r *memoryRepository) CountTenantFactsByStatus(
	ctx context.Context, tenantID uint64,
) (map[string]int64, error) {
	return r.groupFactCounts(
		r.scopeTenant(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID),
		memoryFactStatusColumn)
}

// CountTenantFactsByCategory 统计整个工作区按类型的记忆分布（FR-09 规则 1）。
func (r *memoryRepository) CountTenantFactsByCategory(
	ctx context.Context, tenantID uint64,
) (map[string]int64, error) {
	return r.groupFactCounts(
		r.scopeTenant(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID),
		memoryFactCategoryColumn)
}

// CountUserFactsByCategory 统计单个成员按类型的记忆分布（全状态）。
// 与既有 CountActiveFactsByCategory 的区别是后者只数 active，供四模块徽标使用；
// 控制台明细需要看到归档与待确认的构成，因此不过滤状态。
func (r *memoryRepository) CountUserFactsByCategory(
	ctx context.Context, tenantID uint64, userID string,
) (map[string]int64, error) {
	return r.groupFactCounts(
		r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID),
		memoryFactCategoryColumn)
}

// CountTenantMemoryUsers 统计工作区内至少有一条记忆的成员数。
func (r *memoryRepository) CountTenantMemoryUsers(ctx context.Context, tenantID uint64) (int64, error) {
	var total int64
	err := r.scopeTenant(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID).
		Distinct(memoryFactUserColumn).
		Count(&total).Error
	return total, err
}

// ListTenantMemoryUsers 按用户维度分页返回记忆规模，附带成员总数
// （FR-09 规则 2：识别异常增长）。
//
// 排序是 item_count DESC、user_id ASC：规模最大的成员排在最前，同规模时按
// user_id 稳定排序，翻页不会出现同一条记录跨页重复或漏掉。
func (r *memoryRepository) ListTenantMemoryUsers(
	ctx context.Context, tenantID uint64, limit, offset int,
) ([]*types.MemoryUserSummary, int64, error) {
	limit, offset = types.MemoryAdminPage(limit, offset)
	total, err := r.CountTenantMemoryUsers(ctx, tenantID)
	if err != nil {
		return nil, 0, err
	}
	summaries := make([]*types.MemoryUserSummary, 0, limit)
	if total == 0 {
		return summaries, 0, nil
	}
	var rows []memoryUserSummaryRow
	err = r.scopeTenant(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID).
		Select(memoryUserSummarySelect, memoryUserSummaryArgs()...).
		Group(memoryFactUserColumn).
		Order("item_count DESC, user_id ASC").
		Offset(offset).
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	for _, row := range rows {
		summaries = append(summaries, row.toSummary())
	}
	return summaries, total, nil
}

// GetTenantMemoryUser 返回单个成员的规模计数；该成员在本工作区没有任何记忆时
// 返回 (nil, nil)。
//
// 返回 nil 而不区分"用户不存在"与"用户属于其他工作区"是有意的：查询始终带
// tenant_id，其他工作区的同名用户天然落在结果集外，调用方因此拿不到任何可用于
// 探测的信息（FR-09 规则 4）。
func (r *memoryRepository) GetTenantMemoryUser(
	ctx context.Context, tenantID uint64, userID string,
) (*types.MemoryUserSummary, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, nil
	}
	var rows []memoryUserSummaryRow
	err := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Select(memoryUserSummarySelect, memoryUserSummaryArgs()...).
		Group(memoryFactUserColumn).
		Limit(1).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].toSummary(), nil
}

// ListFactIDsByStatus 返回指定状态的记忆 ID，ids 非空时进一步收敛到这批 ID 内。
//
// 管理员"批量删除待确认项"据此把作用范围钉死在 status=pending：先查出真正可删的
// ID，再交给既有的 HardDeleteFacts。即使调用方混入 active / archived 的 ID，
// 它们也不会出现在返回值里，因此绝不会被删除（FR-09 规则 3 / FR-02 规则 6）。
// ids 为空表示不限制 ID，即该成员的全部该状态条目。
func (r *memoryRepository) ListFactIDsByStatus(
	ctx context.Context, tenantID uint64, userID, status string, ids []string, limit int,
) ([]string, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(status) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > types.MemoryAdminBatchLimit {
		limit = types.MemoryAdminBatchLimit
	}
	query := r.scopeUser(r.db.WithContext(ctx).Model(&types.MemoryFact{}), tenantID, userID).
		Where("status = ?", status)
	if len(ids) > 0 {
		query = query.Where("id IN ?", ids)
	}
	out := make([]string, 0, limit)
	// created_at ASC = 最旧者优先，与 TrimPendingFacts 的裁剪顺序一致。
	if err := query.Order("created_at ASC").Order("id ASC").Limit(limit).Pluck("id", &out).Error; err != nil {
		return nil, err
	}
	return out, nil
}
