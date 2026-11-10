package chatpipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-10 在 Map-Reduce 增量摘要上的缓存与失效：摘要结果写回持久化检查点，
// 下一轮直接复用而不再调用摘要模型；前缀被改则丢弃检查点、全量重算。

// checkpointMessageService 是只实现历史装载所需方法的消息服务替身。
type checkpointMessageService struct {
	interfaces.MessageService
	rows       []*types.Message
	checkpoint *types.Message
	saved      map[string]*types.ContextCheckpoint
}

func (s *checkpointMessageService) GetRecentMessagesBySession(
	_ context.Context, sessionID string, limit int,
) ([]*types.Message, error) {
	out := make([]*types.Message, 0, len(s.rows))
	for _, msg := range s.rows {
		if msg.SessionID == sessionID {
			out = append(out, msg)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (s *checkpointMessageService) GetLatestContextCheckpoint(
	_ context.Context, _ string,
) (*types.Message, error) {
	return s.checkpoint, nil
}

func (s *checkpointMessageService) SaveContextCheckpoint(
	_ context.Context, sessionID, messageID string, checkpoint *types.ContextCheckpoint,
) error {
	if s.saved == nil {
		s.saved = map[string]*types.ContextCheckpoint{}
	}
	s.saved[sessionID+"/"+messageID] = checkpoint
	return nil
}

// countingSummaryChat 记录摘要模型被调用的次数。
type countingSummaryChat struct {
	calls int
	reply string
}

func (m *countingSummaryChat) Chat(
	_ context.Context, _ []chat.Message, _ *chat.ChatOptions,
) (*types.ChatResponse, error) {
	m.calls++
	return &types.ChatResponse{Content: m.reply}, nil
}

func (m *countingSummaryChat) ChatStream(
	context.Context, []chat.Message, *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, nil
}

func (m *countingSummaryChat) GetModelName() string { return "summary-model" }
func (m *countingSummaryChat) GetModelID() string   { return "summary-model" }

// checkpointPipelineSeed 造 count 轮问答（每轮 user + assistant 两行）。
func checkpointPipelineSeed(count int) (*checkpointMessageService, [][2]string) {
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	svc := &checkpointMessageService{}
	ids := make([][2]string, 0, count)
	for i := 0; i < count; i++ {
		requestID := fmt.Sprintf("req-%d", i)
		at := base.Add(time.Duration(i) * time.Hour)
		userID, assistantID := "u-"+requestID, "a-"+requestID
		svc.rows = append(svc.rows,
			&types.Message{
				ID: userID, SessionID: "s1", RequestID: requestID, Role: "user",
				Content: fmt.Sprintf("第%d轮提问", i), CreatedAt: at,
			},
			&types.Message{
				ID: assistantID, SessionID: "s1", RequestID: requestID, Role: "assistant",
				Content: fmt.Sprintf("第%d轮回答", i), CreatedAt: at.Add(time.Second), IsCompleted: true,
			},
		)
		ids = append(ids, [2]string{userID, assistantID})
	}
	return svc, ids
}

func checkpointChatManage(recentRounds, fetchRounds int) *types.ChatManage {
	// ChatManage 由三个内嵌结构组成，提升字段不能写进字面量，只能逐个赋值。
	cm := &types.ChatManage{}
	cm.SessionID = "s1"
	cm.MaxRounds = recentRounds
	cm.ChatModelID = "summary-model"
	cm.ChatModelName = "qwen3-32b"
	cm.GlobalContextConfig = &types.ContextConfig{
		CompressionStrategy: types.ContextCompressionSmart,
		RecentMessageCount:  recentRounds,
		SummarizeThreshold:  fetchRounds,
	}
	return cm
}

func TestLoadSmartPersistsSummaryAsCheckpointAndReusesIt(t *testing.T) {
	svc, ids := checkpointPipelineSeed(8)
	summarizer := &countingSummaryChat{reply: "运行摘要"}
	plugin := &PluginLoadHistory{
		messageService: svc,
		modelService:   &stubModelService{model: summarizer},
	}
	next := func() *PluginError { return nil }

	// 第一轮：没有检查点，8 轮里最新 2 轮保留，其余 6 轮被折进摘要。
	chatManage := checkpointChatManage(2, 8)
	require.Nil(t, plugin.OnEvent(context.Background(), types.LOAD_HISTORY, chatManage, next))
	require.Equal(t, "运行摘要", chatManage.HistorySummary)
	require.Len(t, chatManage.History, 2)
	require.Equal(t, "第6轮提问", chatManage.History[0].Query)
	require.Greater(t, summarizer.calls, 0, "第一轮必须调用摘要模型")

	saved := svc.saved["s1/"+ids[5][1]]
	require.NotNil(t, saved, "检查点必须落在被覆盖的最后一轮的 assistant 行上")
	require.Equal(t, "运行摘要", saved.Summary)
	require.Equal(t, "req-0", saved.AnchorTurnID)
	require.Empty(t, saved.BaseHash)
	require.Equal(t, 6, saved.CoveredTurns)
	require.NotEmpty(t, saved.PrefixHash)
	require.Equal(t, types.ContextCheckpointSourceSmart, saved.Source)

	// 第二轮：把检查点交回去，同样的历史不得再调用一次摘要模型。
	svc.checkpoint = &types.Message{
		ID: ids[5][1], SessionID: "s1", RequestID: "req-5",
		Role: "assistant", ContextCheckpoint: saved,
	}
	callsBefore := summarizer.calls
	reused := checkpointChatManage(2, 8)
	require.Nil(t, plugin.OnEvent(context.Background(), types.LOAD_HISTORY, reused, next))
	require.Equal(t, summarizer.calls, callsBefore, "复用检查点时不得重复调用摘要模型")
	require.Equal(t, "运行摘要", reused.HistorySummary, "持久化摘要直接参与后续对话")
	require.Len(t, reused.History, 2, "只重放检查点之后的轮次")
	require.Equal(t, "第6轮提问", reused.History[0].Query)
	require.Len(t, svc.saved, 1, "没有新覆盖的轮次时不写新检查点")
}

func TestLoadSmartDiscardsCheckpointWhenCoveredContentChanged(t *testing.T) {
	svc, ids := checkpointPipelineSeed(8)
	summarizer := &countingSummaryChat{reply: "运行摘要"}
	plugin := &PluginLoadHistory{
		messageService: svc,
		modelService:   &stubModelService{model: summarizer},
	}
	next := func() *PluginError { return nil }

	chatManage := checkpointChatManage(2, 8)
	require.Nil(t, plugin.OnEvent(context.Background(), types.LOAD_HISTORY, chatManage, next))
	saved := svc.saved["s1/"+ids[5][1]]
	require.NotNil(t, saved)

	svc.checkpoint = &types.Message{
		ID: ids[5][1], SessionID: "s1", RequestID: "req-5",
		Role: "assistant", ContextCheckpoint: saved,
	}
	// 编辑被覆盖区间内的一条消息：前缀不再严格一致，检查点必须失效。
	svc.rows[2].Content = "第1轮提问（已编辑）"

	callsBefore := summarizer.calls
	reused := checkpointChatManage(2, 8)
	require.Nil(t, plugin.OnEvent(context.Background(), types.LOAD_HISTORY, reused, next))
	require.Greater(t, summarizer.calls, callsBefore, "检查点失效后必须全量重算")
	require.Len(t, reused.History, 2)
	require.NotEmpty(t, reused.HistorySummary)

	// 重算之后写下的是一份新检查点，前缀哈希与旧的不同。
	refreshed := svc.saved["s1/"+ids[5][1]]
	require.NotNil(t, refreshed)
	require.NotEqual(t, saved.PrefixHash, refreshed.PrefixHash)
}

func TestLoadSmartDoesNotClaimRoundsItDidNotSummarize(t *testing.T) {
	svc, _ := checkpointPipelineSeed(8)
	// 摘要模型不可用（返回错误）：SmartCompress 报错后走滑动窗口兜底，
	// 绝不能把没有摘要覆盖的轮次写进检查点。
	plugin := &PluginLoadHistory{
		messageService: svc,
		modelService:   &stubModelService{model: &failingSummaryChat{}},
	}
	chatManage := checkpointChatManage(2, 8)
	require.Nil(t, plugin.OnEvent(context.Background(), types.LOAD_HISTORY, chatManage,
		func() *PluginError { return nil }))
	require.Empty(t, svc.saved, "摘要失败时不得写下检查点")
	require.Len(t, chatManage.History, 2, "兜底为滑动窗口")
}

// failingSummaryChat 让每次摘要调用都失败。
type failingSummaryChat struct{}

func (m *failingSummaryChat) Chat(
	context.Context, []chat.Message, *chat.ChatOptions,
) (*types.ChatResponse, error) {
	return nil, fmt.Errorf("摘要模型不可用")
}

func (m *failingSummaryChat) ChatStream(
	context.Context, []chat.Message, *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	return nil, nil
}

func (m *failingSummaryChat) GetModelName() string { return "broken" }
func (m *failingSummaryChat) GetModelID() string   { return "broken" }

func TestStickyVerbatimBlockIsBounded(t *testing.T) {
	require.Empty(t, stickyVerbatimBlock(nil, 0), "没有 sticky 轮次就不附加原文块")

	kept := []contextx.Turn{
		{User: "预算多少", Assistant: "决定：12 万元", Sticky: true},
		{User: "新问题", Assistant: "新回答"},
	}
	block := stickyVerbatimBlock(kept, 1)
	require.Contains(t, block, "决定：12 万元", "sticky 轮次的原文必须随检查点一起留下")
	require.Contains(t, block, "原样保留")
	require.NotContains(t, block, "新问题", "保留窗口内的轮次下一轮还会重放，不必附原文")

	// 超出上限时只写入放得下的部分，绝不无界增长。
	big := make([]contextx.Turn, 0, 200)
	for i := 0; i < 200; i++ {
		big = append(big, contextx.Turn{
			User:      strings.Repeat("问", 200),
			Assistant: strings.Repeat("答", 200),
			Sticky:    true,
		})
	}
	require.LessOrEqual(t, len(stickyVerbatimBlock(big, 200)), maxStickyVerbatimBytes+len("\n\n[以下轮次因包含决定/期限/关键数字而原样保留]\n"))
}
