package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-10 在 engine 侧的挂载点：检查点的写入时机（释放量过小也要写）、
// 轮次身份视图向引擎消息坐标的平移，以及没有 sink 时的退化行为。

// recordingCheckpointSink 记录引擎写下的每一份压缩检查点。
type recordingCheckpointSink struct {
	messageIDs  []string
	checkpoints []*types.ContextCheckpoint
	err         error
}

func (s *recordingCheckpointSink) SaveContextCheckpoint(
	_ context.Context, turnMessageID string, checkpoint *types.ContextCheckpoint,
) error {
	if s.err != nil {
		return s.err
	}
	s.messageIDs = append(s.messageIDs, turnMessageID)
	s.checkpoints = append(s.checkpoints, checkpoint)
	return nil
}

// bigHistoryView 为 bigHistory() 的十个轮次构造轮次身份视图（坐标即引擎消息坐标：
// bigHistory 自己就带着系统提示）。
func bigHistoryView() *compaction.HistoryView {
	view := compaction.NewHistoryView("turn-0", "", "", 0)
	for i := 0; i < 10; i++ {
		turnID := fmt.Sprintf("turn-%d", i)
		start := 1 + 3*i
		view.AppendStoredTurn(turnID, fmt.Sprintf("assistant-%d", i), []types.StoredMessage{
			{TurnID: turnID, Role: "user", Content: fmt.Sprintf("第%d轮提问", i)},
			{TurnID: turnID, Role: "assistant", Content: ""},
		}, start, start+3)
	}
	return view
}

// 一次压缩可能因为释放量太小而被本轮丢弃，却仍然已经把已落库的历史摘要完了：一段
// 很短的已存历史紧邻一个自身就撑满窗口的活跃轮次，切点够不到那个重量。这份摘要照样是
// 合法的检查点，丢掉它只会让下一轮重付一次摘要调用。
func TestContextCheckpointIsSavedWhenCompactionFreesTooLittle(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	sink := &recordingCheckpointSink{}
	engine.SetContextCheckpointSink(sink)

	messages := []chat.Message{
		{Role: "system", Content: "you are an agent"},
		{Role: "user", Content: "第一个问题"},
		{Role: "assistant", Content: "第一个回答"},
		{Role: "user", Content: "读取导出文件"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{
			ID: "call-1", Type: "function",
			Function: chat.FunctionCall{Name: "read_file", Arguments: `{"path":"/w/export.csv"}`},
		}}},
		{Role: "tool", Name: "read_file", ToolCallID: "call-1", Content: strings.Repeat("row,value ", 3000)},
	}
	view := compaction.NewHistoryView("turn-a", "", "", 0)
	view.AppendStoredTurn("turn-a", "assistant-a", []types.StoredMessage{
		{TurnID: "turn-a", Role: "user", Content: "第一个问题"},
		{TurnID: "turn-a", Role: "assistant", Content: "第一个回答"},
	}, 1, 3)
	engine.compactor.SetHistoryView(view)

	before := engine.countContextTokens(messages)
	require.True(t, engine.compactor.Settings().ShouldCompact(before), "前置条件：必须已越阈")

	out, ok := engine.runCompaction(context.Background(), messages, 1, compaction.ReasonThreshold)
	require.False(t, ok, "释放量不足 1/20，本轮不采用压缩结果")
	require.Equal(t, messages, out)
	require.Positive(t, engine.compactionExhaustedAt, "释放量不足仍要置耗尽标记")

	require.Len(t, sink.messageIDs, 1, "被丢弃的压缩仍然要落检查点")
	require.Equal(t, "assistant-a", sink.messageIDs[0])
	cp := sink.checkpoints[0]
	require.Equal(t, "## Goal\n继续任务", cp.Summary)
	require.Equal(t, view.Turns[0].Chain, cp.PrefixHash)
	require.Equal(t, "turn-a", cp.AnchorTurnID)
	require.Equal(t, 1, cp.CoveredTurns)
	require.Equal(t, types.ContextCheckpointSourceAgent, cp.Source)
	require.False(t, cp.CreatedAt.IsZero())
}

func TestContextCheckpointIsSavedOnSuccessfulCompaction(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	sink := &recordingCheckpointSink{}
	engine.SetContextCheckpointSink(sink)

	messages := bigHistory()
	view := bigHistoryView()
	engine.compactor.SetHistoryView(view)

	out, ok := engine.runCompaction(context.Background(), messages, 1, compaction.ReasonThreshold)
	require.True(t, ok)
	require.Less(t, len(out), len(messages))

	require.Len(t, sink.messageIDs, 1)
	require.Equal(t, "assistant-9", sink.messageIDs[0],
		"检查点落在最后一个被覆盖的已存轮次上，活跃轮次不算")
	cp := sink.checkpoints[0]
	require.Equal(t, view.Turns[9].Chain, cp.PrefixHash)
	require.Equal(t, 10, cp.CoveredTurns)
	require.False(t, cp.Degraded)
}

func TestContextCheckpointSaveFailureDoesNotBreakTheTurn(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	engine.SetContextCheckpointSink(&recordingCheckpointSink{err: fmt.Errorf("db down")})
	engine.compactor.SetHistoryView(bigHistoryView())

	out, ok := engine.runCompaction(context.Background(), bigHistory(), 1, compaction.ReasonThreshold)
	require.True(t, ok, "写入失败只是下一轮多付一次摘要，本轮照常采用压缩结果")
	require.Less(t, len(out), len(bigHistory()))
}

func TestRunCompactionWithoutSinkProducesNoCheckpoint(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	engine.compactor.SetHistoryView(bigHistoryView())

	// 没有 sink：saveContextCheckpoint 必须安静地什么也不做。
	_, ok := engine.runCompaction(context.Background(), bigHistory(), 1, compaction.ReasonThreshold)
	require.True(t, ok)
}

func TestAttachHistoryViewShiftsSpansIntoEngineCoordinates(t *testing.T) {
	engine := newCompactionEngine(t, &compactionFakeChat{summary: "摘要"})
	llmContext := []chat.Message{
		{Role: "user", Content: "第一个问题"},
		{Role: "assistant", Content: "第一个回答"},
	}
	view := compaction.NewHistoryView("turn-a", "", "", 0)
	view.AppendStoredTurn("turn-a", "assistant-a", []types.StoredMessage{
		{TurnID: "turn-a", Role: "user", Content: "第一个问题"},
		{TurnID: "turn-a", Role: "assistant", Content: "第一个回答"},
	}, 0, 2)
	engine.SetHistoryCheckpointView(view)

	messages := engine.buildMessagesWithLLMContext("系统提示", "本轮提问", "session-1", llmContext, nil)
	engine.attachHistoryView(context.Background(), messages, llmContext)

	got := engine.compactor.HistoryView()
	require.NotNil(t, got)
	require.Len(t, got.Turns, 1)
	require.Equal(t, 1, got.Turns[0].Start, "系统提示占一条，历史区间必须整体平移")
	require.Equal(t, 3, got.Turns[0].End)
	require.Equal(t, "assistant-a", got.Turns[0].AssistantMessageID)
	require.Equal(t, view.Turns[0].Chain, got.Turns[0].Chain, "平移不得改动前缀哈希")
	require.Equal(t, 0, view.Turns[0].Start, "原视图不得被就地修改")
}

func TestAttachHistoryViewSkipsWhenHistoryIsNotOneToOne(t *testing.T) {
	engine := newCompactionEngine(t, &compactionFakeChat{summary: "摘要"})
	// llmContext 里混进一条 system 消息：buildMessagesWithLLMContext 会把它丢掉，
	// 于是下标不再对齐。此时宁可不产生检查点，也不能把摘要存到错误的轮次上。
	llmContext := []chat.Message{
		{Role: "system", Content: "混进来的系统消息"},
		{Role: "user", Content: "第一个问题"},
		{Role: "assistant", Content: "第一个回答"},
	}
	view := compaction.NewHistoryView("turn-a", "", "", 0)
	view.AppendStoredTurn("turn-a", "assistant-a", nil, 1, 3)
	engine.SetHistoryCheckpointView(view)
	engine.compactor.SetHistoryView(view)

	messages := engine.buildMessagesWithLLMContext("系统提示", "本轮提问", "session-1", llmContext, nil)
	engine.attachHistoryView(context.Background(), messages, llmContext)
	require.Nil(t, engine.compactor.HistoryView(), "下标错位时必须清掉已有视图")
}

func TestHistoryViewIsInvalidatedAfterCompactionRewritesMessages(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	sink := &recordingCheckpointSink{}
	engine.SetContextCheckpointSink(sink)
	engine.compactor.SetHistoryView(bigHistoryView())

	_, ok := engine.runCompaction(context.Background(), bigHistory(), 1, compaction.ReasonThreshold)
	require.True(t, ok)
	require.Len(t, sink.messageIDs, 1)
	require.Nil(t, engine.compactor.HistoryView(), "消息已被重建，视图下标不再可信")

	// 同一轮内再压一次：压缩照常，但不得写出第二份边界错误的检查点。
	_, ok = engine.runCompaction(context.Background(), bigHistory(), 2, compaction.ReasonThreshold)
	require.True(t, ok)
	require.Len(t, sink.messageIDs, 1)
}

func TestCompactFinalAnswerContextProducesNoCheckpoint(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	sink := &recordingCheckpointSink{}
	engine.SetContextCheckpointSink(sink)
	engine.compactor.SetHistoryView(bigHistoryView())

	// 最终答案合成的消息形态：系统提示 + 用户问题 + 每条工具结果渲染为 user 消息 + 合成指令。
	// 这些下标与装载历史时的轮次区间无关，因此这次压缩不得产出检查点。
	payload := strings.Repeat("工具产出 ", 400)
	messages := []chat.Message{
		{Role: "system", Content: "You are the Zilan agent."},
		{Role: "user", Content: "用户问题"},
	}
	for i := 0; i < 6; i++ {
		messages = append(messages, chat.Message{
			Role: "user", Content: "Tool knowledge_search returned: " + payload,
		})
	}
	messages = append(messages, chat.Message{
		Role:    "user",
		Content: "Based on the above tool call results, generate a complete answer.",
	})
	require.True(t, engine.compactor.Settings().ShouldCompact(engine.countContextTokens(messages)))

	out := engine.compactFinalAnswerContext(context.Background(), messages, 6)
	require.Less(t, len(out), len(messages))
	require.Empty(t, sink.messageIDs, "合成上下文的压缩不得写检查点")
}
