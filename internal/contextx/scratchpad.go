package contextx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/chat"
)

// ---------------------------------------------------------------------------
// Agent tool-result governance
// ---------------------------------------------------------------------------

// ToolGovConfig tunes tool-result handling in the agent loop.
type ToolGovConfig struct {
	// InnerSummaryThreshold is the token count above which a tool result is
	// inner-summarized before entering the context (default 4000).
	InnerSummaryThreshold int
	// InnerSummaryBudget is the target token size of an inner summary
	// (default 800).
	InnerSummaryBudget int
	// ScratchpadCompressEvery triggers a mid-run scratchpad compression after
	// this many executed steps (default 6).
	ScratchpadCompressEvery int
	// KeepRecentToolResults keeps the newest K tool results verbatim during
	// scratchpad compression (default 2).
	KeepRecentToolResults int
}

// DefaultToolGovConfig returns the ima-grade defaults.
func DefaultToolGovConfig() ToolGovConfig {
	return ToolGovConfig{
		InnerSummaryThreshold:   4000,
		InnerSummaryBudget:      800,
		ScratchpadCompressEvery: 6,
		KeepRecentToolResults:   2,
	}
}

const innerSummaryPrompt = `The following is the raw output of the tool "%s", which is too large to keep in context.

User's current goal: %s

Extract a dense summary that preserves:
- Every fact, figure, URL, identifier, and quote relevant to the user's goal
- Any error messages or status codes verbatim
- Structured data (keep small JSON/tables intact)

Output ONLY the summary, at most %d words. Same language as the tool output.

Raw tool output:
%s`

// InnerSummarizeToolResult compresses an oversized tool result with a
// lightweight LLM call. When content is within budget, or summarization
// fails, the original (possibly hard-truncated) content is returned — the
// agent loop must never break because of governance.
func InnerSummarizeToolResult(
	ctx context.Context,
	toolName, query, content string,
	cfg ToolGovConfig,
	summarize SummarizeFunc,
	counter *Counter,
) string {
	if counter == nil {
		counter = NewCounter(VendorGeneric)
	}
	if cfg.InnerSummaryThreshold <= 0 {
		cfg.InnerSummaryThreshold = DefaultToolGovConfig().InnerSummaryThreshold
	}
	if cfg.InnerSummaryBudget <= 0 {
		cfg.InnerSummaryBudget = DefaultToolGovConfig().InnerSummaryBudget
	}
	if counter.Count(content) <= cfg.InnerSummaryThreshold {
		return content
	}
	if summarize != nil {
		// Feed at most ~3x the threshold into the summarizer to bound cost.
		feed := content
		if counter.Count(feed) > cfg.InnerSummaryThreshold*3 {
			feed = TruncateToTokens(feed, cfg.InnerSummaryThreshold*3, counter)
		}
		summary, err := summarize(ctx, fmt.Sprintf(innerSummaryPrompt, toolName, query, cfg.InnerSummaryBudget, feed))
		if err == nil {
			if s := strings.TrimSpace(summary); s != "" {
				return fmt.Sprintf("[工具结果摘要 - 原文 %d tokens]\n%s", counter.Count(content), s)
			}
		}
	}
	// Fallback: hard truncation with a marker.
	return TruncateToTokens(content, cfg.InnerSummaryThreshold, counter) +
		fmt.Sprintf("\n[... truncated, original %d tokens]", counter.Count(content))
}

// ---------------------------------------------------------------------------
// ReAct scratchpad cyclic compression
// ---------------------------------------------------------------------------

const scratchpadSummaryPrompt = `You are compressing the working memory (scratchpad) of a ReAct agent mid-run.

The agent's goal: %s

Below are older tool-call results that will be REMOVED from context. Write a checkpoint summary preserving:
- What has been tried and the outcome of each step
- Key findings, file paths, IDs, URLs, numbers discovered so far
- Open sub-tasks and the current plan state

Output ONLY the checkpoint, at most %d words. Same language as the content.

Older tool results:
%s`

// CompressScratchpad replaces old tool-result messages (everything before the
// newest keepRecent ones) with a single synthetic system message holding a
// checkpoint summary. Returns the new message list and whether compression
// happened.
//
// The OpenAI tool-calling invariant is preserved: an assistant message with
// tool_calls must be followed by one tool message per tool_call_id. Folding
// removes tool messages, so the parent assistant message has the folded IDs
// stripped from its ToolCalls; a hollow assistant shell (no content, no
// remaining tool calls, no reasoning) is dropped entirely.
func CompressScratchpad(
	ctx context.Context,
	messages []chat.Message,
	goal string,
	cfg ToolGovConfig,
	summarize SummarizeFunc,
	counter *Counter,
) ([]chat.Message, bool, error) {
	if counter == nil {
		counter = NewCounter(VendorGeneric)
	}
	if cfg.KeepRecentToolResults <= 0 {
		cfg.KeepRecentToolResults = DefaultToolGovConfig().KeepRecentToolResults
	}

	// Locate tool messages (role == "tool").
	toolIdx := []int{}
	for i, m := range messages {
		if m.Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) <= cfg.KeepRecentToolResults {
		return messages, false, nil
	}

	foldIdx := toolIdx[:len(toolIdx)-cfg.KeepRecentToolResults]
	var old strings.Builder
	foldSet := map[int]bool{}
	foldedCallIDs := map[string]bool{}
	for _, i := range foldIdx {
		foldSet[i] = true
		if messages[i].ToolCallID != "" {
			foldedCallIDs[messages[i].ToolCallID] = true
		}
		name := messages[i].Name
		if name == "" {
			name = "tool"
		}
		old.WriteString(fmt.Sprintf("### %s\n%s\n\n", name, messages[i].Content))
	}

	if summarize == nil {
		return messages, false, nil
	}
	summary, err := summarize(ctx, fmt.Sprintf(scratchpadSummaryPrompt, goal, cfg.InnerSummaryBudget, old.String()))
	if err != nil || strings.TrimSpace(summary) == "" {
		return messages, false, err
	}

	// Emitted as a system message (same convention as the agent memory
	// consolidator's mid-conversation summary) — a "tool" role message
	// without a tool_call_id would be rejected by OpenAI-compatible APIs.
	checkpoint := chat.Message{
		Role:    "system",
		Content: "[中期检查点摘要 - 早期工具结果已压缩]\n" + strings.TrimSpace(summary),
	}

	out := make([]chat.Message, 0, len(messages)-len(foldIdx)+1)
	inserted := false
	for i, m := range messages {
		if foldSet[i] {
			if !inserted {
				out = append(out, checkpoint)
				inserted = true
			}
			continue
		}
		// Strip tool_calls whose results were folded so no assistant message
		// references a tool_call_id that no longer has a tool message.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			kept := make([]chat.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				if !foldedCallIDs[tc.ID] {
					kept = append(kept, tc)
				}
			}
			if len(kept) != len(m.ToolCalls) {
				m.ToolCalls = kept
				if len(kept) == 0 &&
					strings.TrimSpace(m.Content) == "" &&
					strings.TrimSpace(m.ReasoningContent) == "" {
					continue
				}
			}
		}
		out = append(out, m)
	}
	return out, true, nil
}

// ---------------------------------------------------------------------------
// Structured tool output normalization
// ---------------------------------------------------------------------------

// NormalizeToolOutput compacts tool output to reduce token waste:
//   - JSON input is minified; oversized arrays are truncated with a marker;
//     overlong string fields are capped.
//   - Non-JSON input passes through unchanged (inner summary handles size).
func NormalizeToolOutput(content string, maxArrayItems, maxFieldRunes int) string {
	if maxArrayItems <= 0 {
		maxArrayItems = 50
	}
	if maxFieldRunes <= 0 {
		maxFieldRunes = 2000
	}
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return content
	}
	var v interface{}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return content
	}
	v = normalizeJSON(v, maxArrayItems, maxFieldRunes)
	out, err := json.Marshal(v)
	if err != nil {
		return content
	}
	return string(out)
}

func normalizeJSON(v interface{}, maxArrayItems, maxFieldRunes int) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			t[k] = normalizeJSON(val, maxArrayItems, maxFieldRunes)
		}
		return t
	case []interface{}:
		for i, val := range t {
			t[i] = normalizeJSON(val, maxArrayItems, maxFieldRunes)
		}
		if len(t) > maxArrayItems {
			omitted := len(t) - maxArrayItems
			t = append(t[:maxArrayItems], fmt.Sprintf("[... %d more items omitted]", omitted))
		}
		return t
	case string:
		runes := []rune(t)
		if len(runes) > maxFieldRunes {
			return string(runes[:maxFieldRunes]) + "…"
		}
		return t
	default:
		return v
	}
}
