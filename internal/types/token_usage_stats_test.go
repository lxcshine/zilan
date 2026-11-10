package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件覆盖 FR-12 采集通道的载荷形状：Extra 键的进程内往返、TurnUsageData 的
// JSON 往返、五层诊断到归因的转换，以及两列 jsonb 的 NULL 语义。

func TestNewTurnUsageExtraRoundTrip(t *testing.T) {
	usage := &TokenUsage{PromptTokens: 1200, CompletionTokens: 300, TotalTokens: 1500}
	stats := &ContextTurnStats{
		Source: ContextTurnStatsSourceAgent,
		Window: 128000,
		Usable: 111616,
		Rounds: 3,
	}
	stats.RecordEvent(ContextTurnEvent{
		Kind: ContextEventCompaction, Reason: "threshold", Round: 2,
		TokensBefore: 40000, TokensAfter: 12000,
	})

	extra := NewTurnUsageExtra(usage, stats)
	require.Contains(t, extra, TurnUsageExtraKey)
	require.Contains(t, extra, TurnContextStatsExtraKey)

	// 进程内传递：取回来的必须是同一个指针，不做任何序列化。
	gotUsage, gotStats := TurnUsageFromExtra(extra)
	require.Same(t, usage, gotUsage)
	require.Same(t, stats, gotStats)
	require.Equal(t, 1500, gotUsage.TotalTokens)
	require.Len(t, gotStats.Events, 1)
	// RecordEvent 只给了前后规模，释放量由它自己补齐。
	require.Equal(t, 28000, gotStats.Events[0].FreedTokens)
}

func TestNewTurnUsageExtraNilWhenNothingToCarry(t *testing.T) {
	require.Nil(t, NewTurnUsageExtra(nil, nil))

	// 只有一半也要能走通，缺的那一半取回来是 nil。
	usage := &TokenUsage{TotalTokens: 7}
	gotUsage, gotStats := TurnUsageFromExtra(NewTurnUsageExtra(usage, nil))
	require.Same(t, usage, gotUsage)
	require.Nil(t, gotStats)
}

func TestTurnUsageFromExtraIgnoresForeignPayloads(t *testing.T) {
	// 类型不符时安静地返回 nil：归因缺失只让两列存 NULL，绝不能让答案落库失败。
	gotUsage, gotStats := TurnUsageFromExtra(map[string]interface{}{
		TurnUsageExtraKey:        `{"total_tokens":7}`,
		TurnContextStatsExtraKey: 42,
	})
	require.Nil(t, gotUsage)
	require.Nil(t, gotStats)

	nilUsage, nilStats := TurnUsageFromExtra(nil)
	require.Nil(t, nilUsage)
	require.Nil(t, nilStats)
}

func TestTurnUsageDataJSONRoundTrip(t *testing.T) {
	stats := ContextTurnStatsFromDiagnostics(ContextTurnStatsSourcePipeline, &ContextDiagnostics{
		Window:         128000,
		Usable:         111616,
		ReservedOutput: 16384,
		Intent:         "kb_search",
		Budget:         ContextLayerBudget{System: 4000, Memory: 8000, Retrieval: 40000, History: 40000, Query: 8000},
		Used:           ContextLayerBudget{System: 3100, Memory: 6200, Retrieval: 38000, History: 21000, Query: 900},
		Actions:        []string{"L2 shrunk 52000 -> 38000 tokens (tiered)"},
	})
	require.NotNil(t, stats)

	payload := TurnUsageData{
		MessageID:    "msg-1",
		Usage:        &TokenUsage{PromptTokens: 68200, CompletionTokens: 412, TotalTokens: 68612},
		ContextStats: stats,
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	var decoded TurnUsageData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, "msg-1", decoded.MessageID)
	require.Equal(t, 68612, decoded.Usage.TotalTokens)
	require.Equal(t, ContextTurnStatsSourcePipeline, decoded.ContextStats.Source)
	require.Equal(t, 38000, decoded.ContextStats.Used.Retrieval)
	require.Len(t, decoded.ContextStats.Events, 1)
	require.Equal(t, ContextEventShrink, decoded.ContextStats.Events[0].Kind)
	require.Equal(t, 14000, decoded.ContextStats.Events[0].FreedTokens)
}

func TestContextTurnStatsValueStoresNullWhenEmpty(t *testing.T) {
	// nil 与空归因都存 NULL：读取侧只认 NULL / 非 NULL。
	var nilStats *ContextTurnStats
	value, err := nilStats.Value()
	require.NoError(t, err)
	require.Nil(t, value)

	value, err = (&ContextTurnStats{Source: ContextTurnStatsSourceAgent}).Value()
	require.NoError(t, err)
	require.Nil(t, value)

	stats := &ContextTurnStats{Source: ContextTurnStatsSourceAgent, Window: 128000}
	value, err = stats.Value()
	require.NoError(t, err)
	require.NotNil(t, value)

	var scanned ContextTurnStats
	require.NoError(t, scanned.Scan(value))
	require.Equal(t, 128000, scanned.Window)

	// 库里存 NULL 时扫出零值，而不是报错。
	require.NoError(t, scanned.Scan(nil))
	require.True(t, scanned.Empty())
}

func TestContextTurnStatsFromDiagnosticsNilPassthrough(t *testing.T) {
	// 未开启分层预算时诊断为 nil，这一行的 context_stats 就该是 NULL。
	require.Nil(t, ContextTurnStatsFromDiagnostics(ContextTurnStatsSourcePipeline, nil))
	// 有诊断但内容为空（全零）时同样返回 nil，不存一份没有信息量的归因。
	require.Nil(t, ContextTurnStatsFromDiagnostics(ContextTurnStatsSourcePipeline, &ContextDiagnostics{}))
}

func TestContextTurnStatsLayerEntriesRatio(t *testing.T) {
	stats := &ContextTurnStats{
		Used:   ContextLayerTokens{System: 100, Memory: 100, Retrieval: 100, History: 100, Query: 100},
		Budget: ContextLayerTokens{System: 200},
	}
	entries := stats.LayerEntries()
	require.Len(t, entries, 5)
	require.Equal(t, ContextLayerSystem, entries[0].Layer)
	require.Equal(t, 200, entries[0].Budget)
	require.InDelta(t, 0.2, entries[0].Ratio, 1e-9)
	require.Equal(t, ContextLayerQuery, entries[4].Layer)

	// 分母为零时 ratio 一律为 0，看板不会出现 NaN。
	var empty *ContextTurnStats
	require.Nil(t, empty.LayerEntries())
	for _, entry := range (&ContextTurnStats{}).LayerEntries() {
		require.Zero(t, entry.Ratio)
	}
}
