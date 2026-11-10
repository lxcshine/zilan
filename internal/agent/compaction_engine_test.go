package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-06 在 engine 侧的挂载点：溢出压缩重试、重试上限、降级告知、
// 多模态优先降图、取消不调模型、最终答案合成前压缩。

// overflowPromptTooLong 是一条真实的溢出错报文，且刻意不含任何瞬时错误标记
// （429/500/502/503/504/timeout/connection 等），以免落进瞬时重试分支。
const overflowPromptTooLong = "prompt is too long: 120000 tokens > 100000 maximum"

// compactionFakeChat 同时充当 Agent 轮次的流式模型与压缩器的摘要模型，
// 并记录每次非流式调用的选项，用于验证摘要请求不携带 tools。
type compactionFakeChat struct {
	// failStreams 是前 N 次流式调用返回溢出错误的次数。
	failStreams int
	summary     string

	streamCalls  int
	summaryCalls int
	summaryOpts  []*chat.ChatOptions
}

func (m *compactionFakeChat) ChatStream(
	_ context.Context, _ []chat.Message, _ *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	m.streamCalls++
	ch := make(chan types.StreamResponse, 1)
	if m.streamCalls <= m.failStreams {
		ch <- types.StreamResponse{ResponseType: types.ResponseTypeError, Content: overflowPromptTooLong}
		close(ch)
		return ch, nil
	}
	ch <- types.StreamResponse{
		ResponseType: types.ResponseTypeAnswer,
		Content:      "恢复后的回答",
		FinishReason: "stop",
		Done:         true,
	}
	close(ch)
	return ch, nil
}

func (m *compactionFakeChat) Chat(
	_ context.Context, _ []chat.Message, opts *chat.ChatOptions,
) (*types.ChatResponse, error) {
	m.summaryCalls++
	m.summaryOpts = append(m.summaryOpts, opts)
	return &types.ChatResponse{Content: m.summary}, nil
}

func (m *compactionFakeChat) GetModelName() string { return "qwen3-32b" }
func (m *compactionFakeChat) GetModelID() string   { return "compaction-fake" }

// compactionTestSettings 是一组便于测试的小窗口配置：阈值 3500，保留预算只够最后一条消息。
func compactionTestSettings() compaction.Settings {
	return compaction.Settings{
		Enabled:          true,
		MaxContextTokens: 4000,
		ReserveTokens:    500,
		KeepRecentTokens: 20,
	}
}

func newCompactionEngine(t *testing.T, model chat.Chat) *AgentEngine {
	t.Helper()
	engine := newTestEngine(t, model)
	engine.compactor = compaction.New(model, engine.govCounter(), compactionTestSettings())
	require.NotNil(t, engine.compactor)
	return engine
}

// bigHistory 构造一段远超测试阈值的 ReAct 历史，工具调用与工具结果严格配对。
func bigHistory() []chat.Message {
	payload := strings.Repeat("检索结果 ", 60)
	messages := []chat.Message{{Role: "system", Content: "You are the Zilan agent."}}
	for i := 0; i < 10; i++ {
		id := "call-" + string(rune('a'+i))
		messages = append(messages,
			chat.Message{Role: "user", Content: "第" + string(rune('0'+i)) + "轮提问 " + payload},
			chat.Message{
				Role: "assistant",
				ToolCalls: []chat.ToolCall{{
					ID: id, Type: "function",
					Function: chat.FunctionCall{Name: "knowledge_search", Arguments: `{"query":"q"}`},
				}},
			},
			chat.Message{Role: "tool", ToolCallID: id, Name: "knowledge_search", Content: payload},
		)
	}
	return append(messages, chat.Message{Role: "user", Content: "最新提问"})
}

// collectNotices 订阅思考流事件，收集推给用户的可见提示。
func collectNotices(engine *AgentEngine) *[]string {
	notices := &[]string{}
	engine.eventBus.On(event.EventAgentThought, func(_ context.Context, evt event.Event) error {
		if data, ok := evt.Data.(event.AgentThoughtData); ok && data.Content != "" {
			*notices = append(*notices, data.Content)
		}
		return nil
	})
	return notices
}

func joined(notices *[]string) string { return strings.Join(*notices, "\n") }

// ---------------------------------------------------------------------------
// 规则 1 + 规则 4：溢出后压缩重试，且每 turn 只此一次
// ---------------------------------------------------------------------------

func TestCallLLMWithRetryCompactsAndRetriesOnceOnOverflow(t *testing.T) {
	model := &compactionFakeChat{failStreams: 1, summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	messages := bigHistory()
	before := len(messages)

	resp, err := engine.callLLMWithRetry(
		context.Background(), &messages, nil, &types.AgentState{}, "q", 0, "session")
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, "恢复后的回答", resp.Content)

	require.Equal(t, 2, model.streamCalls, "溢出后只重试一次")
	require.Positive(t, model.summaryCalls, "重试前必须真的做过压缩")
	require.True(t, engine.overflowRecovered)

	// 压缩后的列表必须留在调用方手里，否则下一轮会重建刚被拒绝的请求。
	require.Less(t, len(messages), before)
	require.True(t, compaction.IsSummaryMessage(&messages[1]),
		"被压缩的内容必须以明确占位说明呈现")
	require.Equal(t, "system", messages[0].Role)
	require.Equal(t, "最新提问", messages[len(messages)-1].Content)
}

func TestCallLLMWithRetryDoesNotRetryOverflowTwice(t *testing.T) {
	model := &compactionFakeChat{failStreams: 99, summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	messages := bigHistory()

	resp, err := engine.callLLMWithRetry(
		context.Background(), &messages, nil, &types.AgentState{}, "q", 0, "session")
	require.Error(t, err, "重试后仍溢出则按失败处理")
	require.Nil(t, resp)
	require.Equal(t, 2, model.streamCalls, "每 turn 最多一次「压缩后重试」")
	require.True(t, engine.overflowRecovered)

	// 同一 turn 内第二次溢出不再触发压缩重试。
	model.streamCalls = 0
	_, err = engine.callLLMWithRetry(
		context.Background(), &messages, nil, &types.AgentState{}, "q", 1, "session")
	require.Error(t, err)
	require.Equal(t, 1, model.streamCalls)
}

func TestOverflowErrorIsNotTreatedAsTransient(t *testing.T) {
	// 限流报文虽然提到 token，但绝不能被识别为溢出：否则会拿压缩后的上下文去重试
	// 一个只需要退避的请求。
	throttling := errString("ThrottlingException: Too many tokens, please wait before trying again")
	require.False(t, compaction.IsOverflowError(throttling))
	require.False(t, compaction.IsOverflowError(
		errString("429 Too Many Requests: rate limit exceeded, too many tokens")))

	// 溢出报文不得同时命中瞬时错误标记，否则会先被瞬时重试分支消耗掉次数。
	overflow := errString(overflowPromptTooLong)
	require.True(t, compaction.IsOverflowError(overflow))
	require.False(t, isTransientError(overflow))
}

type errString string

func (e errString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// 规则 8：多模态溢出优先降图，而不是压缩文本历史
// ---------------------------------------------------------------------------

func imagePart(url string) chat.MessageContentPart {
	return chat.MessageContentPart{Type: "image_url", ImageURL: &chat.ImageURL{URL: url}}
}

func TestManageContextWindowDropsImagesBeforeCompactingText(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n不该被调用"}
	engine := newCompactionEngine(t, model)
	notices := collectNotices(engine)

	// 4 张图 × 1024 token 的保守估值已越过 3500 的阈值，文本本身很小。
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
	settings := engine.compactor.Settings()
	currentTokens := engine.countContextTokens(messages)
	require.True(t, settings.ShouldCompact(currentTokens), "前置条件：必须已越阈")

	out, changed := engine.manageContextWindow(context.Background(), messages, 1, currentTokens)
	require.True(t, changed)
	require.Zero(t, model.summaryCalls, "图片导致的溢出不得先压缩文本历史")
	require.Equal(t, 2, compaction.CountImages(out))
	require.Zero(t, imageCount(&out[1]), "最早的非必要图片被移除")
	require.Equal(t, 2, imageCount(&out[3]), "本轮上传的图片是必要图片")
	require.Contains(t, joined(notices), "图片")
	require.False(t, settings.ShouldCompact(engine.countContextTokens(out)))
}

func imageCount(msg *chat.Message) int {
	n := len(msg.Images)
	for i := range msg.MultiContent {
		if msg.MultiContent[i].Type == "image_url" && msg.MultiContent[i].ImageURL != nil {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 规则 5：压缩无效时降级截断，并明确告知用户
// ---------------------------------------------------------------------------

func TestManageContextWindowTellsUserWhenEarlyStepsAreDropped(t *testing.T) {
	// 摘要器和原文一样长 → 释放量不足 1/20 → 判定压缩无效 → 降级为规则式截断。
	model := &compactionFakeChat{summary: strings.Repeat("冗长摘要 ", 4000)}
	engine := newCompactionEngine(t, model)
	notices := collectNotices(engine)

	messages := bigHistory()
	currentTokens := engine.countContextTokens(messages)
	require.True(t, engine.compactor.Settings().ShouldCompact(currentTokens))

	out, changed := engine.manageContextWindow(context.Background(), messages, 1, currentTokens)
	require.True(t, changed)
	require.Less(t, len(out), len(messages))
	require.Equal(t, "system", out[0].Role)
	require.Contains(t, out[1].Content, compaction.TruncateMarker)

	text := joined(notices)
	require.Contains(t, text, "早期步骤已被丢弃", "降级不得静默，必须让用户看见")
	require.Contains(t, text, "可能影响后续推理")
	require.Contains(t, out[1].Content, "早期步骤已被丢弃")
	require.Positive(t, engine.compactionExhaustedAt, "压缩无效后不得在同一规模下反复重试")
}

func TestCompactionEmitsVisibleCompactedNotice(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)
	notices := collectNotices(engine)

	messages := bigHistory()
	out, ok := engine.runCompaction(context.Background(), messages, 1, compaction.ReasonThreshold)
	require.True(t, ok)
	require.Less(t, len(out), len(messages))
	require.Contains(t, joined(notices), "已压缩早期步骤")
	require.Zero(t, engine.compactionExhaustedAt)
	require.Zero(t, engine.lastSentMsgCount, "压缩后用量基线必须清零，否则下一轮立刻再压一次")
}

// ---------------------------------------------------------------------------
// 规则 9：取消语义
// ---------------------------------------------------------------------------

func TestRunCompactionSkipsModelCallWhenCancelled(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, ok := engine.runCompaction(ctx, bigHistory(), 1, compaction.ReasonOverflow)
	require.False(t, ok)
	require.Zero(t, model.summaryCalls, "取消后不得发起任何模型调用")
	require.Zero(t, engine.compactionExhaustedAt, "取消不是「无可压缩」，不得置耗尽标记")
	require.Equal(t, len(bigHistory()), len(out))
}

func TestCompactFinalAnswerContextSkipsWorkWhenCancelled(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	messages := bigHistory()
	out := engine.compactFinalAnswerContext(ctx, messages, 3)
	require.Equal(t, messages, out)
	require.Zero(t, model.summaryCalls)
}

// ---------------------------------------------------------------------------
// 规则 7：最终答案合成前压缩，且不重放工具调用
// ---------------------------------------------------------------------------

func TestCompactFinalAnswerContextCompactsWithoutReplayingTools(t *testing.T) {
	model := &compactionFakeChat{summary: "## Goal\n继续任务"}
	engine := newCompactionEngine(t, model)

	// 最终答案合成的消息形态：系统提示 + 用户问题 + 每条工具结果渲染为 user 消息 + 合成指令。
	payload := strings.Repeat("工具产出 ", 400)
	messages := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "用户问题"},
	}
	for i := 0; i < 6; i++ {
		messages = append(messages, chat.Message{
			Role: "user", Content: "Tool knowledge_search returned: " + payload,
		})
	}
	finalPrompt := "Based on the above tool call results, generate a complete answer."
	messages = append(messages, chat.Message{Role: "user", Content: finalPrompt})

	require.True(t, engine.compactor.Settings().ShouldCompact(engine.countContextTokens(messages)))

	out := engine.compactFinalAnswerContext(context.Background(), messages, 6)
	require.Less(t, len(out), len(messages))
	require.Equal(t, "system", out[0].Role)
	require.True(t, compaction.IsSummaryMessage(&out[1]))
	require.Equal(t, finalPrompt, out[len(out)-1].Content, "合成指令必须原样保留在最后")

	require.Positive(t, model.summaryCalls)
	for _, opts := range model.summaryOpts {
		require.Empty(t, opts.Tools, "压缩过程不得携带工具，因而不可能重放工具调用")
	}
}
