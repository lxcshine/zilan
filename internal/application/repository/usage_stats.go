package repository

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// FR-12 每轮 Token 用量归因与分层看板：只读数据源
//
// messages 表没有 tenant_id 列，工作区边界靠 JOIN sessions 得到；tenantID 在
// 每一条查询里都显式写死，调用方无法放宽它（PRD §5.1、FR-12 ③）。
//
// 方言兼容：usage / context_stats 在 Postgres 是 JSONB、在 SQLite 是 TEXT。
// 这里统一用 CAST(... AS TEXT) 取回 JSON 文本，再在 Go 侧解析——两种方言共用
// 同一条 SQL，也不需要 PG 专有的 ->> / jsonb_each 操作符（与 FR-09 处理
// MAX(updated_at) 方言差异的思路一致，见 memory_admin.go）。
//
// 聚合本身不在 SQL 里做：分层占用与压缩事件都藏在 JSON 里，SQL 侧聚合要么写两套
// 方言、要么把 JSON 拆成几十列。因此这里只做"时间窗 + 工作区 + 角色"的粗筛并
// 限量返回，精确聚合交给服务层（PRD 要求分页/限量，避免全表载入）。
// ---------------------------------------------------------------------------

// usageStatsTurnSelect 是每轮归因的列投影。COALESCE 把 NULL 折成空串：
// 两列都只写在 assistant 行上，用户行与"供应商未回报 usage"的轮次都是 NULL，
// 而 database/sql 拒绝把 NULL 直接扫进 string。
const usageStatsTurnSelect = "messages.id AS id, messages.created_at AS created_at, " +
	"COALESCE(CAST(messages.usage AS TEXT), '') AS usage_json, " +
	"COALESCE(CAST(messages.context_stats AS TEXT), '') AS context_stats_json"

// usageStatsRow 是查询的行投影。JSON 先按字符串取回，解析在 toTurnUsage 里做，
// 于是"库里存了坏 JSON"只会让那一行退化为无归因，不会让整个看板查询失败。
type usageStatsRow struct {
	ID               string    `gorm:"column:id"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UsageJSON        string    `gorm:"column:usage_json"`
	ContextStatsJSON string    `gorm:"column:context_stats_json"`
}

type tokenUsageStatsRepository struct {
	db *gorm.DB
}

// NewTokenUsageStatsRepository 构造看板数据源。
func NewTokenUsageStatsRepository(db *gorm.DB) interfaces.TokenUsageStatsRepository {
	return &tokenUsageStatsRepository{db: db}
}

// scopeTurns 是两条查询共用的粗筛条件：本工作区、未软删除、assistant 行、
// 时间窗 [start, end)，且至少有一列归因（跳过所有不可能贡献数据的行）。
func (r *tokenUsageStatsRepository) scopeTurns(
	ctx context.Context, tenantID uint64, start, end time.Time,
) *gorm.DB {
	return r.db.WithContext(ctx).
		Table("messages").
		Joins("JOIN sessions ON sessions.id = messages.session_id").
		Where("sessions.tenant_id = ?", tenantID).
		Where("sessions.deleted_at IS NULL").
		Where("messages.deleted_at IS NULL").
		Where("messages.role = ?", "assistant").
		Where("messages.created_at >= ? AND messages.created_at < ?", start, end).
		Where("messages.usage IS NOT NULL OR messages.context_stats IS NOT NULL")
}

// CountTurns 统计时间窗内参与聚合的 assistant 行数。
func (r *tokenUsageStatsRepository) CountTurns(
	ctx context.Context, tenantID uint64, start, end time.Time,
) (int64, error) {
	var total int64
	err := r.scopeTurns(ctx, tenantID, start, end).Count(&total).Error
	if err != nil {
		return 0, err
	}
	return total, nil
}

// ListTurns 按时间升序取出每轮归因，最多 limit 行。
func (r *tokenUsageStatsRepository) ListTurns(
	ctx context.Context, tenantID uint64, start, end time.Time, limit int,
) ([]types.MessageTurnUsage, error) {
	if limit <= 0 {
		return nil, nil
	}
	var rows []usageStatsRow
	err := r.scopeTurns(ctx, tenantID, start, end).
		Select(usageStatsTurnSelect).
		Order("messages.created_at ASC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]types.MessageTurnUsage, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toTurnUsage(ctx))
	}
	return out, nil
}

// toTurnUsage 把行投影解析成对外的每轮归因。坏 JSON 只丢那一行的对应字段，
// 并留一条告警：看板宁可少一轮，也不能因为一行脏数据整体 500。
func (r usageStatsRow) toTurnUsage(ctx context.Context) types.MessageTurnUsage {
	turn := types.MessageTurnUsage{ID: r.ID, CreatedAt: r.CreatedAt}
	if text := strings.TrimSpace(r.UsageJSON); text != "" && text != "null" {
		var usage types.TokenUsage
		if err := json.Unmarshal([]byte(text), &usage); err != nil {
			logger.Warnf(ctx, "[TokenUsage] 解析 messages.usage 失败（message=%s）: %v", r.ID, err)
		} else {
			turn.Usage = &usage
		}
	}
	if text := strings.TrimSpace(r.ContextStatsJSON); text != "" && text != "null" {
		var stats types.ContextTurnStats
		if err := json.Unmarshal([]byte(text), &stats); err != nil {
			logger.Warnf(ctx, "[TokenUsage] 解析 messages.context_stats 失败（message=%s）: %v", r.ID, err)
		} else {
			turn.ContextStats = &stats
		}
	}
	return turn
}
