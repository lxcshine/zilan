package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-10：压缩检查点的产出条件、前缀哈希的一致性与失效判定。

const checkpointTestPayload = "检索结果 "

// storedHistory 构造 rounds 个已持久化轮次（每轮 user + assistant）加一条活跃轮次的
// 用户提问，并给出与之对应的轮次身份视图。视图里的区间坐标与 messages 一致。
func storedHistory(rounds int, mutate func(turnID, role, content string) string) ([]chat.Message, *HistoryView) {
	if mutate == nil {
		mutate = func(_, _, content string) string { return content }
	}
	messages := []chat.Message{{Role: "system", Content: "You are the Zilan agent."}}
	view := NewHistoryView("req-0", "", "", 0)
	for i := 0; i < rounds; i++ {
		turnID := fmt.Sprintf("req-%d", i)
		start := len(messages)
		question := mutate(turnID, "user", fmt.Sprintf("question %d %s", i, strings.Repeat(checkpointTestPayload, 40)))
		answer := mutate(turnID, "assistant", fmt.Sprintf("answer %d", i))
		messages = append(messages,
			chat.Message{Role: "user", Content: question},
			chat.Message{Role: "assistant", Content: answer},
		)
		view.AppendStoredTurn(turnID, fmt.Sprintf("assistant-%d", i), []types.StoredMessage{
			{TurnID: turnID, Role: "user", Content: question},
			{TurnID: turnID, Role: "assistant", Content: answer},
		}, start, len(messages))
	}
	// 活跃轮次：本轮的用户提问不属于任何已持久化轮次，压缩不得为它产生检查点。
	messages = append(messages, chat.Message{Role: "user", Content: "本轮提问"})
	return messages, view
}

func TestCompactProducesCheckpointOnStoredTurnBoundary(t *testing.T) {
	messages, view := storedHistory(4, nil)
	llm := &stubChat{response: "## Goal\n继续任务"}
	c := New(llm, testCounter(), noSplitSettings())
	require.NotNil(t, c)
	c.SetHistoryView(view)

	result, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err)
	require.NotNil(t, result.Checkpoint, "被摘要区间正好结束在已持久化轮次上，必须产出检查点")

	// 保留预算只够最后一条已存轮次的回答与本轮提问，因此被覆盖的是前三轮。
	cp := result.Checkpoint
	require.Equal(t, "req-2", cp.TurnID, "检查点落在被覆盖的最后一轮上")
	require.Equal(t, "assistant-2", cp.MessageID, "检查点写在该轮的 assistant 行")
	require.Equal(t, "## Goal\n继续任务", cp.Summary)
	require.False(t, cp.Degraded)
	require.Equal(t, "req-0", cp.AnchorTurnID)
	require.Empty(t, cp.BaseHash)
	require.Equal(t, 3, cp.CoveredTurns, "覆盖轮次数必须包含本轮新覆盖的全部轮次")
	require.Equal(t, view.Turns[2].Chain, cp.PrefixHash, "前缀哈希就是覆盖到该轮为止的链值")
}

func TestCompactCheckpointIsNilWithoutStoredTurnIdentity(t *testing.T) {
	messages, _ := storedHistory(4, nil)
	llm := &stubChat{response: "## Goal\n继续任务"}
	c := New(llm, testCounter(), noSplitSettings())
	require.NotNil(t, c)
	// 没有注入轮次身份视图：Zilan 的 chat.Message 不带轮次字段，拿不到身份就不产生
	// 检查点，退化为「只在本轮内压缩」。

	result, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err)
	require.Nil(t, result.Checkpoint)
	require.Positive(t, result.Freed(), "没有检查点不影响压缩本身")
}

func TestCompactCheckpointCoversStoredTurnsWhenLiveTurnIsSplit(t *testing.T) {
	messages, view := storedHistory(3, nil)
	// 活跃轮次自带工具调用与结果：切点会把这一轮切开，但检查点仍然只覆盖已持久化的
	// 那几轮——轮次前缀摘要描述的是下一轮还会原样重放的内容，不进检查点。
	payload := strings.Repeat(checkpointTestPayload, 200)
	messages = append(messages,
		chat.Message{
			Role:      "assistant",
			Content:   "思考",
			ToolCalls: []chat.ToolCall{toolCall("live-1", "knowledge_search", `{"query":"q"}`)},
		},
		chat.Message{Role: "tool", ToolCallID: "live-1", Name: "knowledge_search", Content: payload},
	)

	llm := &stubChat{response: "## Goal\n继续任务"}
	c := New(llm, testCounter(), noSplitSettings())
	require.NotNil(t, c)
	c.SetHistoryView(view)

	result, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err)
	require.True(t, result.SplitTurn)
	require.NotNil(t, result.Checkpoint)
	require.Equal(t, "req-2", result.Checkpoint.TurnID, "只覆盖已持久化的轮次，不含被切开的活跃轮次")
	require.Equal(t, 3, result.Checkpoint.CoveredTurns)
	require.Equal(t, view.Turns[2].Chain, result.Checkpoint.PrefixHash)
	require.Equal(t, "## Goal\n继续任务", result.Checkpoint.Summary, "检查点只承载历史摘要")
	require.Contains(t, result.Summary, splitTurnSeparator, "本轮上下文里的摘要还包含轮次前缀")
	require.Equal(t, 2, llm.calls, "历史摘要与轮次前缀摘要各一次")
}

func TestCompactCheckpointIsNilWhenNoStoredTurnWasSummarized(t *testing.T) {
	messages, _ := storedHistory(3, nil)
	// 视图里一个已持久化轮次都没有（例如历史装载失败或多轮被关闭）：压缩照常进行，
	// 但没有任何一行可以承载检查点。
	emptyView := NewHistoryView("req-0", "", "", 0)
	llm := &stubChat{response: "## Goal\n继续任务"}
	c := New(llm, testCounter(), noSplitSettings())
	require.NotNil(t, c)
	c.SetHistoryView(emptyView)

	result, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err)
	require.Nil(t, result.Checkpoint)
}

func TestCheckpointPrefixHashDetectsAnyEditInsideCoveredRange(t *testing.T) {
	_, base := storedHistory(4, nil)

	// 编辑 / 重新生成：改动被覆盖区间内任意一条消息的内容，链值都会变。
	for _, edited := range []func(turnID, role, content string) string{
		func(turnID, role, content string) string {
			if turnID == "req-0" && role == "user" {
				return content + "（已编辑）"
			}
			return content
		},
		func(turnID, role, content string) string {
			if turnID == "req-2" && role == "assistant" {
				return "重新生成的回答"
			}
			return content
		},
	} {
		_, mutated := storedHistory(4, edited)
		require.NotEqual(t, base.Turns[3].Chain, mutated.Turns[3].Chain,
			"前缀被改则检查点失效：链值必须不同")
	}

	// 撤回：覆盖区间少了一轮，链值随之不同。
	_, shorter := storedHistory(3, nil)
	require.NotEqual(t, base.Turns[3].Chain, shorter.Turns[2].Chain,
		"少一轮内容，前缀不再一致")

	// 未改动的部分必须稳定：重算得到的链值与保存时一致。
	_, again := storedHistory(4, nil)
	require.Equal(t, base.Turns[3].Chain, again.Turns[3].Chain)
}

func TestCheckpointChainExtendsFromReusedCheckpoint(t *testing.T) {
	// 复用上一份检查点时，链从它的 PrefixHash 继续延伸，锚点与基线照抄。延伸的结果
	// 必须与「从头逐轮折叠」完全相同，否则下一轮无法用重算来校验前缀。
	_, first := storedHistory(2, nil)
	reused := first.Turns[1].Chain

	messages, view := storedHistory(4, nil)
	extended := NewHistoryView(first.AnchorTurnID, "base-hash", reused, 2)
	for _, turn := range view.Turns[2:] {
		extended.AppendStoredTurn(turn.TurnID, turn.AssistantMessageID, []types.StoredMessage{
			{TurnID: turn.TurnID, Role: "user", Content: messages[turn.Start].Content},
			{TurnID: turn.TurnID, Role: "assistant", Content: messages[turn.Start+1].Content},
		}, turn.Start, turn.End)
	}
	require.Equal(t, 4, extended.Turns[len(extended.Turns)-1].CoveredTurns)
	require.Equal(t, view.Turns[2].Chain, extended.Turns[0].Chain, "延伸必须与从头折叠等价")
	require.Equal(t, view.Turns[3].Chain, extended.Turns[1].Chain)

	// 基线不同（更早的检查点被失效重建过）时，链值必须不同。
	rebased := NewHistoryView(first.AnchorTurnID, "other-base", reused, 2)
	require.NotEqual(t, view.Turns[2].Chain, rebased.Chain())
}

func TestStoredTurnEndingAtOnlyMatchesTurnBoundaries(t *testing.T) {
	messages, view := storedHistory(3, nil)
	require.Equal(t, "req-2", storedTurnEndingAt(view, len(messages)-1).TurnID,
		"活跃提问之前正好是最后一个已持久化轮次的结尾")
	require.Nil(t, storedTurnEndingAt(view, len(messages)), "活跃轮次没有持久化身份")
	require.Nil(t, storedTurnEndingAt(view, view.Turns[0].Start+1), "轮次内部不是边界")
	require.Nil(t, storedTurnEndingAt(view, 0), "系统提示之前没有历史")
	require.Nil(t, storedTurnEndingAt(nil, 4))
}

func TestHistoryViewShiftMovesSpansOnly(t *testing.T) {
	_, view := storedHistory(2, nil)
	shifted := view.Shift(1)
	require.Len(t, shifted.Turns, 2)
	for i := range view.Turns {
		require.Equal(t, view.Turns[i].Start+1, shifted.Turns[i].Start)
		require.Equal(t, view.Turns[i].End+1, shifted.Turns[i].End)
		require.Equal(t, view.Turns[i].Chain, shifted.Turns[i].Chain, "平移不得改动前缀哈希")
		require.Equal(t, view.Turns[i].TurnID, shifted.Turns[i].TurnID)
	}
	require.Equal(t, view.Turns[0].Start, view.Turns[0].Start, "原视图不得被就地修改")
	require.NotEqual(t, shifted.Turns[0].Start, view.Turns[0].Start)
	require.Same(t, view, view.Shift(0), "偏移为 0 时直接复用")
}

func TestFoldStoredTurnsMatchesPerTurnFolding(t *testing.T) {
	_, view := storedHistory(3, nil)
	stored := make([][]types.StoredMessage, 0, len(view.Turns))
	chain := types.CheckpointChainSeed(CheckpointPromptVersion, view.BaseHash, view.AnchorTurnID)
	for i := range view.Turns {
		stored = append(stored, []types.StoredMessage{{TurnID: view.Turns[i].TurnID, Role: "user", Content: "x"}})
		chain = types.FoldStoredMessages(chain, stored[i])
	}
	require.Equal(t, chain, FoldStoredTurns(
		types.CheckpointChainSeed(CheckpointPromptVersion, view.BaseHash, view.AnchorTurnID), stored))
}

func TestPrepareCarriesCheckpointIdentity(t *testing.T) {
	messages, view := storedHistory(4, nil)
	prep := Prepare(messages, noSplitSettings(), testCounter(), view)
	require.NotNil(t, prep)
	require.NotNil(t, prep.checkpointTurn)
	require.Equal(t, "req-2", prep.checkpointTurn.TurnID)
	require.Equal(t, "req-0", prep.AnchorTurnID)
	require.Empty(t, prep.BaseHash)

	cp := prep.checkpoint("摘要", false)
	require.NotNil(t, cp)
	require.Equal(t, "req-2", cp.TurnID)
	require.Equal(t, view.Turns[2].Chain, cp.PrefixHash)

	// 没有新摘要内容时，上一份检查点仍然成立，不覆盖。
	empty := &Preparation{checkpointTurn: prep.checkpointTurn}
	require.Nil(t, empty.checkpoint("摘要", false))
}

func TestCheckpointPromptVersionIsTiedToSummaryMarker(t *testing.T) {
	// 摘要信封标记变化必须让既有检查点整体失效，否则旧摘要会被当作自己的产物继续折叠。
	require.Contains(t, CheckpointPromptVersion, SummaryMarker)
}
