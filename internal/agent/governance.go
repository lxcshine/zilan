package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// governanceTimeout bounds one inner-summary / scratchpad-checkpoint LLM call
// so a slow summarizer can never stall the agent loop.
const governanceTimeout = 45 * time.Second

// resolveToolGovConfig maps the agent's ContextGovernance config onto
// contextx.ToolGovConfig, filling ima-grade defaults for unset fields.
// The second return value reports whether governance is enabled.
func (e *AgentEngine) resolveToolGovConfig() (contextx.ToolGovConfig, bool) {
	cfg := contextx.DefaultToolGovConfig()
	c := e.config.ContextGovernance
	if !c.GovernanceEnabled() {
		return cfg, false
	}
	if c == nil {
		return cfg, true
	}
	if c.InnerSummaryThreshold > 0 {
		cfg.InnerSummaryThreshold = c.InnerSummaryThreshold
	}
	if c.InnerSummaryBudget > 0 {
		cfg.InnerSummaryBudget = c.InnerSummaryBudget
	}
	if c.ScratchpadCompressEvery > 0 {
		cfg.ScratchpadCompressEvery = c.ScratchpadCompressEvery
	}
	if c.KeepRecentToolResults > 0 {
		cfg.KeepRecentToolResults = c.KeepRecentToolResults
	}
	return cfg, true
}

// govCounter returns the vendor-calibrated token counter for the active chat
// model, built lazily so engine construction never fails on tokenizer init.
func (e *AgentEngine) govCounter() *contextx.Counter {
	if e.govTokenCounter == nil {
		modelName := ""
		if e.chatModel != nil {
			modelName = e.chatModel.GetModelName()
		}
		e.govTokenCounter = contextx.CounterForModel(modelName, "", "")
	}
	return e.govTokenCounter
}

// govSummarizeFunc builds the lightweight summarizer used for tool-result
// inner summaries and scratchpad checkpoints. It reuses the engine's chat
// model with a low temperature for factual compression.
func (e *AgentEngine) govSummarizeFunc() contextx.SummarizeFunc {
	return func(ctx context.Context, prompt string) (string, error) {
		callCtx, cancel := context.WithTimeout(ctx, governanceTimeout)
		callCtx = types.WithLLMCallMetadata(callCtx, "agent_context_governance", "")
		defer cancel()
		resp, err := e.chatModel.Chat(callCtx, []chat.Message{
			{Role: "user", Content: prompt},
		}, &chat.ChatOptions{
			Temperature: 0.2,
			MaxTokens:   2000,
		})
		if err != nil {
			return "", err
		}
		if resp == nil || resp.Content == "" {
			return "", fmt.Errorf("empty governance summary response")
		}
		return resp.Content, nil
	}
}

// governToolResults post-processes freshly appended tool messages
// (messages[fromIdx:]): JSON outputs are normalized (minified, oversized
// arrays truncated, overlong string fields capped) and results exceeding the
// token threshold are inner-summarized by a lightweight LLM call before they
// enter the context window. Governance never fails the loop — on any error
// the original (possibly hard-truncated) content is kept.
func (e *AgentEngine) governToolResults(
	ctx context.Context,
	messages []chat.Message,
	fromIdx int,
	query string,
) []chat.Message {
	cfg, enabled := e.resolveToolGovConfig()
	if !enabled {
		return messages
	}
	counter := e.govCounter()
	var summarize contextx.SummarizeFunc // built lazily on the first oversized result
	for i := fromIdx; i < len(messages); i++ {
		if messages[i].Role != "tool" || messages[i].Content == "" {
			continue
		}
		content := contextx.NormalizeToolOutput(messages[i].Content, 0, 0)
		if counter.Count(content) > cfg.InnerSummaryThreshold {
			if summarize == nil {
				summarize = e.govSummarizeFunc()
			}
			before := counter.Count(content)
			content = contextx.InnerSummarizeToolResult(
				ctx, messages[i].Name, query, content, cfg, summarize, counter)
			logger.Infof(ctx,
				"[Agent][Governance] Tool %q result inner-summarized: %d → %d tokens",
				messages[i].Name, before, counter.Count(content))
			common.PipelineInfo(ctx, "Agent", "tool_result_inner_summary", map[string]interface{}{
				"tool":          messages[i].Name,
				"tokens_before": before,
				"tokens_after":  counter.Count(content),
			})
		}
		messages[i].Content = content
	}
	return messages
}

// maybeCompressScratchpad checkpoint-compresses the ReAct scratchpad once
// every cfg.ScratchpadCompressEvery executed tool steps, folding older tool
// results into a single mid-run summary so later rounds keep their token
// budget for reasoning. Compression is cadence-based (not token-based) and
// complements manageContextWindow's threshold-based consolidation.
func (e *AgentEngine) maybeCompressScratchpad(
	ctx context.Context,
	messages []chat.Message,
	query string,
	executedSteps int,
) []chat.Message {
	cfg, enabled := e.resolveToolGovConfig()
	if !enabled || cfg.ScratchpadCompressEvery <= 0 {
		return messages
	}
	if executedSteps-e.lastScratchpadCompressAt < cfg.ScratchpadCompressEvery {
		return messages
	}
	// Advance the cadence marker before attempting compression so a failing
	// summarizer is not retried on every subsequent round.
	e.lastScratchpadCompressAt = executedSteps

	compressed, done, err := contextx.CompressScratchpad(
		ctx, messages, query, cfg, e.govSummarizeFunc(), e.govCounter())
	if err != nil {
		logger.Warnf(ctx, "[Agent][Governance] Scratchpad compression failed: %v", err)
		return messages
	}
	if done {
		logger.Infof(ctx,
			"[Agent][Governance] Scratchpad compressed at %d executed steps: %d → %d messages",
			executedSteps, len(messages), len(compressed))
		common.PipelineInfo(ctx, "Agent", "scratchpad_compressed", map[string]interface{}{
			"executed_steps":  executedSteps,
			"messages_before": len(messages),
			"messages_after":  len(compressed),
		})
	}
	return compressed
}
