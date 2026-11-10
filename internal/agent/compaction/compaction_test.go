package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 测试替身与构造器
// ---------------------------------------------------------------------------

// stubChat 提供固定的摘要响应，并记录每一次调用，用于验证重试上限与取消语义。
type stubChat struct {
	response string
	err      error
	finish   string
	calls    int
	prompts  []string
	maxTok   []int
	tools    []int
}

func (s *stubChat) Chat(_ context.Context, messages []chat.Message, opts *chat.ChatOptions) (*types.ChatResponse, error) {
	s.calls++
	for _, m := range messages {
		if m.Role == "user" {
			s.prompts = append(s.prompts, m.Content)
		}
	}
	if opts != nil {
		s.maxTok = append(s.maxTok, opts.MaxTokens)
		s.tools = append(s.tools, len(opts.Tools))
	}
	if s.err != nil {
		return nil, s.err
	}
	return &types.ChatResponse{Content: s.response, FinishReason: s.finish}, nil
}

func (s *stubChat) ChatStream(context.Context, []chat.Message, *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *stubChat) GetModelName() string { return "qwen3-32b" }
func (s *stubChat) GetModelID() string   { return "stub" }

func testCounter() *contextx.Counter { return contextx.NewCounter(contextx.VendorOpenAI) }

func testSettings() Settings {
	return Settings{
		Enabled:          true,
		MaxContextTokens: 4000,
		ReserveTokens:    500,
		KeepRecentTokens: 400,
	}
}

// noSplitSettings 把保留预算压到只够最后一条 user 消息，使切点必然落在轮次边界上，
// 于是摘要调用次数确定为 1（testSettings 下切点会落在轮次内部，额外产生一次轮次前缀摘要）。
func noSplitSettings() Settings {
	s := testSettings()
	s.KeepRecentTokens = 20
	return s
}

func toolCall(id, name, args string) chat.ToolCall {
	return chat.ToolCall{
		ID:       id,
		Type:     "function",
		Function: chat.FunctionCall{Name: name, Arguments: args},
	}
}

// reactHistory 构造一段典型的 ReAct 历史：system + 若干「user / assistant(工具调用) /
// 工具结果」三元组，最后以一条 user 消息收尾。
func reactHistory(rounds int, payload string) []chat.Message {
	messages := []chat.Message{{Role: "system", Content: "You are the Zilan agent."}}
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("call-%d", i)
		messages = append(messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("question %d %s", i, payload)},
			chat.Message{
				Role:      "assistant",
				Content:   fmt.Sprintf("thought %d", i),
				ToolCalls: []chat.ToolCall{toolCall(id, "knowledge_search", `{"query":"q"}`)},
			},
			chat.Message{Role: "tool", ToolCallID: id, Name: "knowledge_search", Content: payload},
		)
	}
	return append(messages, chat.Message{Role: "user", Content: "final question"})
}

// requireToolPairingIntact 断言保留区间内每条工具结果都能在保留区间内找到发起它的
// assistant 消息，且第一条被保留的消息不是工具结果（PRD FR-06 规则 2）。
func requireToolPairingIntact(t *testing.T, messages []chat.Message, from int) {
	t.Helper()
	require.Less(t, from, len(messages))
	require.NotEqual(t, "tool", messages[from].Role,
		"切点不得落在工具结果上，否则该结果与发起它的 assistant 消息被拆开")
	for i := from; i < len(messages); i++ {
		if messages[i].Role != "tool" {
			continue
		}
		found := false
		for j := from; j < i; j++ {
			for _, tc := range messages[j].ToolCalls {
				if tc.ID == messages[i].ToolCallID {
					found = true
				}
			}
		}
		require.True(t, found, "msg[%d] 是孤儿工具结果 (tool_call_id=%s)", i, messages[i].ToolCallID)
	}
}

// ---------------------------------------------------------------------------
// 规则 2：切点安全性
// ---------------------------------------------------------------------------

func TestFindCutPointNeverSplitsToolPairing(t *testing.T) {
	counter := testCounter()
	messages := reactHistory(6, strings.Repeat("检索结果 ", 30))
	total := CountMessages(counter, messages)

	// 扫遍从 0 到全量的每一种保留预算：任何预算下切点都必须是安全的。
	for budget := 0; budget <= total+200; budget += 37 {
		cut := FindCutPoint(messages, historyStart(messages), budget, counter)
		require.GreaterOrEqual(t, cut.FirstKeptIdx, 1, "budget=%d 必须跳过系统提示", budget)
		requireToolPairingIntact(t, messages, cut.FirstKeptIdx)
	}
}

func TestFindCutPointKeepsAssistantWithItsToolResults(t *testing.T) {
	counter := testCounter()
	// 一条 assistant 带两个工具调用，其后是两条工具结果。预算只够保留最后两条时，
	// 切点必须回退到 assistant 之前，而不是落在两条工具结果中间。
	messages := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{
			toolCall("c1", "knowledge_search", `{}`), toolCall("c2", "grep_chunks", `{}`),
		}},
		{Role: "tool", ToolCallID: "c1", Name: "knowledge_search", Content: "r1"},
		{Role: "tool", ToolCallID: "c2", Name: "grep_chunks", Content: "r2"},
	}
	cut := FindCutPoint(messages, 1, 1, counter)
	require.Equal(t, 2, cut.FirstKeptIdx,
		"预算极小时退化为最后一个合法切点：assistant 与其两条工具结果同进同出")
	requireToolPairingIntact(t, messages, cut.FirstKeptIdx)

	cut = FindCutPoint(messages, 1, CountMessages(counter, messages), counter)
	require.Equal(t, 1, cut.FirstKeptIdx)
	requireToolPairingIntact(t, messages, cut.FirstKeptIdx)
}

func TestPrepareReturnsNilWhenNothingOutsideKeepBudget(t *testing.T) {
	counter := testCounter()
	messages := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q"},
	}
	require.Nil(t, Prepare(messages, testSettings(), counter, nil))
}

// ---------------------------------------------------------------------------
// 规则 1 / 3：压缩产出与占位标记
// ---------------------------------------------------------------------------

func TestCompactReplacesHistoryWithMarkedSummary(t *testing.T) {
	counter := testCounter()
	messages := reactHistory(6, strings.Repeat("检索结果 ", 60))
	llm := &stubChat{response: "## Goal\n继续任务"}
	c := New(llm, counter, noSplitSettings())
	require.NotNil(t, c)

	result, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err)
	require.Equal(t, 1, llm.calls)
	require.False(t, result.SplitTurn)
	require.Positive(t, result.Freed())
	require.False(t, result.Degraded)

	// 结构：system + 摘要占位 + 原样保留的尾部。
	require.Equal(t, "system", result.Messages[0].Role)
	require.True(t, IsSummaryMessage(&result.Messages[1]), "摘要必须是可被下一次压缩识别的占位消息")
	require.Equal(t, "user", result.Messages[1].Role)
	require.Contains(t, result.Messages[1].Content, SummaryMarker)
	require.Contains(t, result.Messages[1].Content, "已被压缩")
	require.Contains(t, result.Messages[1].Content, "## Goal")
	requireToolPairingIntact(t, result.Messages, 2)

	// 压缩不得改写调用方持有的消息（规则 6：不触碰外部状态的同一原则）。
	require.Equal(t, reactHistory(6, strings.Repeat("检索结果 ", 60)), messages)
}

func TestCompactDoesNotSendToolsToSummarizer(t *testing.T) {
	counter := testCounter()
	messages := reactHistory(6, strings.Repeat("检索结果 ", 60))
	llm := &stubChat{response: "## Goal\nok"}
	c := New(llm, counter, testSettings())
	_, err := c.Compact(context.Background(), messages, ReasonFinalAnswer)
	require.NoError(t, err)
	require.NotEmpty(t, llm.tools)
	for _, n := range llm.tools {
		require.Zero(t, n, "摘要请求不得携带 tools，否则压缩过程可能重放工具调用")
	}
}

// ---------------------------------------------------------------------------
// 规则 4 / 异常：摘要重试上限与无模型降级
// ---------------------------------------------------------------------------

func TestSummarizerAttemptsAreCappedAndDegradeToRawArchive(t *testing.T) {
	counter := testCounter()
	messages := reactHistory(6, strings.Repeat("检索结果 ", 60))
	llm := &stubChat{err: errors.New("summarizer unavailable")}
	c := New(llm, counter, noSplitSettings())

	result, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err, "摘要失败不得让压缩整体失败")
	require.Equal(t, maxSummarizationAttempts, llm.calls, "单次摘要内部最多尝试 2 次")
	require.True(t, result.Degraded)
	require.Contains(t, result.Summary, "Raw conversation archive")
	require.Positive(t, result.Freed())
}

func TestSummarizerRejectsTruncatedSummary(t *testing.T) {
	counter := testCounter()
	messages := reactHistory(6, strings.Repeat("检索结果 ", 60))
	llm := &stubChat{response: "## Goal\n被截断的摘要", finish: "length"}
	c := New(llm, counter, noSplitSettings())

	result, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err)
	require.Equal(t, maxSummarizationAttempts, llm.calls)
	require.True(t, result.Degraded, "被 token 上限截断的摘要必须判为失败并降级")
}

// 切点落在轮次内部时，历史摘要与轮次前缀摘要各是一路独立的摘要调用，各自受 2 次上限约束。
func TestSplitTurnSummarizesHistoryAndTurnPrefixSeparately(t *testing.T) {
	counter := testCounter()
	messages := reactHistory(6, strings.Repeat("检索结果 ", 60))
	llm := &stubChat{response: "## Goal\nok"}
	c := New(llm, counter, testSettings())

	result, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err)
	require.True(t, result.SplitTurn)
	require.Equal(t, 2, llm.calls)
	require.Contains(t, result.Summary, splitTurnSeparator)
	require.Contains(t, strings.Join(llm.prompts, "\n"), "PREFIX of a turn")
}

// ---------------------------------------------------------------------------
// 规则 9：取消语义
// ---------------------------------------------------------------------------

func TestCompactDoesNotCallModelWhenCancelled(t *testing.T) {
	counter := testCounter()
	messages := reactHistory(6, strings.Repeat("检索结果 ", 60))
	llm := &stubChat{response: "## Goal\nok"}
	c := New(llm, counter, testSettings())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Compact(ctx, messages, ReasonOverflow)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, llm.calls, "取消后不得再发起任何模型调用")
}

// ---------------------------------------------------------------------------
// 规则 6：产物追踪跨压缩继承
// ---------------------------------------------------------------------------

func TestArtifactOpsSurviveCompactionAndAreInherited(t *testing.T) {
	counter := testCounter()
	payload := strings.Repeat("检索结果 ", 60)
	messages := []chat.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("call-%d", i)
		messages = append(messages,
			chat.Message{Role: "user", Content: fmt.Sprintf("q%d %s", i, payload)},
			chat.Message{
				Role: "assistant",
				ToolCalls: []chat.ToolCall{
					toolCall(id, "wiki_write_page", fmt.Sprintf(`{"slug":"page-%d","title":"t%d"}`, i, i)),
					toolCall(id+"r", "wiki_read_page", fmt.Sprintf(`{"slug":"read-%d"}`, i)),
				},
			},
			chat.Message{Role: "tool", ToolCallID: id, Name: "wiki_write_page", Content: "ok"},
			chat.Message{Role: "tool", ToolCallID: id + "r", Name: "wiki_read_page", Content: payload},
		)
	}
	messages = append(messages, chat.Message{Role: "user", Content: "latest"})

	llm := &stubChat{response: "## Goal\n已写出多个 Wiki 页面"}
	c := New(llm, counter, testSettings())
	first, err := c.Compact(context.Background(), messages, ReasonThreshold)
	require.NoError(t, err)
	require.Contains(t, first.Summary, "<produced-artifacts>")
	require.Contains(t, first.Summary, "page-0")
	require.Contains(t, first.Summary, "<referenced-artifacts>")
	require.Contains(t, first.Summary, "read-0")

	// 第二次压缩：上一份摘要不在被摘要的消息范围内，产物清单必须靠继承保留下来。
	grown := append(append([]chat.Message(nil), first.Messages[:2]...), reactHistory(4, payload)[1:]...)
	grown = append(grown, chat.Message{Role: "user", Content: "newest"})
	llm2 := &stubChat{response: "## Goal\n第二轮"}
	c2 := New(llm2, counter, testSettings())
	second, err := c2.Compact(context.Background(), grown, ReasonThreshold)
	require.NoError(t, err)
	require.Contains(t, second.Summary, "page-0", "产物清单必须跨压缩继承")
	require.Contains(t, second.Summary, "read-0")
	require.Equal(t, 1, strings.Count(second.Summary, "<"+producedTag+">"),
		"产物块只应被渲染一次，不得逐次压缩累积副本")
}

func TestArtifactOpsPrefersProducedOverReferenced(t *testing.T) {
	ops := artifactOps{}
	ops.read = appendUnique(ops.read, "page-a")
	ops.written = appendUnique(ops.written, "page-a")
	read, produced := ops.resolve()
	require.Equal(t, []string{"page-a"}, produced)
	require.Empty(t, read)
}

func TestAppendUniqueBoundsTrackedArtifacts(t *testing.T) {
	var list []string
	for i := 0; i < maxTrackedArtifacts+10; i++ {
		list = appendUnique(list, fmt.Sprintf("p%d", i))
	}
	require.Len(t, list, maxTrackedArtifacts)
	require.Equal(t, "p10", list[0], "超限时丢最旧的条目")
	require.Equal(t, fmt.Sprintf("p%d", maxTrackedArtifacts+9), list[len(list)-1])
}

// ---------------------------------------------------------------------------
// 规则 8：多模态溢出优先降图
// ---------------------------------------------------------------------------

func imageMessage(role, text string, urls ...string) chat.Message {
	parts := make([]chat.MessageContentPart, 0, len(urls)+1)
	for _, u := range urls {
		parts = append(parts, chat.MessageContentPart{
			Type: "image_url", ImageURL: &chat.ImageURL{URL: u},
		})
	}
	parts = append(parts, chat.MessageContentPart{Type: "text", Text: text})
	return chat.Message{Role: role, MultiContent: parts}
}

func TestDropOldestImagesKeepsCurrentTurnUpload(t *testing.T) {
	messages := []chat.Message{
		{Role: "system", Content: "sys"},
		imageMessage("user", "旧截图", "data:image/png;base64,OLD1"),
		{Role: "assistant", Content: "好的"},
		imageMessage("user", "更早的截图", "data:image/png;base64,OLD2"),
		{Role: "user", Content: "本轮提问", Images: []string{"data:image/png;base64,CURRENT"}},
	}
	require.Equal(t, 3, CountImages(messages))

	out, dropped := DropOldestImages(messages, 1)
	require.Equal(t, 2, dropped)
	require.Equal(t, 1, CountImages(out))
	require.Equal(t, []string{"data:image/png;base64,CURRENT"}, out[4].Images,
		"当前轮用户上传的图片是必要图片，不得移除")
	require.Zero(t, imageCount(&out[1]))
	require.Zero(t, imageCount(&out[3]))
	require.Contains(t, messageText(out[1]), "图片已移除", "移除处必须留下明确占位说明")
	require.Contains(t, messageText(out[3]), "图片已移除")

	// 原消息不得被改写。
	require.Equal(t, 3, CountImages(messages))
}

func TestDropOldestImagesNoopWhenWithinKeep(t *testing.T) {
	messages := []chat.Message{
		{Role: "user", Content: "q", Images: []string{"a"}},
	}
	out, dropped := DropOldestImages(messages, 1)
	require.Zero(t, dropped)
	require.Equal(t, messages, out)
}

// ---------------------------------------------------------------------------
// 规则 5 / 异常：无模型的规则式截断
// ---------------------------------------------------------------------------

func TestTruncateHistoryKeepsSystemRecentAndLeavesNotice(t *testing.T) {
	counter := testCounter()
	messages := reactHistory(6, strings.Repeat("检索结果 ", 60))
	out, dropped := TruncateHistory(messages, counter, 400)
	require.Positive(t, dropped)
	require.Equal(t, "system", out[0].Role)
	require.Equal(t, "user", out[1].Role)
	require.Contains(t, out[1].Content, TruncateMarker)
	require.Contains(t, out[1].Content, "早期步骤已被丢弃")
	requireToolPairingIntact(t, out, 2)
	require.Less(t, len(out), len(messages))
}

func TestTruncateHistoryNoopWhenNothingToDrop(t *testing.T) {
	counter := testCounter()
	messages := []chat.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "q"}}
	out, dropped := TruncateHistory(messages, counter, 100000)
	require.Zero(t, dropped)
	require.Equal(t, messages, out)
}

// ---------------------------------------------------------------------------
// 溢出识别
// ---------------------------------------------------------------------------

func TestIsOverflowError(t *testing.T) {
	overflow := []string{
		"prompt is too long: 120000 tokens > 100000 maximum",
		"request_too_large",
		"Input is too long for requested model, either decrease input length",
		"This model's maximum context length is 8192 tokens",
		"maximum context length is 4096 tokens",
		"Range of input length should be [1, 30000]",
		"context_length_exceeded",
		"too many tokens in the request",
		"model_context_window_exceeded",
	}
	for _, msg := range overflow {
		require.True(t, IsOverflowError(errors.New(msg)), "应识别为溢出: %s", msg)
	}

	notOverflow := []string{
		// 限流绝不能被误判为溢出，否则会拿压缩后的上下文去重试一个只需退避的请求。
		"ThrottlingException: Too many tokens, please wait before trying again",
		"429 Too Many Requests: rate limit exceeded",
		"rate limit reached, retry later",
		"503 Service Unavailable",
		"too many requests, slow down",
		"connection reset by peer",
		"",
	}
	for _, msg := range notOverflow {
		require.False(t, IsOverflowError(errors.New(msg)), "不得识别为溢出: %s", msg)
	}
	require.False(t, IsOverflowError(nil))
}

func TestResponseHitContextLimit(t *testing.T) {
	window := 4000
	cases := []struct {
		name   string
		resp   *types.ChatResponse
		budget int
		want   bool
	}{
		{"nil response", nil, 0, false},
		{
			"prompt 超过窗口",
			&types.ChatResponse{Usage: types.TokenUsage{PromptTokens: 5000, CompletionTokens: 10}}, 0, true,
		},
		{
			"length 停止且零输出、prompt 填满窗口",
			&types.ChatResponse{
				FinishReason: "length",
				Usage:        types.TokenUsage{PromptTokens: 3990, CompletionTokens: 0},
			}, 0, true,
		},
		{
			"length 停止且补全量小于请求上限",
			&types.ChatResponse{
				FinishReason: "max_tokens",
				Usage:        types.TokenUsage{PromptTokens: 100, CompletionTokens: 30},
			}, 1000, true,
		},
		{
			"未请求补全上限时第三条判据不生效",
			&types.ChatResponse{
				FinishReason: "length",
				Usage:        types.TokenUsage{PromptTokens: 100, CompletionTokens: 30},
			}, 0, false,
		},
		{
			"正常自然停止",
			&types.ChatResponse{
				FinishReason: "stop",
				Usage:        types.TokenUsage{PromptTokens: 100, CompletionTokens: 30},
			}, 1000, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ResponseHitContextLimit(tc.resp, window, tc.budget))
		})
	}
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func TestSettingsThresholdAndKeepRecentClamp(t *testing.T) {
	s := Settings{Enabled: true, MaxContextTokens: 200000}.Normalize()
	require.Equal(t, DefaultReserveTokens, s.ReserveTokens)
	require.Equal(t, DefaultKeepRecentTokens, s.KeepRecentTokens)
	require.Equal(t, 200000-DefaultReserveTokens, s.Threshold())
	require.True(t, s.ShouldCompact(200000-DefaultReserveTokens+1))
	require.False(t, s.ShouldCompact(200000-DefaultReserveTokens))

	// 小窗口下保留预算被压到可用量的四分之一，避免"压缩完立刻又触发"。
	small := Settings{Enabled: true, MaxContextTokens: 8000, ReserveTokens: 2000}.Normalize()
	require.Equal(t, 1500, small.KeepRecentTokens)
	require.Equal(t, 6000, small.Threshold())

	require.False(t, Settings{MaxContextTokens: 8000}.ShouldCompact(1<<30), "未启用时不压缩")
	require.Zero(t, Settings{Enabled: true}.Threshold(), "未配置窗口时阈值为 0")
}
