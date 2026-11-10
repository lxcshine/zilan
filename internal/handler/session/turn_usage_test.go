package session

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-12 消费侧的两条落库路径：
//   - Agent 模式：引擎把用量与归因挂在 EventAgentComplete 的 Extra 上，
//     AgentStreamHandler.handleComplete 写到 assistant 消息上，随后由 qa.go 的
//     defer 统一持久化；
//   - 普通模式：chat pipeline 通过 EventChatComplete 送来 types.TurnUsageData，
//     qa.go 的订阅回调在 answer(Done) 触发落库之前先写到同一条消息上。
//
// 两条路径最终都必须让 messages.usage / messages.context_stats 有值，否则普通模式与
// Agent 模式的归因口径会不一致。

// capturingStreamManager 记录被推进流里的每一个事件，供断言 complete 事件的载荷。
type capturingStreamManager struct {
	events []interfaces.StreamEvent
}

func (m *capturingStreamManager) AppendEvent(
	_ context.Context, _, _ string, evt interfaces.StreamEvent,
) error {
	m.events = append(m.events, evt)
	return nil
}

func (m *capturingStreamManager) GetEvents(
	_ context.Context, _, _ string, fromOffset int,
) ([]interfaces.StreamEvent, int, error) {
	if fromOffset >= len(m.events) {
		return nil, fromOffset, nil
	}
	return m.events[fromOffset:], len(m.events), nil
}

func newTestStreamHandler(t *testing.T, msg *types.Message) (*AgentStreamHandler, *capturingStreamManager) {
	t.Helper()
	streams := &capturingStreamManager{}
	bus := event.NewEventBus()
	handler := NewAgentStreamHandler(
		context.Background(), msg.SessionID, msg.ID, "req-1", time.Now(), msg, streams, bus,
	)
	handler.Subscribe()
	return handler, streams
}

func testAssistantMessage() *types.Message {
	return &types.Message{
		ID:        "assistant-1",
		SessionID: "session-1",
		Role:      "assistant",
	}
}

// ---------------------------------------------------------------------------
// Agent 模式：Extra → assistant 消息 + 流式 complete 事件
// ---------------------------------------------------------------------------

func TestHandleCompleteWritesUsageAndContextStats(t *testing.T) {
	msg := testAssistantMessage()
	handler, streams := newTestStreamHandler(t, msg)

	usage := &types.TokenUsage{PromptTokens: 4200, CompletionTokens: 310, TotalTokens: 4510}
	stats := &types.ContextTurnStats{
		Source: types.ContextTurnStatsSourceAgent,
		Model:  "qwen3-32b",
		Window: 128000,
		Usable: 111616,
		Rounds: 4,
	}
	stats.RecordEvent(types.ContextTurnEvent{
		Kind: types.ContextEventCompaction, Reason: "threshold", Round: 3,
		TokensBefore: 90000, TokensAfter: 31000,
	})

	require.NoError(t, handler.handleComplete(context.Background(), event.Event{
		ID:   "complete-1",
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{
			MessageID:   msg.ID,
			FinalAnswer: "最终答案",
			Extra:       types.NewTurnUsageExtra(usage, stats),
		},
	}))

	// 落库前的内存态：usage 与 context_stats 都挂到了这一行上。
	require.Same(t, usage, msg.Usage)
	require.Same(t, stats, msg.ContextStats)
	require.Equal(t, 4510, msg.Usage.TotalTokens)
	require.Len(t, msg.ContextStats.Events, 1)
	require.Equal(t, 59000, msg.ContextStats.Events[0].FreedTokens)
	require.True(t, msg.IsCompleted)

	// 流式 complete 事件带上本轮用量，前端不必等刷新才能看到。
	require.NotEmpty(t, streams.events)
	complete := streams.events[len(streams.events)-1]
	require.Equal(t, types.ResponseTypeComplete, complete.Type)
	require.Same(t, usage, complete.Usage)
	require.Same(t, usage, complete.Data["usage"])
	// 归因明细不推流：它随消息持久化，由会话详情接口带出。
	require.NotContains(t, complete.Data, "context_stats")

	// buildStreamResponse 把它提升到 StreamResponse.Usage（前端消费的最终形状）。
	response := buildStreamResponse(complete, "req-1")
	require.NotNil(t, response.Usage)
	require.Equal(t, 4510, response.Usage.TotalTokens)
}

func TestHandleCompleteIgnoresUsageForForeignMessage(t *testing.T) {
	msg := testAssistantMessage()
	handler, _ := newTestStreamHandler(t, msg)

	require.NoError(t, handler.handleComplete(context.Background(), event.Event{
		ID:   "complete-1",
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{
			MessageID:   "another-message",
			FinalAnswer: "不该被采纳",
			Extra: types.NewTurnUsageExtra(
				&types.TokenUsage{TotalTokens: 99},
				&types.ContextTurnStats{Source: types.ContextTurnStatsSourceAgent, Window: 1000},
			),
		},
	}))

	require.Nil(t, msg.Usage, "消息 ID 不匹配时绝不能把用量写到别的行上")
	require.Nil(t, msg.ContextStats)
	require.Empty(t, msg.Content)
}

func TestHandleCompleteWithoutExtraLeavesColumnsUntouched(t *testing.T) {
	msg := testAssistantMessage()
	// 先前已经写过一份用量（例如异常时序下的补写）：这一轮供应商没回报，
	// 不得用 NULL 把它抹掉。
	msg.Usage = &types.TokenUsage{TotalTokens: 12}
	handler, streams := newTestStreamHandler(t, msg)

	require.NoError(t, handler.handleComplete(context.Background(), event.Event{
		ID:   "complete-1",
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{MessageID: msg.ID, FinalAnswer: "答案"},
	}))

	require.Equal(t, 12, msg.Usage.TotalTokens)
	complete := streams.events[len(streams.events)-1]
	require.Nil(t, complete.Usage)
	require.NotContains(t, complete.Data, "usage")
}

// ---------------------------------------------------------------------------
// 普通模式：chat.complete 载荷经真实 EventBus 往返
// ---------------------------------------------------------------------------

func TestChatCompletePayloadRoundTripThroughEventBus(t *testing.T) {
	bus := event.NewEventBus()
	msg := testAssistantMessage()

	// qa.go 用的就是这个订阅形状：具体 EventBus + event.EventHandler。
	var received bool
	var gotUsage *types.TokenUsage
	var gotStats *types.ContextTurnStats
	bus.On(event.EventChatComplete, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(types.TurnUsageData)
		if !ok {
			return nil
		}
		if data.MessageID != "" && data.MessageID != msg.ID {
			return nil
		}
		received = true
		gotUsage, gotStats = data.Usage, data.ContextStats
		applyTurnUsage(msg, data.Usage, data.ContextStats)
		return nil
	})

	usage := &types.TokenUsage{PromptTokens: 6800, CompletionTokens: 412, TotalTokens: 7212}
	stats := types.ContextTurnStatsFromDiagnostics(types.ContextTurnStatsSourcePipeline, &types.ContextDiagnostics{
		Window:         128000,
		Usable:         111616,
		ReservedOutput: 16384,
		Intent:         "kb_search",
		Budget:         types.ContextLayerBudget{System: 4000, Memory: 8000, Retrieval: 40000, History: 40000, Query: 8000},
		Used:           types.ContextLayerBudget{System: 3100, Memory: 6200, Retrieval: 38000, History: 21000, Query: 900},
		Actions:        []string{"L2 shrunk 52000 -> 38000 tokens (tiered)"},
	})
	require.NotNil(t, stats)

	// 采集侧走的是 types.EventBusInterface 适配器（见 chat_pipeline.emitTurnUsage）。
	require.NoError(t, bus.AsEventBusInterface().Emit(context.Background(), types.Event{
		Type:      types.EventType(event.EventChatComplete),
		SessionID: msg.SessionID,
		Data:      types.TurnUsageData{MessageID: msg.ID, Usage: usage, ContextStats: stats},
	}))

	require.True(t, received, "chat.complete 必须能被 handler 侧的订阅收到")
	require.Same(t, usage, gotUsage, "进程内传递：值就是指针本身")
	require.Same(t, stats, gotStats)

	// 与 Agent 模式同一个落点：同一条 assistant 消息的 usage / context_stats。
	require.Same(t, usage, msg.Usage)
	require.Same(t, stats, msg.ContextStats)
	require.Equal(t, types.ContextTurnStatsSourcePipeline, msg.ContextStats.Source)
	require.Equal(t, 38000, msg.ContextStats.Used.Retrieval)
	require.Len(t, msg.ContextStats.Events, 1)
	require.Equal(t, types.ContextEventShrink, msg.ContextStats.Events[0].Kind)

	// 转挂到 agent.complete 之后，流式 complete 事件同样带得出 usage。
	handler, streams := newTestStreamHandler(t, msg)
	require.NoError(t, handler.handleComplete(context.Background(), event.Event{
		ID:   "complete-1",
		Type: event.EventAgentComplete,
		Data: event.AgentCompleteData{
			FinalAnswer: "",
			Extra:       types.NewTurnUsageExtra(gotUsage, gotStats),
		},
	}))
	complete := streams.events[len(streams.events)-1]
	require.Same(t, usage, complete.Usage)
}

func TestChatCompletePayloadForForeignMessageIsDropped(t *testing.T) {
	bus := event.NewEventBus()
	msg := testAssistantMessage()

	bus.On(event.EventChatComplete, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(types.TurnUsageData)
		if !ok {
			return nil
		}
		if data.MessageID != "" && data.MessageID != msg.ID {
			return nil
		}
		applyTurnUsage(msg, data.Usage, data.ContextStats)
		return nil
	})

	require.NoError(t, bus.AsEventBusInterface().Emit(context.Background(), types.Event{
		Type: types.EventType(event.EventChatComplete),
		Data: types.TurnUsageData{
			MessageID: "another-message",
			Usage:     &types.TokenUsage{TotalTokens: 99},
		},
	}))

	require.Nil(t, msg.Usage)
	require.Nil(t, msg.ContextStats)
}

// ---------------------------------------------------------------------------
// 写入语义
// ---------------------------------------------------------------------------

func TestApplyTurnUsageSkipsEmptyPayloads(t *testing.T) {
	msg := testAssistantMessage()
	msg.Usage = &types.TokenUsage{TotalTokens: 12}
	msg.ContextStats = &types.ContextTurnStats{Source: types.ContextTurnStatsSourceAgent, Window: 1000}

	// 空值不覆盖既有列：供应商没回报 usage 的那一轮不该把先前写入的数字抹成 NULL。
	applyTurnUsage(msg, &types.TokenUsage{}, &types.ContextTurnStats{})
	require.Equal(t, 12, msg.Usage.TotalTokens)
	require.Equal(t, 1000, msg.ContextStats.Window)

	applyTurnUsage(msg, nil, nil)
	require.Equal(t, 12, msg.Usage.TotalTokens)

	// nil 消息不得 panic：采集侧永远不该因为归因把一轮对话打挂。
	applyTurnUsage(nil, &types.TokenUsage{TotalTokens: 1}, nil)
}

func TestApplyTurnUsageWritesBothColumns(t *testing.T) {
	msg := testAssistantMessage()
	usage := &types.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}
	stats := &types.ContextTurnStats{Source: types.ContextTurnStatsSourcePipeline, Window: 8000}

	applyTurnUsage(msg, usage, stats)
	require.Same(t, usage, msg.Usage)
	require.Same(t, stats, msg.ContextStats)

	// 两列都是 jsonb：Value() 必须给出非 nil 的 JSON，否则 GORM 会写 NULL。
	usageValue, err := msg.Usage.Value()
	require.NoError(t, err)
	require.NotNil(t, usageValue)
	statsValue, err := msg.ContextStats.Value()
	require.NoError(t, err)
	require.NotNil(t, statsValue)
}
