package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// FR-12 每轮 Token 用量归因与分层看板（管理员侧）
//
// 准入：仅工作区管理员（Admin / Owner）。路由层已挂 g.Admin()，这里再校验一次，
// 理由与 FR-09 的记忆管理控制台相同：RequireRole 对 API Key 主体是短路的，
// 而看板没有"机器管理员"这个概念，因此 API Key 身份一律拒绝。
//
// 口径：所有 token 数值都来自每轮持久化的 messages.usage（供应商回报）与
// messages.context_stats（internal/contextx 精确 tokenizer 计数），
// 服务层不做任何字符数估算（PRD FR-12 ⑤）。
//
// 审计：管理员查看聚合属只读操作，且响应里只有聚合数字、没有任何会话正文或
// 用户内容，因此本看板不写审计日志（PRD §5.2 允许只读聚合免审计）；
// 访问本身仍由 middleware.Logger 与 rbac 的拒绝路径留痕。
//
// 跨工作区聚合入口没有实现：仓储层的每一条查询都写死 tenant_id，
// 因此也不存在需要叠加 middleware.RequireCrossTenantAccess 的路径。
// ---------------------------------------------------------------------------

const (
	// tokenUsageDefaultWindow 是未显式给时间窗时回看的长度。
	tokenUsageDefaultWindow = 7 * 24 * time.Hour
	// tokenUsageMaxWindowDay / tokenUsageMaxWindowHour 是两种粒度下允许的最大跨度。
	// 上限同时约束了趋势桶的数量（90 个日桶 / 168 个小时桶），
	// 于是一次请求的响应体大小是可预期的。
	tokenUsageMaxWindowDay  = 90 * 24 * time.Hour
	tokenUsageMaxWindowHour = 7 * 24 * time.Hour
	// tokenUsageDefaultScanLimit / tokenUsageMaxScanLimit 是单次聚合最多载入的轮次数。
	// 超出时看板仍然出数，只是把 Truncated 置真，明确告诉调用方覆盖范围不完整。
	tokenUsageDefaultScanLimit = 5000
	tokenUsageMaxScanLimit     = 20000
	// tokenUsageDayLayout / tokenUsageHourLayout 是趋势桶标签的时间格式（UTC）。
	tokenUsageDayLayout  = "2006-01-02"
	tokenUsageHourLayout = "2006-01-02T15:00Z"
)

type tokenUsageService struct {
	repo interfaces.TokenUsageStatsRepository
}

// NewTokenUsageService 构造看板服务。
func NewTokenUsageService(repo interfaces.TokenUsageStatsRepository) interfaces.TokenUsageService {
	return &tokenUsageService{repo: repo}
}

// tokenUsageAdminCaller 校验调用者是本工作区的管理员，返回租户 ID。
// 身份不足与身份缺失都返回同一个 forbidden：不给调用方探测的余地。
func tokenUsageAdminCaller(ctx context.Context) (uint64, error) {
	// API Key 身份没有工作区角色语义，一律拒绝（PRD §5.1）。
	if _, ok := types.TenantAPIKeyScopeFromContext(ctx); ok {
		return 0, types.ErrTokenUsageForbidden
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return 0, types.ErrTokenUsageForbidden
	}
	if !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin) {
		return 0, types.ErrTokenUsageForbidden
	}
	return tenantID, nil
}

// GetTokenUsageOverview 返回本工作区在 [start, end) 内的用量看板。
func (s *tokenUsageService) GetTokenUsageOverview(
	ctx context.Context, start, end time.Time, granularity string, limit int,
) (*types.TokenUsageOverview, error) {
	tenantID, err := tokenUsageAdminCaller(ctx)
	if err != nil {
		return nil, err
	}
	start, end, granularity, limit, err = normalizeTokenUsageWindow(start, end, granularity, limit)
	if err != nil {
		return nil, err
	}

	total, err := s.repo.CountTurns(ctx, tenantID, start, end)
	if err != nil {
		return nil, err
	}
	turns, err := s.repo.ListTurns(ctx, tenantID, start, end, limit)
	if err != nil {
		return nil, err
	}

	overview := aggregateTokenUsage(tenantID, start, end, granularity, turns)
	overview.ScannedTurns = len(turns)
	// 时间窗内的轮次多于本次载入上限时，看板数据只覆盖最早的一部分。
	overview.Truncated = int64(len(turns)) < total || int64(limit) < total
	return overview, nil
}

// normalizeTokenUsageWindow 钳制时间范围、粒度与扫描上限。
//
// 缺省语义是"最近 7 天、按天分桶"；给了一端就补齐另一端。跨度上限按粒度收敛，
// 于是趋势桶的数量与响应体大小都有界。start >= end 是调用方的参数错误，直接拒绝
// 而不是悄悄交换两端：交换会让"我传错了"变成一份看起来正常的空看板。
func normalizeTokenUsageWindow(
	start, end time.Time, granularity string, limit int,
) (time.Time, time.Time, string, int, error) {
	switch granularity {
	case "":
		granularity = types.TokenUsageGranularityDay
	case types.TokenUsageGranularityDay, types.TokenUsageGranularityHour:
	default:
		return start, end, granularity, limit,
			fmt.Errorf("%w: granularity must be %q or %q",
				types.ErrTokenUsageBadRequest, types.TokenUsageGranularityDay, types.TokenUsageGranularityHour)
	}

	now := time.Now().UTC()
	if end.IsZero() {
		end = now
	}
	end = end.UTC()
	if end.After(now) {
		end = now
	}
	if start.IsZero() {
		start = end.Add(-tokenUsageDefaultWindow)
	}
	start = start.UTC()

	maxWindow := tokenUsageMaxWindowDay
	if granularity == types.TokenUsageGranularityHour {
		maxWindow = tokenUsageMaxWindowHour
	}
	if earliest := end.Add(-maxWindow); start.Before(earliest) {
		start = earliest
	}
	if !end.After(start) {
		return start, end, granularity, limit,
			fmt.Errorf("%w: start must be earlier than end", types.ErrTokenUsageBadRequest)
	}

	switch {
	case limit <= 0:
		limit = tokenUsageDefaultScanLimit
	case limit > tokenUsageMaxScanLimit:
		limit = tokenUsageMaxScanLimit
	}
	return start, end, granularity, limit, nil
}

// aggregateTokenUsage 在服务层做精确聚合：SQL 只负责把时间窗内的行捞出来，
// 分层占用与压缩事件都藏在 JSON 里，Go 侧解析一次即可同时喂给四个视图。
func aggregateTokenUsage(
	tenantID uint64, start, end time.Time, granularity string, turns []types.MessageTurnUsage,
) *types.TokenUsageOverview {
	overview := &types.TokenUsageOverview{
		TenantID:    tenantID,
		Start:       start,
		End:         end,
		Granularity: granularity,
		Trend:       buildTokenUsageTrend(start, end, granularity),
		Layers:      emptyTokenUsageLayerStats(),
		Compaction:  make([]types.TokenUsageCompactionStat, 0),
		Cache:       types.TokenUsageCacheStat{ByStatus: make(map[string]int)},
		GeneratedAt: time.Now().UTC(),
	}
	buckets := make(map[string]*types.TokenUsageTrendPoint, len(overview.Trend))
	for i := range overview.Trend {
		buckets[overview.Trend[i].Bucket] = &overview.Trend[i]
	}

	layerTotals := types.ContextLayerTokens{}
	layerRatios := make([]float64, 5)
	layerTurns := 0

	type compactionKey struct{ kind, reason string }
	compactionIndex := make(map[compactionKey]int)
	compactionTurns := 0
	compactionEvents := 0

	for i := range turns {
		turn := &turns[i]
		if bucket, ok := buckets[tokenUsageBucketKey(turn.CreatedAt, granularity)]; ok {
			bucket.Turns++
			if usage := turn.Usage; usage != nil {
				bucket.PromptTokens += usage.PromptTokens
				bucket.CompletionTokens += usage.CompletionTokens
				bucket.TotalTokens += usage.TotalTokens
				bucket.CacheReadTokens += usage.CacheReadTokens
			}
		}

		if usage := turn.Usage; usage != nil && !usage.IsZero() {
			overview.Totals.TurnsWithUsage++
			overview.Totals.PromptTokens += usage.PromptTokens
			overview.Totals.CompletionTokens += usage.CompletionTokens
			overview.Totals.TotalTokens += usage.TotalTokens
			overview.Totals.CacheReadTokens += usage.CacheReadTokens

			overview.Cache.Turns++
			overview.Cache.PromptTokens += usage.PromptTokens
			overview.Cache.CacheReadTokens += usage.CacheReadTokens
			status := usage.CacheStatus
			if status == "" {
				// 早期写入的行可能没有状态字段：有计量就算回报过，否则算未回报。
				if usage.CacheReported {
					status = types.PromptCacheStatusMiss
				} else {
					status = types.PromptCacheStatusUnreported
				}
			}
			overview.Cache.ByStatus[string(status)]++
			// 命中率的分母只包含"供应商真的回报了缓存计量"的轮次：
			// 把不支持缓存的模型算进分母会把命中率压成一个没有意义的数字。
			if status == types.PromptCacheStatusHit || status == types.PromptCacheStatusMiss {
				overview.Cache.ReportedTurns++
				if status == types.PromptCacheStatusHit || usage.CacheReadTokens > 0 {
					overview.Cache.HitTurns++
				}
			}
		}

		stats := turn.ContextStats
		if stats == nil {
			continue
		}
		overview.Totals.TurnsWithStats++
		// 五层占比只在有分层切分的轮次上统计：Agent 链路不做 L0–L4 预算划分
		// （Used 为零值），把它算进分母会把每一层的占比都稀释掉。
		if !stats.Used.IsZero() {
			layerTurns++
			layerTotals.Add(stats.Used)
			for idx, entry := range stats.Used.Entries(stats.Budget) {
				layerRatios[idx] += entry.Ratio
			}
		}

		if len(stats.Events) > 0 {
			compactionTurns++
		}
		for _, ev := range stats.Events {
			if ev.Kind == "" {
				continue
			}
			compactionEvents++
			key := compactionKey{kind: ev.Kind, reason: ev.Reason}
			idx, ok := compactionIndex[key]
			if !ok {
				overview.Compaction = append(overview.Compaction, types.TokenUsageCompactionStat{
					Kind:   ev.Kind,
					Reason: ev.Reason,
				})
				idx = len(overview.Compaction) - 1
				compactionIndex[key] = idx
			}
			stat := &overview.Compaction[idx]
			stat.Count++
			stat.FreedTokens += ev.FreedTokens
			if ev.Degraded {
				stat.Degraded++
			}
		}
	}

	overview.Totals.Turns = len(turns)
	// 五层平均占比：分母是"有分层归因的轮次"，没有归因的轮次不参与，
	// 否则 Agent 链路（不做五层切分）会把每一层的占比都稀释掉。
	// Total 是这一层在时间窗内一共烧掉的 token，看板既能看平均也能看总量。
	for idx := range overview.Layers {
		total := layerTokenAt(layerTotals, idx)
		overview.Layers[idx].Turns = layerTurns
		overview.Layers[idx].Total = total
		overview.Layers[idx].AvgTokens = ratioOf(float64(total), float64(layerTurns))
		overview.Layers[idx].AvgRatio = ratioOf(layerRatios[idx], float64(layerTurns))
	}

	for idx := range overview.Compaction {
		stat := &overview.Compaction[idx]
		stat.AvgFreed = ratioOf(float64(stat.FreedTokens), float64(stat.Count))
	}
	overview.CompactionTurns = compactionTurns
	overview.CompactionEvents = compactionEvents
	// 压缩触发频率按"有归因的轮次"计算：没有归因的轮次无从判断是否压缩过。
	overview.CompactionRate = ratioOf(float64(compactionTurns), float64(overview.Totals.TurnsWithStats))
	overview.Cache.HitRate = ratioOf(float64(overview.Cache.HitTurns), float64(overview.Cache.ReportedTurns))
	overview.Cache.TokenHitRate = ratioOf(
		float64(overview.Cache.CacheReadTokens), float64(overview.Cache.PromptTokens))
	return overview
}

// buildTokenUsageTrend 生成升序的时间桶，空桶也保留：前端画折线不需要自己补洞。
func buildTokenUsageTrend(start, end time.Time, granularity string) []types.TokenUsageTrendPoint {
	step := 24 * time.Hour
	if granularity == types.TokenUsageGranularityHour {
		step = time.Hour
	}
	trend := make([]types.TokenUsageTrendPoint, 0, 64)
	for cursor := truncateTokenUsageBucket(start, step); cursor.Before(end); cursor = cursor.Add(step) {
		trend = append(trend, types.TokenUsageTrendPoint{Bucket: tokenUsageBucketKey(cursor, granularity)})
	}
	return trend
}

// truncateTokenUsageBucket 把时间对齐到桶边界（UTC 日 / UTC 小时）。
func truncateTokenUsageBucket(t time.Time, step time.Duration) time.Time {
	u := t.UTC()
	if step == time.Hour {
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
	}
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// tokenUsageBucketKey 是一行归因所属的桶标签。
func tokenUsageBucketKey(t time.Time, granularity string) string {
	u := t.UTC()
	if granularity == types.TokenUsageGranularityHour {
		return u.Format(tokenUsageHourLayout)
	}
	return u.Format(tokenUsageDayLayout)
}

// emptyTokenUsageLayerStats 返回固定 L0→L4 五行的空骨架。
// 即使时间窗内一条归因都没有，看板也要给出五行零值，前端不必处理"缺层"。
func emptyTokenUsageLayerStats() []types.TokenUsageLayerStat {
	entries := types.ContextLayerTokens{}.Entries(types.ContextLayerTokens{})
	out := make([]types.TokenUsageLayerStat, 0, len(entries))
	for _, entry := range entries {
		out = append(out, types.TokenUsageLayerStat{Layer: entry.Layer})
	}
	return out
}

// layerTokenAt 按下标取出某一层的累计 token（下标顺序与 ContextLayerTokens.Entries 一致）。
func layerTokenAt(total types.ContextLayerTokens, idx int) int {
	switch idx {
	case 0:
		return total.System
	case 1:
		return total.Memory
	case 2:
		return total.Retrieval
	case 3:
		return total.History
	case 4:
		return total.Query
	default:
		return 0
	}
}

// ratioOf 是看板统一的安全除法：分母为零一律得 0，绝不产出 NaN / Inf
// （NaN 会让 json.Marshal 直接报错，整个看板变成 500）。
func ratioOf(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}
	value := numerator / denominator
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
