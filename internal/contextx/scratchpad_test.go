package contextx

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/stretchr/testify/require"
)

func stubSummarize(summary string) SummarizeFunc {
	return func(_ context.Context, _ string) (string, error) {
		return summary, nil
	}
}

// buildScratchpadMessages assembles system + user + n assistant/tool-call
// groups, mirroring the ReAct scratchpad layout.
func buildScratchpadMessages(n int) []chat.Message {
	msgs := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "goal"},
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			chat.Message{
				Role: "assistant",
				ToolCalls: []chat.ToolCall{{
					ID:   id,
					Type: "function",
					Function: chat.FunctionCall{
						Name:      fmt.Sprintf("tool_%d", i),
						Arguments: "{}",
					},
				}},
			},
			chat.Message{
				Role:       "tool",
				ToolCallID: id,
				Name:       fmt.Sprintf("tool_%d", i),
				Content:    fmt.Sprintf("result-%d", i),
			},
		)
	}
	return msgs
}

func TestCompressScratchpadFoldsOldResultsAndPreservesInvariant(t *testing.T) {
	msgs := buildScratchpadMessages(3)
	cfg := DefaultToolGovConfig()
	cfg.KeepRecentToolResults = 1

	var capturedPrompt string
	summarize := func(_ context.Context, prompt string) (string, error) {
		capturedPrompt = prompt
		return "checkpoint: tried tool_1 and tool_2", nil
	}

	out, done, err := CompressScratchpad(context.Background(), msgs, "goal", cfg, summarize, NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.True(t, done)

	// Folded contents reach the summarizer.
	require.Contains(t, capturedPrompt, "result-1")
	require.Contains(t, capturedPrompt, "result-2")
	require.NotContains(t, capturedPrompt, "result-3")

	// Exactly one tool message survives (the newest one).
	toolCount := 0
	checkpointCount := 0
	for _, m := range out {
		if m.Role == "tool" {
			toolCount++
			require.Equal(t, "call_3", m.ToolCallID)
		}
		if m.Role == "system" && strings.Contains(m.Content, "中期检查点摘要") {
			checkpointCount++
			require.Contains(t, m.Content, "checkpoint: tried tool_1 and tool_2")
		}
	}
	require.Equal(t, 1, toolCount)
	require.Equal(t, 1, checkpointCount)

	// Tool-calling invariant: every assistant tool_call references a surviving
	// tool message, and every tool message answers a surviving tool_call.
	survivingToolIDs := map[string]bool{}
	for _, m := range out {
		if m.Role == "tool" {
			survivingToolIDs[m.ToolCallID] = true
		}
	}
	for _, m := range out {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			require.True(t, survivingToolIDs[tc.ID],
				"assistant retains tool_call %s whose tool message was folded", tc.ID)
		}
	}

	// Hollow assistant shells (no content, all tool calls folded) are dropped.
	for _, m := range out {
		if m.Role == "assistant" {
			require.NotEmpty(t, m.ToolCalls, "hollow assistant message should have been dropped")
		}
	}
}

func TestCompressScratchpadKeepsContentBearingAssistant(t *testing.T) {
	msgs := buildScratchpadMessages(2)
	// Give the first assistant message real content — it must survive even
	// though its tool result gets folded.
	msgs[2].Content = "let me search first"
	cfg := DefaultToolGovConfig()
	cfg.KeepRecentToolResults = 1

	out, done, err := CompressScratchpad(context.Background(), msgs, "goal", cfg,
		stubSummarize("checkpoint"), NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.True(t, done)

	var firstAssistant *chat.Message
	for i := range out {
		if out[i].Role == "assistant" {
			firstAssistant = &out[i]
			break
		}
	}
	require.NotNil(t, firstAssistant)
	require.Equal(t, "let me search first", firstAssistant.Content)
	require.Empty(t, firstAssistant.ToolCalls, "folded tool_call must be stripped")
}

func TestCompressScratchpadBelowKeepThresholdNoop(t *testing.T) {
	msgs := buildScratchpadMessages(2)
	cfg := DefaultToolGovConfig()
	cfg.KeepRecentToolResults = 2

	out, done, err := CompressScratchpad(context.Background(), msgs, "goal", cfg,
		stubSummarize("checkpoint"), NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.False(t, done)
	require.Len(t, out, len(msgs))
}

func TestCompressScratchpadNilSummarizerNoop(t *testing.T) {
	msgs := buildScratchpadMessages(4)
	cfg := DefaultToolGovConfig()
	cfg.KeepRecentToolResults = 1

	out, done, err := CompressScratchpad(context.Background(), msgs, "goal", cfg, nil, nil)
	require.NoError(t, err)
	require.False(t, done)
	require.Len(t, out, len(msgs))
}

func TestNormalizeToolOutputMinifiesAndCaps(t *testing.T) {
	// Oversized array + overlong string field + pretty-printed JSON.
	items := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		items = append(items, fmt.Sprintf("%d", i))
	}
	long := strings.Repeat("x", 3000)
	input := "{\n  \"items\": [" + strings.Join(items, ",") + "],\n  \"note\": \"" + long + "\"\n}"

	out := NormalizeToolOutput(input, 50, 100)
	require.NotContains(t, out, "\n", "JSON must be minified")
	require.Contains(t, out, "more items omitted")
	require.Less(t, len(out), len(input))
}

func TestNormalizeToolOutputPassesThroughNonJSON(t *testing.T) {
	plain := "just some text\nwith lines"
	require.Equal(t, plain, NormalizeToolOutput(plain, 0, 0))
}

func TestInnerSummarizeToolResultBelowThresholdPassthrough(t *testing.T) {
	content := "short result"
	called := false
	summarize := func(_ context.Context, _ string) (string, error) {
		called = true
		return "summary", nil
	}
	cfg := DefaultToolGovConfig()
	out := InnerSummarizeToolResult(context.Background(), "web_fetch", "q", content, cfg, summarize, NewCounter(VendorGeneric))
	require.Equal(t, content, out)
	require.False(t, called)
}

func TestInnerSummarizeToolResultAboveThresholdSummarizes(t *testing.T) {
	content := strings.Repeat("数据 ", 5000) // well over 4k tokens
	cfg := DefaultToolGovConfig()
	out := InnerSummarizeToolResult(context.Background(), "web_fetch", "q", content, cfg,
		stubSummarize("dense summary"), NewCounter(VendorGeneric))
	require.Contains(t, out, "dense summary")
	require.Contains(t, out, "工具结果摘要")
	require.Less(t, NewCounter(VendorGeneric).Count(out), cfg.InnerSummaryThreshold)
}

func TestInnerSummarizeToolResultFallbackHardTruncates(t *testing.T) {
	content := strings.Repeat("data ", 5000)
	cfg := DefaultToolGovConfig()
	failSummarize := func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("llm down")
	}
	out := InnerSummarizeToolResult(context.Background(), "web_fetch", "q", content, cfg,
		failSummarize, NewCounter(VendorGeneric))
	require.Contains(t, out, "truncated")
	require.Less(t, NewCounter(VendorGeneric).Count(out), cfg.InnerSummaryThreshold+50)
}
