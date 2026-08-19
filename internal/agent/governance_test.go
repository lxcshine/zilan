package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// govFakeChat serves canned summaries for the governance LLM calls.
type govFakeChat struct {
	summary string
	err     error
	calls   int
}

func (f *govFakeChat) Chat(_ context.Context, _ []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &types.ChatResponse{Content: f.summary}, nil
}

func (f *govFakeChat) ChatStream(_ context.Context, _ []chat.Message, _ *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *govFakeChat) GetModelName() string { return "qwen3-32b" }
func (f *govFakeChat) GetModelID() string   { return "gov-fake" }

func withGovernance(cfg *types.ContextGovernanceConfig) testEngineOption {
	return func(c *types.AgentConfig) { c.ContextGovernance = cfg }
}

func boolPtr(b bool) *bool { return &b }

func TestGovernToolResultsInnerSummarizesOversized(t *testing.T) {
	fake := &govFakeChat{summary: "浓缩后的工具结果"}
	engine := newTestEngine(t, fake, withGovernance(&types.ContextGovernanceConfig{
		InnerSummaryThreshold: 50, // low threshold so the test stays small
		InnerSummaryBudget:    20,
	}))

	big := strings.Repeat("检索结果段落 ", 200) // ~ hundreds of tokens > 50
	messages := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q"},
		{Role: "tool", ToolCallID: "c1", Name: "web_fetch", Content: big},
	}

	out := engine.governToolResults(context.Background(), messages, 2, "q")
	require.Equal(t, 1, fake.calls, "oversized tool result must trigger one inner-summary call")
	require.Contains(t, out[2].Content, "浓缩后的工具结果")
	require.Contains(t, out[2].Content, "工具结果摘要")
	require.Equal(t, "c1", out[2].ToolCallID, "tool_call_id must survive governance")
}

func TestGovernToolResultsKeepsSmallResultAndMinifiesJSON(t *testing.T) {
	fake := &govFakeChat{summary: "unused"}
	engine := newTestEngine(t, fake) // nil governance config → defaults (4k threshold)

	pretty := "{\n  \"a\": 1,\n  \"b\": [1, 2, 3]\n}"
	messages := []chat.Message{
		{Role: "tool", ToolCallID: "c1", Name: "database_query", Content: pretty},
	}

	out := engine.governToolResults(context.Background(), messages, 0, "q")
	require.Equal(t, 0, fake.calls, "small result must not trigger inner summary")
	require.Equal(t, `{"a":1,"b":[1,2,3]}`, out[0].Content, "JSON output must be minified")
}

func TestGovernToolResultsDisabledPassthrough(t *testing.T) {
	fake := &govFakeChat{summary: "unused"}
	engine := newTestEngine(t, fake, withGovernance(&types.ContextGovernanceConfig{
		Enabled: boolPtr(false),
	}))

	pretty := "{\n  \"a\": 1\n}"
	messages := []chat.Message{{Role: "tool", ToolCallID: "c1", Name: "t", Content: pretty}}
	out := engine.governToolResults(context.Background(), messages, 0, "q")
	require.Equal(t, pretty, out[0].Content, "disabled governance must not touch content")
	require.Equal(t, 0, fake.calls)
}

func TestGovernToolResultsSkipsNonToolMessages(t *testing.T) {
	fake := &govFakeChat{summary: "unused"}
	engine := newTestEngine(t, fake, withGovernance(&types.ContextGovernanceConfig{
		InnerSummaryThreshold: 10,
	}))

	big := strings.Repeat("长文本 ", 200)
	messages := []chat.Message{
		{Role: "assistant", Content: big}, // not a tool message → untouched
	}
	out := engine.governToolResults(context.Background(), messages, 0, "q")
	require.Equal(t, big, out[0].Content)
	require.Equal(t, 0, fake.calls)
}

func TestMaybeCompressScratchpadTriggersOnCadence(t *testing.T) {
	fake := &govFakeChat{summary: "检查点：已尝试 tool_1"}
	engine := newTestEngine(t, fake, withGovernance(&types.ContextGovernanceConfig{
		ScratchpadCompressEvery: 2,
		KeepRecentToolResults:   1,
		InnerSummaryThreshold:   1 << 30, // disable inner summary for this test
	}))

	messages := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "goal"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "c1", Type: "function", Function: chat.FunctionCall{Name: "tool_1", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Name: "tool_1", Content: "r1"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "c2", Type: "function", Function: chat.FunctionCall{Name: "tool_2", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c2", Name: "tool_2", Content: "r2"},
	}

	// Below cadence → no-op.
	out := engine.maybeCompressScratchpad(context.Background(), messages, "goal", 1)
	require.Len(t, out, len(messages))
	require.Equal(t, 0, fake.calls)

	// Cadence reached → fold the older tool result into a checkpoint.
	out = engine.maybeCompressScratchpad(context.Background(), messages, "goal", 2)
	require.Equal(t, 1, fake.calls)
	require.Equal(t, 2, engine.lastScratchpadCompressAt)

	toolCount := 0
	checkpoint := false
	for _, m := range out {
		if m.Role == "tool" {
			toolCount++
			require.Equal(t, "c2", m.ToolCallID)
		}
		if m.Role == "system" && strings.Contains(m.Content, "中期检查点摘要") {
			checkpoint = true
		}
	}
	require.Equal(t, 1, toolCount)
	require.True(t, checkpoint)

	// Same cadence position again → no re-trigger.
	out2 := engine.maybeCompressScratchpad(context.Background(), out, "goal", 2)
	require.Equal(t, 1, fake.calls, "must not re-compress at the same step count")
	require.Len(t, out2, len(out))
}

func TestMaybeCompressScratchpadFailureKeepsMessages(t *testing.T) {
	fake := &govFakeChat{err: fmt.Errorf("llm down")}
	engine := newTestEngine(t, fake, withGovernance(&types.ContextGovernanceConfig{
		ScratchpadCompressEvery: 1,
		KeepRecentToolResults:   1,
	}))

	messages := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", ToolCallID: "c1", Name: "t1", Content: "r1"},
		{Role: "tool", ToolCallID: "c2", Name: "t2", Content: "r2"},
	}
	out := engine.maybeCompressScratchpad(context.Background(), messages, "goal", 5)
	require.Len(t, out, len(messages), "failed compression must return the original messages")
	require.Equal(t, 5, engine.lastScratchpadCompressAt, "cadence advances even on failure")
}
