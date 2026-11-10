package service

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 FR-12 看板的读侧与写侧是否对得上：聚合的输入是 messages.usage /
// messages.context_stats 两个 jsonb 列，测试里的轮次先经过 Value()→Scan() 的同一趟
// JSON 往返，于是"采集侧写进去的形状"与"看板读出来的数字"被同一份断言约束。

// fakeTokenUsageRepo 是只读看板数据源的替身，同时记录查询作用域，
// 用于验证 tenantID 只能来自调用者身份、绝不来自请求参数。
type fakeTokenUsageRepo struct {
	turns []types.MessageTurnUsage
	total int64

	gotTenantID uint64
	gotLimit    int
	gotStart    time.Time
	gotEnd      time.Time
}

func (f *fakeTokenUsageRepo) CountTurns(
	_ context.Context, tenantID uint64, start, end time.Time,
) (int64, error) {
	f.gotTenantID = tenantID
	f.gotStart, f.gotEnd = start, end
	return f.total, nil
}

func (f *fakeTokenUsageRepo) ListTurns(
	_ context.Context, tenantID uint64, start, end time.Time, limit int,
) ([]types.MessageTurnUsage, error) {
	f.gotTenantID, f.gotLimit = tenantID, limit
	f.gotStart, f.gotEnd = start, end
	return f.turns, nil
}

// roundTripTurn 模拟一次落库 + 读回：两列都是 jsonb，写侧走 Value()，读侧走 Scan()。
func roundTripTurn(t *testing.T, msg *types.Message) types.MessageTurnUsage {
	t.Helper()
	turn := types.MessageTurnUsage{ID: msg.ID, CreatedAt: msg.CreatedAt}

	usageValue, err := msg.Usage.Value()
	require.NoError(t, err)
	if usageValue != nil {
		var usage types.TokenUsage
		require.NoError(t, usage.Scan(usageValue))
		turn.Usage = &usage
	}
	statsValue, err := msg.ContextStats.Value()
	require.NoError(t, err)
	if statsValue != nil {
		var stats types.ContextTurnStats
		require.NoError(t, stats.Scan(statsValue))
		turn.ContextStats = &stats
	}
	return turn
}

func adminCtx(tenantID uint64) context.Context {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, tenantID)
	return context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleAdmin)
}

// collectedTurns 构造两轮已采集的归因：一轮来自 chat pipeline（有五层切分），
// 一轮来自 Agent 引擎（不做五层切分，但带压缩事件）。
func collectedTurns(t *testing.T, at time.Time) []types.MessageTurnUsage {
	t.Helper()

	pipelineStats := types.ContextTurnStatsFromDiagnostics(
		types.ContextTurnStatsSourcePipeline, &types.ContextDiagnostics{
			Window:         128000,
			Usable:         111616,
			ReservedOutput: 16384,
			Intent:         "kb_search",
			Budget:         types.ContextLayerBudget{System: 4000, Memory: 8000, Retrieval: 40000, History: 40000, Query: 8000},
			Used:           types.ContextLayerBudget{System: 1000, Memory: 1000, Retrieval: 1000, History: 1000, Query: 1000},
			Actions:        []string{"L2 shrunk 52000 -> 38000 tokens (tiered)"},
		})
	require.NotNil(t, pipelineStats)
	pipelineMsg := &types.Message{
		ID:        "assistant-pipeline",
		Role:      "assistant",
		CreatedAt: at,
		Usage: &types.TokenUsage{
			PromptTokens: 5000, CompletionTokens: 500, TotalTokens: 5500,
			CacheReadTokens: 4000, CacheReported: true, CacheStatus: types.PromptCacheStatusHit,
		},
		ContextStats: pipelineStats,
	}

	agentStats := &types.ContextTurnStats{
		Source: types.ContextTurnStatsSourceAgent,
		Model:  "qwen3-32b",
		Window: 128000,
		Usable: 111616,
		Rounds: 4,
	}
	agentStats.RecordEvent(types.ContextTurnEvent{
		Kind: types.ContextEventCompaction, Reason: "threshold", Round: 3,
		TokensBefore: 90000, TokensAfter: 30000,
	})
	agentMsg := &types.Message{
		ID:        "assistant-agent",
		Role:      "assistant",
		CreatedAt: at.Add(25 * time.Hour),
		Usage: &types.TokenUsage{
			PromptTokens: 20000, CompletionTokens: 1500, TotalTokens: 21500,
			CacheReported: true, CacheStatus: types.PromptCacheStatusMiss,
		},
		ContextStats: agentStats,
	}

	// 一轮没有任何归因的 assistant 行：两列都是 NULL，不应参与分层与压缩口径。
	emptyMsg := &types.Message{
		ID: "assistant-empty", Role: "assistant", CreatedAt: at.Add(26 * time.Hour),
	}

	return []types.MessageTurnUsage{
		roundTripTurn(t, pipelineMsg),
		roundTripTurn(t, agentMsg),
		roundTripTurn(t, emptyMsg),
	}
}

func TestGetTokenUsageOverviewAggregatesCollectedTurns(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(48 * time.Hour)
	turns := collectedTurns(t, start.Add(time.Hour))
	require.Nil(t, turns[2].Usage)
	require.Nil(t, turns[2].ContextStats, "空归因必须落成 NULL")

	repo := &fakeTokenUsageRepo{turns: turns, total: int64(len(turns))}
	svc := NewTokenUsageService(repo)

	overview, err := svc.GetTokenUsageOverview(adminCtx(7), start, end, types.TokenUsageGranularityDay, 0)
	require.NoError(t, err)
	require.NotNil(t, overview)

	// 作用域只能来自调用者身份。
	require.Equal(t, uint64(7), repo.gotTenantID)
	require.Equal(t, uint64(7), overview.TenantID)
	require.Equal(t, tokenUsageDefaultScanLimit, repo.gotLimit, "limit<=0 时回落默认扫描上限")

	require.Equal(t, 3, overview.Totals.Turns)
	require.Equal(t, 2, overview.Totals.TurnsWithUsage)
	require.Equal(t, 2, overview.Totals.TurnsWithStats)
	require.Equal(t, 25000, overview.Totals.PromptTokens)
	require.Equal(t, 2000, overview.Totals.CompletionTokens)
	require.Equal(t, 27000, overview.Totals.TotalTokens)
	require.Equal(t, 4000, overview.Totals.CacheReadTokens)
	require.False(t, overview.Truncated)

	// 五层固定 L0→L4 五行，分母只包含有分层切分的轮次（Agent 轮次不参与）。
	require.Len(t, overview.Layers, 5)
	require.Equal(t, types.ContextLayerSystem, overview.Layers[0].Layer)
	require.Equal(t, types.ContextLayerQuery, overview.Layers[4].Layer)
	for _, layer := range overview.Layers {
		require.Equal(t, 1, layer.Turns)
		require.Equal(t, 1000, layer.Total)
		require.InDelta(t, 1000.0, layer.AvgTokens, 1e-9)
		require.InDelta(t, 0.2, layer.AvgRatio, 1e-9)
	}

	// 压缩/截断事件：pipeline 的分层收敛 + Agent 的阈值压缩，各一条。
	require.Equal(t, 2, overview.CompactionTurns)
	require.Equal(t, 2, overview.CompactionEvents)
	require.InDelta(t, 1.0, overview.CompactionRate, 1e-9, "分母是有归因的轮次")
	byKind := make(map[string]types.TokenUsageCompactionStat, len(overview.Compaction))
	for _, stat := range overview.Compaction {
		byKind[stat.Kind] = stat
	}
	require.Contains(t, byKind, types.ContextEventShrink)
	require.Equal(t, 14000, byKind[types.ContextEventShrink].FreedTokens)
	require.Contains(t, byKind, types.ContextEventCompaction)
	require.Equal(t, "threshold", byKind[types.ContextEventCompaction].Reason)
	require.Equal(t, 60000, byKind[types.ContextEventCompaction].FreedTokens)
	require.InDelta(t, 60000.0, byKind[types.ContextEventCompaction].AvgFreed, 1e-9)

	// 缓存命中率的分母只包含"供应商真的回报了缓存计量"的轮次。
	require.Equal(t, 2, overview.Cache.Turns)
	require.Equal(t, 2, overview.Cache.ReportedTurns)
	require.Equal(t, 1, overview.Cache.HitTurns)
	require.InDelta(t, 0.5, overview.Cache.HitRate, 1e-9)
	require.InDelta(t, 4000.0/25000.0, overview.Cache.TokenHitRate, 1e-9)
	require.Equal(t, 1, overview.Cache.ByStatus[string(types.PromptCacheStatusHit)])
	require.Equal(t, 1, overview.Cache.ByStatus[string(types.PromptCacheStatusMiss)])

	// 趋势按天分桶，空桶也保留。
	require.Len(t, overview.Trend, 2)
	require.Equal(t, "2026-11-01", overview.Trend[0].Bucket)
	require.Equal(t, 1, overview.Trend[0].Turns)
	require.Equal(t, 5500, overview.Trend[0].TotalTokens)
	require.Equal(t, "2026-11-02", overview.Trend[1].Bucket)
	require.Equal(t, 2, overview.Trend[1].Turns, "没有归因的那一轮也计入轮次")
	require.Equal(t, 21500, overview.Trend[1].TotalTokens)
}

func TestGetTokenUsageOverviewMarksTruncatedWindow(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	turns := collectedTurns(t, start.Add(time.Hour))
	// 时间窗内的行数多于本次载入上限 → 看板只覆盖最早的一部分，必须明说。
	repo := &fakeTokenUsageRepo{turns: turns[:1], total: int64(len(turns))}
	svc := NewTokenUsageService(repo)

	overview, err := svc.GetTokenUsageOverview(adminCtx(7), start, start.Add(48*time.Hour), "", 1)
	require.NoError(t, err)
	require.True(t, overview.Truncated)
	require.Equal(t, 1, overview.ScannedTurns)
	require.Equal(t, 1, repo.gotLimit)
	require.Equal(t, types.TokenUsageGranularityDay, overview.Granularity, "缺省粒度是按天")
}

func TestGetTokenUsageOverviewRejectsNonAdmins(t *testing.T) {
	repo := &fakeTokenUsageRepo{total: 0}
	svc := NewTokenUsageService(repo)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	// 普通成员（viewer）看不到聚合视图。
	viewerCtx := context.WithValue(
		context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7)),
		types.TenantRoleContextKey, types.TenantRoleViewer)
	_, err := svc.GetTokenUsageOverview(viewerCtx, start, end, "", 0)
	require.ErrorIs(t, err, types.ErrTokenUsageForbidden)

	// 缺少工作区身份同样拒绝，且与身份不足返回同一个错误：不给探测的余地。
	_, err = svc.GetTokenUsageOverview(context.Background(), start, end, "", 0)
	require.ErrorIs(t, err, types.ErrTokenUsageForbidden)

	// API Key 主体没有工作区角色语义，一律拒绝。
	apiKeyCtx := types.WithTenantAPIKeyScope(adminCtx(7), types.TenantAPIKeyScope{})
	_, err = svc.GetTokenUsageOverview(apiKeyCtx, start, end, "", 0)
	require.ErrorIs(t, err, types.ErrTokenUsageForbidden)

	require.Zero(t, repo.gotTenantID, "被拒绝的请求不得触达数据源")
}

func TestGetTokenUsageOverviewRejectsBadWindow(t *testing.T) {
	repo := &fakeTokenUsageRepo{total: 0}
	svc := NewTokenUsageService(repo)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	_, err := svc.GetTokenUsageOverview(adminCtx(7), start.Add(time.Hour), start, "", 0)
	require.ErrorIs(t, err, types.ErrTokenUsageBadRequest, "start >= end 是参数错误，不悄悄交换两端")

	_, err = svc.GetTokenUsageOverview(adminCtx(7), start, start.Add(time.Hour), "week", 0)
	require.ErrorIs(t, err, types.ErrTokenUsageBadRequest)
}
