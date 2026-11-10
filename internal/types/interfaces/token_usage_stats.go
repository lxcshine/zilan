package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// ---------------------------------------------------------------------------
// FR-12 每轮 Token 用量归因与分层看板
//
// 两个接口都是只读的：看板不改写任何数据，仓储层也不提供任何写方法，
// 于是"管理员视图"这条链路上根本不存在越权写入的入口（PRD §5.1）。
// ---------------------------------------------------------------------------

// TokenUsageStatsRepository 是看板的数据源：按工作区 + 时间窗取出每轮的用量归因。
//
// 所有方法都必须显式接收 tenantID 并把它写进 WHERE 条件——聚合口径永远不越过
// 工作区边界，调用方无法通过传参放宽它。
type TokenUsageStatsRepository interface {
	// CountTurns 统计时间窗内带用量归因（usage 或 context_stats 至少有一个非空）
	// 的 assistant 行数，用于判断聚合是否被截断。
	CountTurns(ctx context.Context, tenantID uint64, start, end time.Time) (int64, error)

	// ListTurns 按创建时间升序取出时间窗内的每轮归因，最多 limit 行。
	// SQL 只负责粗筛（工作区 / 角色 / 时间窗），JSON 的精确解析与聚合在服务层完成：
	// Postgres 把这两列存成 JSONB、SQLite 存成 TEXT，方言差异在仓储层用
	// CAST(... AS TEXT) 抹平，服务层因此只面对一种形状。
	ListTurns(
		ctx context.Context, tenantID uint64, start, end time.Time, limit int,
	) ([]types.MessageTurnUsage, error)
}

// TokenUsageService 提供管理员看板的服务层能力：身份校验、参数钳制与聚合计算。
type TokenUsageService interface {
	// GetTokenUsageOverview 返回本工作区在 [start, end) 内的用量看板：
	// 时间趋势、五层平均占比、压缩/截断触发频率与缓存命中率。
	// 仅工作区管理员可调用，否则返回 types.ErrTokenUsageForbidden。
	GetTokenUsageOverview(
		ctx context.Context, start, end time.Time, granularity string, limit int,
	) (*types.TokenUsageOverview, error)
}
