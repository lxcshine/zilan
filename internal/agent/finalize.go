package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

func finalAnswerImageRequirement(hasRetrievedImage bool) string {
	if !hasRetrievedImage {
		return ""
	}
	return `
5. Retrieved tool results contain Markdown images. Unless the user explicitly requested text-only output or every image is clearly unrelated, the final answer MUST include at least one relevant Markdown image copied verbatim from the tool results. Preserve its complete URL exactly. Use ASCII half-width parentheses exactly as ![alt](url) and never use full-width （ or ）. Place the image immediately after the paragraph it supports. When multiple images support different sections, distribute them across those sections instead of stopping after the first image.
6. Before finishing, silently verify that the answer contains a Markdown image when requirement 5 applies.`
}

// streamFinalAnswerToEventBus streams the final answer generation through EventBus
func (e *AgentEngine) streamFinalAnswerToEventBus(
	ctx context.Context,
	query string,
	state *types.AgentState,
	sessionID string,
) error {
	totalToolCalls := countTotalToolCalls(state.RoundSteps)
	logger.Infof(ctx, "[Agent][FinalAnswer] Synthesizing from %d steps, %d tool calls",
		len(state.RoundSteps), totalToolCalls)
	common.PipelineInfo(ctx, "Agent", "final_answer_start", map[string]interface{}{
		"session_id":   sessionID,
		"query":        query,
		"steps":        len(state.RoundSteps),
		"tool_results": totalToolCalls,
	})

	// Build messages with all context
	systemPrompt := e.buildSystemPrompt(ctx)
	userTurn := e.RenderUserTurnContent(sessionID, query)

	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userTurn},
	}

	// Add all tool call results as context
	toolResultCount := 0
	hasRetrievedImage := false
	for stepIdx, step := range state.RoundSteps {
		for toolIdx, toolCall := range step.ToolCalls {
			toolResultCount++
			if searchutil.MarkdownImageRegex.MatchString(toolCall.Result.Output) {
				hasRetrievedImage = true
			}
			modelOutput := e.modelContext.ModelToolResultForTool(toolCall.Name, toolCall.Result)
			messages = append(messages, chat.Message{
				Role:    "user",
				Content: fmt.Sprintf("Tool %s returned: %s", toolCall.Name, modelOutput),
			})
			logger.Debugf(ctx, "[Agent][FinalAnswer] Added tool result [Step-%d][Tool-%d]: %s (output: %d chars)",
				stepIdx+1, toolIdx+1, toolCall.Name, len(toolCall.Result.Output))
		}
	}

	logger.Debugf(ctx, "[Agent][FinalAnswer] Built context: %d messages, %d tool results",
		len(messages), toolResultCount)

	imageRequirement := finalAnswerImageRequirement(hasRetrievedImage)

	// Add final answer prompt
	finalPrompt := fmt.Sprintf(`Based on the above tool call results, generate a complete answer for the user's question.

User question: %s

Requirements:
1. Answer based on the actually retrieved content
2. Organize the answer in a structured format
3. If information is insufficient, honestly state so
4. IMPORTANT: Respond in the same language as the user's question
%s

Now generate the final answer:`, query, imageRequirement)

	messages = append(messages, chat.Message{
		Role:    "user",
		Content: finalPrompt,
	})

	// PRD FR-06 规则 7：达到最大迭代次数（或 LLM 失败降级）时的最终答案合成，同样先压缩
	// 再发送。
	messages = e.compactFinalAnswerContext(ctx, messages, len(state.RoundSteps))

	// Generate a single ID for this entire final answer stream
	answerID := generateEventID("answer")
	logger.Debugf(ctx, "[Agent][FinalAnswer] AnswerID: %s", answerID)
	answerDoneEmitted := false

	llmResult, err := e.streamLLMToEventBus(
		ctx,
		messages,
		&chat.ChatOptions{Temperature: e.config.Temperature}, // Thinking disabled for final answer synthesis
		func(chunk *types.StreamResponse, fullContent string) {
			// Defensive filter: only emit answer content, skip thinking chunks
			if chunk.ResponseType == types.ResponseTypeThinking {
				return
			}
			if chunk.Content != "" {
				logger.Debugf(ctx, "[Agent][FinalAnswer] Emitting answer chunk: %d chars", len(chunk.Content))
				e.eventBus.Emit(ctx, event.Event{
					ID:        answerID,
					Type:      event.EventAgentFinalAnswer,
					SessionID: sessionID,
					Data: event.AgentFinalAnswerData{
						Content: chunk.Content,
						Done:    chunk.Done,
					},
				})
				if chunk.Done {
					answerDoneEmitted = true
				}
			}
		},
	)
	if err != nil {
		logger.Errorf(ctx, "[Agent][FinalAnswer] Final answer generation failed: %v", err)
		common.PipelineError(ctx, "Agent", "final_answer_stream_failed", map[string]interface{}{
			"session_id": sessionID,
			"error":      err.Error(),
		})
		return err
	}
	// 最终答案合成本身也是一次模型往返，同样计入本轮总量（PRD FR-12）。
	e.accumulateTurnUsage(llmResult.Usage)

	if !answerDoneEmitted {
		e.eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: "",
				Done:    true,
			},
		})
	}

	// Safety net: strip any residual <think> blocks that may have leaked through
	fullAnswer := agenttools.StripThinkBlocks(llmResult.Content)
	logger.Infof(ctx, "[Agent][FinalAnswer] Final answer generated: %d characters", len(fullAnswer))
	common.PipelineInfo(ctx, "Agent", "final_answer_done", map[string]interface{}{
		"session_id": sessionID,
		"answer_len": len(fullAnswer),
	})
	state.FinalAnswer = fullAnswer
	return nil
}

// compactFinalAnswerContext 在最终答案合成前压缩上下文（PRD FR-06 规则 7）。
//
// 合成用的消息把每条工具结果渲染为 user 消息，因此不存在 tool_call / 工具结果的配对问题；
// 压缩本身是一次不携带任何 tools 的摘要调用，因此**不会重放工具调用**，也不会触碰任何
// 外部状态（规则 6）。最后一条是合成指令，切点永远保留它。
//
// 规则 9：ctx 已取消时直接返回原消息，不发起任何模型调用。
func (e *AgentEngine) compactFinalAnswerContext(
	ctx context.Context, messages []chat.Message, round int,
) []chat.Message {
	if ctx.Err() != nil {
		return messages
	}
	settings := e.compactor.Settings()
	if !settings.ShouldCompact(e.countContextTokens(messages)) {
		return messages
	}
	// 合成用的消息列表是临时拼装的（每条工具结果渲染为 user 消息），下标与装载历史时的
	// 轮次区间毫无关系：这次压缩不得产出检查点，否则摘要会被写到错误的 assistant 行上
	// （PRD FR-10）。
	e.invalidateHistoryView()
	if compacted, ok := e.runCompaction(ctx, messages, round, compaction.ReasonFinalAnswer); ok {
		return compacted
	}
	degraded, _ := e.degradeContext(ctx, messages, round)
	return degraded
}

// handleMaxIterations generates a final answer when the agent loop exhausted all iterations
// without the LLM producing a natural stop. It marks state.IsComplete = true.
func (e *AgentEngine) handleMaxIterations(
	ctx context.Context, query string, state *types.AgentState, sessionID string,
) {
	logger.Info(ctx, "Reached max iterations, generating final answer")
	common.PipelineWarn(ctx, "Agent", "max_iterations_reached", map[string]interface{}{
		"iterations": state.CurrentRound,
		"max":        e.config.MaxIterations,
	})

	// Stream final answer generation through EventBus
	if err := e.streamFinalAnswerToEventBus(ctx, query, state, sessionID); err != nil {
		logger.Errorf(ctx, "Failed to synthesize final answer: %v", err)
		common.PipelineError(ctx, "Agent", "final_answer_failed", map[string]interface{}{
			"error": err.Error(),
		})
		state.FinalAnswer = "Sorry, I was unable to generate a complete answer."
	}
	state.IsComplete = true
}

// emitCompletionEvent emits the EventAgentComplete event with execution summary.
func (e *AgentEngine) emitCompletionEvent(
	ctx context.Context, state *types.AgentState, sessionID, messageID string, startTime time.Time,
) {
	// Convert knowledge refs to interface{} slice for event data
	knowledgeRefsInterface := make([]interface{}, 0, len(state.KnowledgeRefs))
	for _, ref := range state.KnowledgeRefs {
		knowledgeRefsInterface = append(knowledgeRefsInterface, ref)
	}

	e.eventBus.Emit(ctx, event.Event{
		ID:        generateEventID("complete"),
		Type:      event.EventAgentComplete,
		SessionID: sessionID,
		Data: event.AgentCompleteData{
			FinalAnswer:     state.FinalAnswer,
			KnowledgeRefs:   knowledgeRefsInterface,
			AgentSteps:      state.RoundSteps, // Include detailed execution steps for message storage
			TotalSteps:      len(state.RoundSteps),
			TotalDurationMs: time.Since(startTime).Milliseconds(),
			MessageID:       messageID, // Include message ID for proper message update
			// 本轮的用量与上下文归因（PRD FR-12）。挂在 Extra 上而不是新增事件：
			// EventAgentComplete 每条退出路径恰好发一次（见 executeLoop 的
			// completionEmitted 守卫），于是归因也恰好落库一次。进程内传递，
			// 值就是 *types.TokenUsage / *types.ContextTurnStats 本身。
			Extra: e.turnAccountingExtra(),
		},
	})

	logger.Infof(ctx, "Agent execution completed in %d rounds", state.CurrentRound)
}
