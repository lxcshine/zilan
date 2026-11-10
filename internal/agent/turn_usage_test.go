package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-12 在 Agent 侧的采集：本轮多次模型往返的用量累加、上下文归因的
// 窗口口径，以及压缩 / 降图 / 规则式截断三类事件都必须留痕。
//
// 口径约束：断言里的 token 数全部来自 internal/contextx 的精确 tokenizer
// （engine.countContextTokens）或模型回报的 usage，测试自身不做任何字符数估算。

// usageFakeChat 是逐次回报用量的假模型：第 i 次流式调用在终局 chunk 上带回 usages[i]，
// 用尽之后不再回报（模拟供应商不给 usage 的情形）。
type usageFakeChat struct {
	usages []types.TokenUsage
	calls  int
}

func (m *usageFakeChat) ChatStream(
	_ context.Context, _ []chat.Message, _ *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	idx := m.calls
	m.calls++
	var usage *types.TokenUsage
	if idx < len(m.usages) {
		reported := m.usages[idx]
		usage = &reported
	}
	ch := make(chan types.StreamResponse, 1)
	ch <- types.StreamResponse{
		ResponseType: types.ResponseTypeAnswer,
		Content:      "答案",
		FinishReason: "stop",
		Done:         true,
		Usage:        usage,
	}
	close(ch)
	return ch, nil
}

func (m *usageFakeChat) Chat(
	_ context.Context, _ []chat.Message, _ *chat.ChatOptions,
) (*types.ChatResponse, error) {
	return &types.ChatResponse{Content: "摘要"}, nil
}

func (m *usageFakeChat) GetModelName() string { return "qwen3-32b" }
func (m *usageFakeChat) GetModelID() string   { return "usage-fake" }

// ---------------------------------------------------------------------------
// 用量累加
// ---------------------------------------------------------------------------

func TestTurnUsageAccumulatesAcrossModelRoundTrips(t *testing.T) {
	model := &usageFakeChat{usages: []types.TokenUsage{
		{
			PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
			CacheReadTokens: 800, CacheReported: true, CacheStatus: types.PromptCacheStatusHit,
		},
		{PromptTokens: 1500, CompletionTokens: 250, TotalTokens: 1750},
	}}
	engine := newTestEngine(t, model)

	for iteration := 0; iteration < 2; iteration++ {
		_, err := engine.streamThinkingToEventBus(
			context.Background(), emptyMessages(), nil, iteration, "session")
		require.NoError(t, err)
	}

	usage := engine.turnUsagePayload()
	require.NotNil(t, usage)
	// messages.usage 上持久化的是本轮总量，不是"最近一次"（lastUsage 只是估算基线）。
	require.Equal(t, 2500, usage.PromptTokens)
	require.Equal(t, 350, usage.CompletionTokens)
	require.Equal(t, 2850, usage.TotalTokens)
	require.Equal(t, 800, usage.CacheReadTokens)
	require.True(t, usage.CacheReported)
	require.Equal(t, types.PromptCacheStatusHit, usage.CacheStatus)
	require.Equal(t, 2, engine.turnRounds)

	// 返回的是副本：调用方改它不会污染引擎里的累计值。
	usage.TotalTokens = 0
	require.Equal(t, 2850, engine.turnUsagePayload().TotalTokens)
}

func TestTurnUsageNilWhenProviderReportsNothing(t *testing.T) {
	engine := newTestEngine(t, &usageFakeChat{})
	_, err := engine.streamThinkingToEventBus(
		context.Background(), emptyMessages(), nil, 0, "session")
	require.NoError(t, err)

	// 一次都没有回报 → nil，于是消息行上的 usage 存 NULL 而不是一份全零的假计量。
	require.Nil(t, engine.turnUsagePayload())
	require.Zero(t, engine.turnRounds)
}

func TestResetTurnAccountingClearsPreviousTurn(t *testing.T) {
	engine := newTestEngine(t, &usageFakeChat{})
	engine.accumulateTurnUsage(&types.TokenUsage{TotalTokens: 120})
	engine.recordContextEvent(1, types.ContextTurnEvent{Kind: types.ContextEventCompaction})
	require.NotNil(t, engine.turnUsagePayload())

	engine.resetTurnAccounting()
	require.Nil(t, engine.turnUsagePayload())
	require.Nil(t, engine.turnStats)
	require.Zero(t, engine.turnRounds)
}

// ---------------------------------------------------------------------------
// 上下文归因的窗口口径
// ---------------------------------------------------------------------------

func TestTurnStatsNilWithoutWindowOrEvents(t *testing.T) {
	// 未配置上下文窗口（compactor 为 nil）且本轮没有任何压缩/截断：没有可展示的内容，
	// 归因为 nil，那一列存 NULL。
	engine := newTestEngine(t, &usageFakeChat{})
	require.Nil(t, engine.turnStatsPayload())
}

func TestTurnStatsCarryWindowAndRounds(t *testing.T) {
	engine := newCompactionEngine(t, &usageFakeChat{})
	engine.accumulateTurnUsage(&types.TokenUsage{TotalTokens: 10})

	stats := engine.turnStatsPayload()
	require.NotNil(t, stats)
	require.Equal(t, types.ContextTurnStatsSourceAgent, stats.Source)
	require.Equal(t, "qwen3-32b", stats.Model)

	settings := engine.compactor.Settings()
	require.Equal(t, settings.MaxContextTokens, stats.Window)
	require.Equal(t, settings.ReserveTokens, stats.ReservedOutput)
	require.Equal(t, settings.MaxContextTokens-settings.ReserveTokens, stats.Usable)
	require.Equal(t, settings.Threshold(), stats.CompactThreshold)
	require.Equal(t, 1, stats.Rounds)

	// Agent 链路不做 L0–L4 预算切分，看板据此把它排除在五层占比的分母之外。
	require.True(t, stats.Budget.IsZero())
	require.True(t, stats.Used.IsZero())
	require.Empty(t, stats.Events)
}

// ---------------------------------------------------------------------------
// 压缩 / 降图 / 截断事件必须计入
// ---------------------------------------------------------------------------

func TestTurnStatsRecordCompactionEvent(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	messages := bigHistory()
	currentTokens := engine.countContextTokens(messages)
	require.True(t, engine.compactor.Settings().ShouldCompact(currentTokens), "前置条件：必须已越阈")

	out, changed := engine.manageContextWindow(context.Background(), messages, 3, currentTokens)
	require.True(t, changed)
	require.Less(t, len(out), len(messages))

	stats := engine.turnStatsPayload()
	require.NotNil(t, stats)
	require.Len(t, stats.Events, 1)

	ev := stats.Events[0]
	require.Equal(t, types.ContextEventCompaction, ev.Kind)
	require.Equal(t, string(compaction.ReasonThreshold), ev.Reason)
	require.Equal(t, 3, ev.Round, "Agent 链路的事件必须带上轮次")
	require.Equal(t, currentTokens, ev.TokensBefore, "口径是精确 tokenizer 的计数")
	require.Positive(t, ev.FreedTokens)
	require.Equal(t, ev.TokensBefore-ev.TokensAfter, ev.FreedTokens)
	require.False(t, ev.Degraded)
	require.False(t, ev.CheckpointSaved, "没有检查点 sink 时不得谎报已保存")
}

func TestTurnStatsRecordCompactionEvenWhenNotAdopted(t *testing.T) {
	// 摘要和原文一样长 → 释放量不足 1/20 → 本轮不采用压缩结果。摘要调用与检查点仍然
	// 真实发生了，必须留痕，但释放量记 0，否则看板会把没兑现的收益算进压缩回报率。
	model := &compactionFakeChat{summary: strings.Repeat("冗长摘要 ", 4000)}
	engine := newCompactionEngine(t, model)
	messages := bigHistory()

	out, ok := engine.runCompaction(context.Background(), messages, 1, compaction.ReasonThreshold)
	require.False(t, ok)
	require.Equal(t, len(messages), len(out), "不采用压缩结果时原样返回")

	stats := engine.turnStatsPayload()
	require.Len(t, stats.Events, 1)
	ev := stats.Events[0]
	require.Equal(t, types.ContextEventCompaction, ev.Kind)
	require.Equal(t, "released_too_little_not_adopted", ev.Detail)
	require.Zero(t, ev.FreedTokens)
	require.Equal(t, ev.TokensBefore, ev.TokensAfter)
}

func TestTurnStatsRecordImageDrop(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n不该被调用"}
	engine := newCompactionEngine(t, model)

	// 4 张图 × 1024 token 的保守估值已越过阈值，文本本身很小。
	messages := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "第一轮", MultiContent: []chat.MessageContentPart{
			imagePart("data:image/png;base64,OLD1"), imagePart("data:image/png;base64,OLD2"),
		}},
		{Role: "assistant", Content: "好的"},
		{Role: "user", Content: "本轮提问", MultiContent: []chat.MessageContentPart{
			imagePart("data:image/png;base64,CUR1"), imagePart("data:image/png;base64,CUR2"),
		}},
	}
	before := engine.countContextTokens(messages)
	require.True(t, engine.compactor.Settings().ShouldCompact(before), "前置条件：必须已越阈")

	out, changed := engine.manageContextWindow(context.Background(), messages, 1, before)
	require.True(t, changed)
	require.Zero(t, model.summaryCalls, "图片导致的溢出不得先压缩文本历史")
	require.Equal(t, 2, compaction.CountImages(out))

	stats := engine.turnStatsPayload()
	require.Len(t, stats.Events, 1)
	ev := stats.Events[0]
	require.Equal(t, types.ContextEventImageDrop, ev.Kind)
	require.Equal(t, "multimodal_overflow", ev.Reason)
	require.Equal(t, 1, ev.Round)
	require.Equal(t, before, ev.TokensBefore)
	require.Positive(t, ev.FreedTokens, "图片按图块计费，移除两张必须真的回收空间")
	require.Contains(t, ev.Detail, "dropped 2 earliest image(s)")
}

func TestTurnStatsRecordTruncationWhenCompactionCannotHelp(t *testing.T) {
	model := &compactionFakeChat{summary: strings.Repeat("冗长摘要 ", 4000)}
	engine := newCompactionEngine(t, model)
	messages := bigHistory()
	currentTokens := engine.countContextTokens(messages)

	out, changed := engine.manageContextWindow(context.Background(), messages, 1, currentTokens)
	require.True(t, changed)
	require.Contains(t, out[1].Content, compaction.TruncateMarker)

	stats := engine.turnStatsPayload()
	require.NotNil(t, stats)
	// 两件事都要留痕：那次没被采用的压缩，以及随后丢弃了历史的规则式截断。
	var compactions, truncations []types.ContextTurnEvent
	for _, ev := range stats.Events {
		switch ev.Kind {
		case types.ContextEventCompaction:
			compactions = append(compactions, ev)
		case types.ContextEventTruncation:
			truncations = append(truncations, ev)
		}
	}
	require.Len(t, compactions, 1)
	require.Equal(t, "released_too_little_not_adopted", compactions[0].Detail)

	require.Len(t, truncations, 1)
	ev := truncations[0]
	require.Equal(t, "history_truncated", ev.Reason)
	require.Equal(t, 1, ev.Round)
	require.True(t, ev.Degraded, "走到规则式截断说明摘要式压缩已经救不回来了")
	require.Positive(t, ev.FreedTokens)
	require.Greater(t, ev.TokensBefore, ev.TokensAfter)
	require.Contains(t, ev.Detail, compaction.TruncateMarker)
}

// ---------------------------------------------------------------------------
// 通过 EventAgentComplete 的 Extra 交给消费侧
// ---------------------------------------------------------------------------

func TestEmitCompletionEventCarriesTurnUsageExtra(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)

	var got event.AgentCompleteData
	var received bool
	engine.eventBus.On(event.EventAgentComplete, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentCompleteData)
		if ok {
			got, received = data, true
		}
		return nil
	})

	// 一次真实的压缩 + 一次模型往返的用量。
	messages := bigHistory()
	_, changed := engine.manageContextWindow(
		context.Background(), messages, 2, engine.countContextTokens(messages))
	require.True(t, changed)
	engine.accumulateTurnUsage(&types.TokenUsage{PromptTokens: 900, CompletionTokens: 100, TotalTokens: 1000})

	engine.emitCompletionEvent(
		context.Background(), &types.AgentState{FinalAnswer: "答案"}, "session", "msg-1", time.Now())

	require.True(t, received)
	require.Equal(t, "msg-1", got.MessageID)

	// 载荷往返：Extra 里的值就是指针本身，进程内不做序列化。
	usage, stats := types.TurnUsageFromExtra(got.Extra)
	require.NotNil(t, usage)
	require.Equal(t, 1000, usage.TotalTokens)
	require.Equal(t, 900, usage.PromptTokens)
	require.NotNil(t, stats)
	require.Equal(t, types.ContextTurnStatsSourceAgent, stats.Source)
	require.Equal(t, 1, stats.Rounds)
	require.Len(t, stats.Events, 1)
	require.Equal(t, types.ContextEventCompaction, stats.Events[0].Kind)
	require.False(t, stats.Empty(), "非空归因才会被写进 context_stats 列")
}

func TestEmitCompletionEventOmitsExtraWhenNothingReported(t *testing.T) {
	engine := newTestEngine(t, &usageFakeChat{})

	var got event.AgentCompleteData
	engine.eventBus.On(event.EventAgentComplete, func(_ context.Context, evt event.Event) error {
		got, _ = evt.Data.(event.AgentCompleteData)
		return nil
	})
	engine.emitCompletionEvent(
		context.Background(), &types.AgentState{FinalAnswer: "答案"}, "session", "msg-1", time.Now())

	require.Nil(t, got.Extra, "既没有用量也没有归因时不凭空造一个空 map")
}
