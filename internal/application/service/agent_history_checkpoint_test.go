package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-10 在历史装载侧的语义：从检查点起读、前缀严格一致才复用、
// 不一致则丢弃检查点退回全量重算，以及检查点写入通道。

// checkpointHistoryRepo 是只实现历史装载所需方法的消息仓储替身。
type checkpointHistoryRepo struct {
	interfaces.MessageRepository
	rows       []*types.Message
	checkpoint *types.Message
	saved      map[string]*types.ContextCheckpoint
	saveErr    error
	lastLimit  int
}

func (r *checkpointHistoryRepo) GetRecentMessagesBySession(
	_ context.Context, sessionID string, limit int,
) ([]*types.Message, error) {
	r.lastLimit = limit
	out := make([]*types.Message, 0, len(r.rows))
	for _, msg := range r.rows {
		if msg.SessionID == sessionID {
			out = append(out, msg)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (r *checkpointHistoryRepo) GetLatestContextCheckpoint(
	_ context.Context, _ string,
) (*types.Message, error) {
	return r.checkpoint, nil
}

func (r *checkpointHistoryRepo) UpdateMessageContextCheckpoint(
	_ context.Context, sessionID, messageID string, checkpoint *types.ContextCheckpoint,
) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	if r.saved == nil {
		r.saved = map[string]*types.ContextCheckpoint{}
	}
	r.saved[sessionID+"/"+messageID] = checkpoint
	return nil
}

// checkpointSeedTurn 造一轮已持久化消息（user + 已完成 assistant）。
func checkpointSeedTurn(requestID, question, answer string, at time.Time) [2]*types.Message {
	return [2]*types.Message{
		{
			ID: "u-" + requestID, SessionID: "s1", RequestID: requestID,
			Role: "user", Content: question, CreatedAt: at,
		},
		{
			ID: "a-" + requestID, SessionID: "s1", RequestID: requestID,
			Role: "assistant", Content: answer, CreatedAt: at.Add(time.Second), IsCompleted: true,
		},
	}
}

// checkpointSeedSession 造 count 轮连续对话。
func checkpointSeedSession(count int) [][2]*types.Message {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	turns := make([][2]*types.Message, 0, count)
	for i := 0; i < count; i++ {
		turns = append(turns, checkpointSeedTurn(
			fmt.Sprintf("req-%d", i),
			fmt.Sprintf("第%d轮提问", i),
			fmt.Sprintf("第%d轮回答", i),
			base.Add(time.Duration(i)*time.Hour),
		))
	}
	return turns
}

// checkpointCovering 按装载侧完全相同的口径算出覆盖 turns[:through+1] 的检查点，
// 并把它挂在被覆盖最后一轮的 assistant 行上。
func checkpointCovering(turns [][2]*types.Message, through int, summary string) *types.Message {
	stored := make([][]types.StoredMessage, 0, through+1)
	for _, pair := range turns[:through+1] {
		stored = append(stored, []types.StoredMessage{
			{TurnID: pair[0].RequestID, Role: pair[0].Role, Content: pair[0].Content},
			{TurnID: pair[1].RequestID, Role: pair[1].Role, Content: pair[1].Content},
		})
	}
	row := *turns[through][1]
	row.ContextCheckpoint = &types.ContextCheckpoint{
		Summary:   summary,
		CreatedAt: time.Now(),
		PrefixHash: compaction.FoldStoredTurns(
			types.CheckpointChainSeed(compaction.CheckpointPromptVersion, "", turns[0][0].RequestID), stored),
		AnchorTurnID: turns[0][0].RequestID,
		CoveredTurns: through + 1,
		Source:       types.ContextCheckpointSourceAgent,
	}
	return &row
}

func checkpointRepoWith(turns [][2]*types.Message, checkpoint *types.Message) *checkpointHistoryRepo {
	repo := &checkpointHistoryRepo{checkpoint: checkpoint}
	for _, pair := range turns {
		repo.rows = append(repo.rows, pair[0], pair[1])
	}
	return repo
}

func TestLoadAgentHistoryReusesCheckpointAndStartsAfterIt(t *testing.T) {
	turns := checkpointSeedSession(4)
	// 检查点覆盖前两轮，第三、四轮仍要原样重放。
	repo := checkpointRepoWith(turns, checkpointCovering(turns, 1, "已有摘要"))

	loaded, err := LoadAgentHistory(context.Background(), repo, "s1", 5)
	require.NoError(t, err)
	require.True(t, loaded.Reused, "前缀一致时必须复用检查点")
	require.Equal(t, "已有摘要", loaded.Summary)
	require.Equal(t, "a-req-1", loaded.CheckpointMessageID)

	require.Len(t, loaded.Messages, 5, "一条摘要占位 + 两轮各两条消息")
	require.True(t, compaction.IsSummaryMessage(&loaded.Messages[0]),
		"摘要必须以压缩占位消息的形式参与后续对话")
	require.Contains(t, loaded.Messages[0].Content, "已有摘要")
	require.Equal(t, "第2轮提问", loaded.Messages[1].Content)
	require.Equal(t, "第3轮提问", loaded.Messages[3].Content)

	// 视图接着检查点往后延伸：锚点与基线照抄，链值从检查点继续。
	view := loaded.View
	require.NotNil(t, view)
	require.Equal(t, "req-0", view.AnchorTurnID)
	require.Equal(t, 2, view.CoveredTurns)
	require.Len(t, view.Turns, 2)
	require.Equal(t, "a-req-2", view.Turns[0].AssistantMessageID)
	require.Equal(t, 1, view.Turns[0].Start, "摘要占位占了第 0 条")
	require.Equal(t, 3, view.Turns[0].End)
	require.Equal(t, 3, view.Turns[0].CoveredTurns, "覆盖轮次数接着检查点往后累加")
	require.Equal(t, 4, view.Turns[1].CoveredTurns)
	require.Equal(t, types.FoldStoredMessages(repo.checkpoint.ContextCheckpoint.PrefixHash,
		[]types.StoredMessage{
			{TurnID: "req-2", Role: "user", Content: "第2轮提问"},
			{TurnID: "req-2", Role: "assistant", Content: "第2轮回答"},
		}), view.Turns[0].Chain)

	require.GreaterOrEqual(t, repo.lastLimit, 2*2+5*4,
		"抓取窗口必须装得下被覆盖的轮次，否则无法校验前缀")
}

func TestLoadAgentHistoryDiscardsCheckpointWhenCoveredContentChanged(t *testing.T) {
	turns := checkpointSeedSession(4)
	checkpoint := checkpointCovering(turns, 1, "已有摘要")
	// 编辑被覆盖区间内的一条消息（撤回/重新生成同理）：前缀不再一致。
	turns[0][0].Content = "第0轮提问（已编辑）"
	repo := checkpointRepoWith(turns, checkpoint)

	loaded, err := LoadAgentHistory(context.Background(), repo, "s1", 5)
	require.NoError(t, err)
	require.False(t, loaded.Reused, "前缀被改则检查点失效")
	require.Empty(t, loaded.Summary)
	require.Len(t, loaded.Messages, 8, "四轮全部原样重放")
	require.False(t, compaction.IsSummaryMessage(&loaded.Messages[0]))

	require.Equal(t, "req-0", loaded.View.AnchorTurnID)
	require.Zero(t, loaded.View.CoveredTurns)
	require.Len(t, loaded.View.Turns, 4)
}

func TestLoadAgentHistoryDiscardsCheckpointWhenBoundaryTurnIsMissing(t *testing.T) {
	turns := checkpointSeedSession(4)
	checkpoint := checkpointCovering(turns, 1, "已有摘要")
	// 检查点所在的那一轮已经不在装载结果里（例如被删除后未失效干净）。
	repo := checkpointRepoWith(turns[2:], checkpoint)

	loaded, err := LoadAgentHistory(context.Background(), repo, "s1", 5)
	require.NoError(t, err)
	require.False(t, loaded.Reused)
	require.Len(t, loaded.Messages, 4, "只剩两轮可重放")
	require.Equal(t, "req-2", loaded.View.AnchorTurnID)
}

func TestLoadAgentHistoryWithoutCheckpointBuildsViewOverLoadedTurns(t *testing.T) {
	turns := checkpointSeedSession(5)
	repo := checkpointRepoWith(turns, nil)

	loaded, err := LoadAgentHistory(context.Background(), repo, "s1", 3)
	require.NoError(t, err)
	require.False(t, loaded.Reused)
	require.Empty(t, loaded.Summary)
	require.Len(t, loaded.Messages, 6, "窗口只保留最新三轮")

	view := loaded.View
	require.NotNil(t, view)
	require.Equal(t, "req-2", view.AnchorTurnID, "锚点是本轮装载到的最早一轮")
	require.Empty(t, view.BaseHash)
	require.Zero(t, view.CoveredTurns)
	require.Len(t, view.Turns, 3)
	require.Equal(t, 0, view.Turns[0].Start)
	require.Equal(t, 2, view.Turns[0].End)
	require.Equal(t, 3, view.Turns[2].CoveredTurns)

	// 视图里的链值必须能被同样的输入重算出来——这正是下一轮校验前缀的依据。
	require.Equal(t, compaction.FoldStoredTurns(
		types.CheckpointChainSeed(compaction.CheckpointPromptVersion, "", "req-2"),
		[][]types.StoredMessage{
			{{TurnID: "req-2", Role: "user", Content: "第2轮提问"},
				{TurnID: "req-2", Role: "assistant", Content: "第2轮回答"}},
			{{TurnID: "req-3", Role: "user", Content: "第3轮提问"},
				{TurnID: "req-3", Role: "assistant", Content: "第3轮回答"}},
			{{TurnID: "req-4", Role: "user", Content: "第4轮提问"},
				{TurnID: "req-4", Role: "assistant", Content: "第4轮回答"}},
		}), view.Turns[2].Chain)
}

func TestLoadAgentHistoryWithoutRowsReturnsEmpty(t *testing.T) {
	repo := &checkpointHistoryRepo{}
	loaded, err := LoadAgentHistory(context.Background(), repo, "s1", 5)
	require.NoError(t, err)
	require.Empty(t, loaded.Messages)
	require.Nil(t, loaded.View)

	loaded, err = LoadAgentHistory(context.Background(), repo, "s1", 0)
	require.NoError(t, err)
	require.Empty(t, loaded.Messages, "多轮关闭时不装载历史")
}

func TestMessageCheckpointSinkSavesThroughRepository(t *testing.T) {
	repo := &checkpointHistoryRepo{}
	sink := NewMessageCheckpointSink(repo, "s1")
	require.NotNil(t, sink)

	cp := &types.ContextCheckpoint{Summary: "摘要", PrefixHash: "hash"}
	require.NoError(t, sink.SaveContextCheckpoint(context.Background(), "a-req-1", cp))
	require.Equal(t, cp, repo.saved["s1/a-req-1"])

	// 写入失败必须原样上报，由引擎记日志（本轮不受影响，下一轮重算一次摘要）。
	repo.saveErr = fmt.Errorf("db down")
	require.Error(t, sink.SaveContextCheckpoint(context.Background(), "a-req-1", cp))

	require.Nil(t, NewMessageCheckpointSink(nil, "s1"), "没有仓储就没有写入通道")
}
