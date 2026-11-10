package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/modelcontext"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	minToolResultTokens     = 8 * 1024
	maxToolResultTokens     = 32 * 1024
	toolResultTokenFraction = 5 // 20%

	// minFreedFraction 是一次压缩必须回收的上下文占比的倒数：释放量低于 TokensBefore/20
	// 即视为压缩无效，说明摘要调用买到的空间还不如它自己花掉的多（PRD FR-06 规则 5）。
	minFreedFraction = 20 // 5%

	// keepRecentImages 是多模态溢出降级时保留的最新图片数，即当前轮用户刚上传的那张
	// （PRD FR-06 规则 8）。
	keepRecentImages = 1
)

// manageContextWindow 在上下文越过阈值时把较早的对话摘要掉。返回的 bool 报告消息是否
// 发生变化，调用方据此知道自己的 token 估算已经过期。
//
// currentTokens 是调用方对当前上下文规模的最佳估计（有 API Usage 时用 Usage，否则用
// internal/contextx 的精确 tokenizer 计数）。
func (e *AgentEngine) manageContextWindow(
	ctx context.Context, messages []chat.Message, round, currentTokens int,
) ([]chat.Message, bool) {
	settings := e.compactor.Settings()
	if !settings.ShouldCompact(currentTokens) {
		return messages, false
	}

	logger.Infof(ctx, "[Agent][Round-%d] 上下文 %d tokens 越过阈值 %d "+
		"(window=%d, reserved=%d, keep_recent=%d)，开始压缩",
		round, currentTokens, settings.Threshold(), settings.MaxContextTokens,
		settings.ReserveTokens, settings.KeepRecentTokens)

	changed := false
	// 规则 8：图片导致的溢出优先降级/移除最早的非必要图片，而不是压缩文本历史。
	if degraded, dropped := e.dropOldestImages(ctx, messages, round); dropped > 0 {
		messages, changed = degraded, true
		currentTokens = e.countContextTokens(messages)
		if !settings.ShouldCompact(currentTokens) {
			return messages, true
		}
	}

	if compacted, ok := e.runCompaction(ctx, messages, round, compaction.ReasonThreshold); ok {
		messages, changed = compacted, true
		if !settings.ShouldCompact(e.countContextTokens(messages)) {
			return messages, true
		}
	}

	// 压缩后仍超阈，说明重量落在保留窗口内部，切点够不到——某条工具结果自己就足够大。
	degraded, ok := e.degradeContext(ctx, messages, round)
	return degraded, changed || ok
}

// countContextTokens 用精确 tokenizer 计量整段上下文（PRD §4.9：Zilan 使用多厂商精确
// tokenizer，而非上游的 BPE 估算）。
func (e *AgentEngine) countContextTokens(messages []chat.Message) int {
	return compaction.CountMessages(e.govCounter(), messages)
}

// runCompaction 执行一次压缩，并报告上下文是否真的变小了。返回 false 意味着本轮再试也
// 没有帮助，调用方不得继续重试：一次释放量为零的压缩仍然要付一整次摘要往返。
func (e *AgentEngine) runCompaction(
	ctx context.Context, messages []chat.Message, round int, reason compaction.Reason,
) ([]chat.Message, bool) {
	if e.compactor == nil {
		return messages, false
	}
	// 规则 9：压缩过程中用户取消，则不再发起模型调用。
	if ctx.Err() != nil {
		return messages, false
	}
	// 耗尽标记绑定在消息数而不是裸 bool 上：一旦循环追加了新的轮次，就有新历史可以摘要，
	// 先前那个"无可压缩"不再描述当前上下文。
	if e.compactionExhaustedAt > 0 && len(messages) <= e.compactionExhaustedAt {
		return messages, false
	}

	result, err := e.compactor.Compact(ctx, messages, reason)
	if err != nil {
		if errors.Is(err, compaction.ErrNothingToCompact) {
			logger.Infof(ctx, "[Agent][Round-%d] 保留预算之外已无内容，跳过压缩", round)
		} else {
			logger.Warnf(ctx, "[Agent][Round-%d] 压缩失败: %v", round, err)
		}
		if ctx.Err() == nil {
			e.compactionExhaustedAt = len(messages)
		}
		return messages, false
	}
	// 释放量判据之前先落检查点（PRD FR-10 规则 1）。这次压缩即使对本轮无益——释放的
	// 空间还不够摘要自己的开销——被压缩掉的已落库历史也确实变成了摘要；丢掉它只会让
	// 下一轮装载同样的历史、重付同样的摘要调用。典型场景是一个很大的活跃轮次紧邻一段
	// 很短的已存历史。
	e.saveContextCheckpoint(ctx, result.Checkpoint, round)

	// 归因（PRD FR-12）：检查点落库之后再记账，绝不改变上面的保存顺序。
	// checkpointSaved 复用 saveContextCheckpoint 的准入条件（该函数不返回结果，
	// 而 checkpoint.go 属于 FR-10，不在本次改动范围内）。
	checkpointSaved := result.Checkpoint != nil && e.checkpointSink != nil &&
		result.Checkpoint.MessageID != ""
	recordCompaction := func(adopted bool) {
		ev := types.ContextTurnEvent{
			Kind:            types.ContextEventCompaction,
			Reason:          string(result.Reason),
			TokensBefore:    result.TokensBefore,
			TokensAfter:     result.TokensAfter,
			Degraded:        result.Degraded,
			CheckpointSaved: checkpointSaved,
		}
		if !adopted {
			// 摘要调用与检查点都真实发生了，但本轮不采用压缩结果，上下文并没有
			// 变小：释放量记 0，否则看板会把没兑现的收益算进压缩回报率。
			ev.TokensAfter = result.TokensBefore
			ev.Detail = "released_too_little_not_adopted"
		}
		e.recordContextEvent(round, ev)
	}

	// "释放了一点"是太弱的判据。按那个规则，26700 tokens 里回收 240 也算进展，于是循环
	// 每轮都付一次摘要费用，而上下文原地不动。
	if result.Freed() < result.TokensBefore/minFreedFraction {
		logger.Warnf(ctx, "[Agent][Round-%d] 压缩释放量过小 (%d → %d tokens)，本规模下不再尝试",
			round, result.TokensBefore, result.TokensAfter)
		e.compactionExhaustedAt = len(messages)
		recordCompaction(false)
		return messages, false
	}

	logger.Infof(ctx, "[Agent][Round-%d] 已压缩 (%s): %d → %d tokens, %d → %d messages "+
		"(split_turn=%v, degraded=%v, omitted=%d)",
		round, result.Reason, result.TokensBefore, result.TokensAfter,
		result.MessagesBefore, result.MessagesAfter, result.SplitTurn, result.Degraded, result.Omitted)
	common.PipelineInfo(ctx, "Agent", "context_compacted", map[string]interface{}{
		"round":           round,
		"reason":          string(result.Reason),
		"tokens_before":   result.TokensBefore,
		"tokens_after":    result.TokensAfter,
		"messages_before": result.MessagesBefore,
		"messages_after":  result.MessagesAfter,
		"degraded":        result.Degraded,
		"split_turn":      result.SplitTurn,
	})
	e.notifyCompaction(ctx, round, result)
	recordCompaction(true)

	// 用量基线描述的是压缩前的上下文；留着它会让下一轮拿已经不存在的历史去估算，从而
	// 立刻再压一次。
	e.lastUsage = types.TokenUsage{}
	e.lastSentMsgCount = 0
	// 耗尽标记描述的也是压缩前的历史。一次成功的压缩把那段历史重建到了上次失败停下的
	// 计数之下，守卫的前提——"该计数以下答案不会变"——不再成立。
	e.compactionExhaustedAt = 0
	// 消息列表已被重建（系统提示 + 摘要 + 保留尾部），轮次区间下标随之失效。
	e.invalidateHistoryView()

	return result.Messages, true
}

// forceCompaction 无视阈值执行压缩，用于供应商已经告诉我们窗口满了、而让我们走到这一步
// 的那个估算恰恰是最不可信的东西的场合。
func (e *AgentEngine) forceCompaction(
	ctx context.Context, messages []chat.Message, round int,
) []chat.Message {
	// 规则 8 同样适用于溢出恢复：先试图片降级，它不需要模型调用。
	if degraded, dropped := e.dropOldestImages(ctx, messages, round); dropped > 0 {
		messages = degraded
		if !e.compactor.Settings().ShouldCompact(e.countContextTokens(messages)) {
			return messages
		}
	}
	if compacted, ok := e.runCompaction(ctx, messages, round, compaction.ReasonOverflow); ok {
		return compacted
	}
	degraded, _ := e.degradeContext(ctx, messages, round)
	return degraded
}

// dropOldestImages 移除最早的非必要图片（PRD FR-06 规则 8）。当前轮用户上传的图片保留。
func (e *AgentEngine) dropOldestImages(
	ctx context.Context, messages []chat.Message, round int,
) ([]chat.Message, int) {
	if compaction.CountImages(messages) <= keepRecentImages {
		return messages, 0
	}
	degraded, dropped := compaction.DropOldestImages(messages, keepRecentImages)
	if dropped == 0 {
		return messages, 0
	}
	logger.Infof(ctx, "[Agent][Round-%d] 多模态溢出降级：移除 %d 张早期图片", round, dropped)
	common.PipelineInfo(ctx, "Agent", "context_images_dropped", map[string]interface{}{
		"round":          round,
		"images_dropped": dropped,
	})
	// 归因（PRD FR-12）：DropOldestImages 返回副本，原切片未被改写，因此这里可以
	// 事后计量前后规模。数字一律出自精确 tokenizer（compaction.CountMessages）。
	e.recordContextEvent(round, types.ContextTurnEvent{
		Kind:         types.ContextEventImageDrop,
		Reason:       "multimodal_overflow",
		TokensBefore: e.countContextTokens(messages),
		TokensAfter:  e.countContextTokens(degraded),
		Detail:       fmt.Sprintf("dropped %d earliest image(s), kept %d", dropped, keepRecentImages),
	})
	// 规则 3 + §5.3-3：图片被移除必须让用户看见，不得静默降级。
	e.notifyContext(ctx, round, fmt.Sprintf(
		"[上下文压缩] 为适配模型上下文窗口，已移除 %d 张较早的图片（本轮上传的图片保留），"+
			"可能影响对早期图片内容的引用。", dropped))
	return degraded, dropped
}

// degradeContext 是压缩无效时的降级路径（PRD FR-06 规则 5）：先把工具结果预览化
// （保持 tool_call 与结果配对不被破坏），仍不够则执行无模型的规则式截断，并明确告知
// 用户"早期步骤已被丢弃，可能影响后续推理"。
func (e *AgentEngine) degradeContext(
	ctx context.Context, messages []chat.Message, round int,
) ([]chat.Message, bool) {
	settings := e.compactor.Settings()
	trimmed, ok := e.trimToolResults(ctx, messages, round, settings)
	counter := e.govCounter()
	trimmedTokens := compaction.CountMessages(counter, trimmed)
	if !settings.ShouldCompact(trimmedTokens) {
		return trimmed, ok
	}
	truncated, dropped := compaction.TruncateHistory(trimmed, counter, settings.KeepRecentTokens)
	if dropped == 0 {
		return trimmed, ok
	}
	logger.Warnf(ctx, "[Agent][Round-%d] 压缩仍无法解决溢出，规则式截断丢弃 %d 条早期消息", round, dropped)
	// 截断丢弃了消息，轮次区间下标随之失效（PRD FR-10）。
	e.invalidateHistoryView()
	common.PipelineWarn(ctx, "Agent", "context_truncated", map[string]interface{}{
		"round":            round,
		"messages_dropped": dropped,
	})
	// 归因（PRD FR-12）：硬截断丢弃的内容没有摘要顶替，因此与压缩分开统计，并标记
	// Degraded——走到这里意味着摘要式压缩已经救不回来了。
	e.recordContextEvent(round, types.ContextTurnEvent{
		Kind:         types.ContextEventTruncation,
		Reason:       "history_truncated",
		TokensBefore: trimmedTokens,
		TokensAfter:  compaction.CountMessages(counter, truncated),
		Degraded:     true,
		Detail:       fmt.Sprintf("%s dropped %d earliest message(s)", compaction.TruncateMarker, dropped),
	})
	e.notifyContext(ctx, round, fmt.Sprintf(
		"%s 早期步骤已被丢弃（%d 条消息），可能影响后续推理。如需其中的细节，请让我重新检索。",
		compaction.TruncateMarker, dropped))
	return truncated, true
}

// notifyCompaction 把一次成功的压缩作为用户可见提示推送到会话
// （PRD FR-06「结果与状态变更」：会话中出现"已压缩早期步骤"的可见提示）。
func (e *AgentEngine) notifyCompaction(
	ctx context.Context, round int, result *compaction.Result,
) {
	text := fmt.Sprintf("[上下文压缩] 已压缩早期步骤：%d → %d tokens（%d → %d 条消息）。"+
		"被压缩的工具结果以摘要形式保留。",
		result.TokensBefore, result.TokensAfter, result.MessagesBefore, result.MessagesAfter)
	if result.Degraded {
		text += " 摘要模型调用失败，已降级为规则式归档，细节可能不完整。"
	}
	e.notifyContext(ctx, round, text)
}

// notifyContext 通过思考流把一条上下文治理提示推送给用户。走 EventAgentThought 是因为
// 它是既有的、前端已渲染的会话内可见通道，无需改动 handler / router。
func (e *AgentEngine) notifyContext(ctx context.Context, iteration int, text string) {
	if e == nil || e.eventBus == nil || text == "" {
		return
	}
	noticeID := generateEventID("context-compaction")
	_ = e.eventBus.Emit(ctx, event.Event{
		ID:        noticeID,
		Type:      event.EventAgentThought,
		SessionID: e.sessionID,
		Data:      event.AgentThoughtData{Content: text, Iteration: iteration},
	})
	_ = e.eventBus.Emit(ctx, event.Event{
		ID:        noticeID,
		Type:      event.EventAgentThought,
		SessionID: e.sessionID,
		Data:      event.AgentThoughtData{Content: "", Iteration: iteration, Done: true},
	})
}

// responseHitContextLimit 报告响应是否是被塞满的上下文窗口塑形的，而不是被我们请求的
// 补全预算截断的。
func (e *AgentEngine) responseHitContextLimit(response *types.ChatResponse) bool {
	window := 0
	if e.config != nil {
		window = e.config.MaxContextTokens
	}
	// Zilan 的 Agent 轮次不显式设置 max_tokens（由供应商默认），因此第三条判据
	// （补全量小于请求上限）不参与判定，传 0。
	return compaction.ResponseHitContextLimit(response, window, 0)
}

// trimToolResults 把工具输出替换为预览，直到其总量落进窗口的一定比例。这是最后手段：
// 与压缩不同，它移除的内容没有摘要顶替。
func (e *AgentEngine) trimToolResults(
	ctx context.Context, messages []chat.Message, round int, settings compaction.Settings,
) ([]chat.Message, bool) {
	counter := e.govCounter()
	trimmed, ok := trimToolResultsToBudget(
		messages, counter, toolResultBudget(settings.MaxContextTokens),
	)
	if !ok {
		return messages, false
	}
	logger.Infof(ctx, "[Agent][Round-%d] 已将工具结果预览化以落进 token 预算", round)
	// 归因（PRD FR-12）：预览化移除的内容没有摘要顶替，因此记为 tool_trim 而不是
	// compaction。trimToolResultsToBudget 返回副本，原切片可以事后计量。
	e.recordContextEvent(round, types.ContextTurnEvent{
		Kind:         types.ContextEventToolTrim,
		Reason:       "tool_result_preview",
		TokensBefore: compaction.CountMessages(counter, messages),
		TokensAfter:  compaction.CountMessages(counter, trimmed),
	})
	return trimmed, true
}

func toolResultBudget(maxContextTokens int) int {
	if maxContextTokens <= 0 {
		return maxToolResultTokens
	}
	budget := maxContextTokens / toolResultTokenFraction
	if budget < minToolResultTokens {
		return minToolResultTokens
	}
	if budget > maxToolResultTokens {
		return maxToolResultTokens
	}
	return budget
}

// trimToolResultsToBudget 返回供下一次模型调用使用的消息副本，绝不原地修改被 SSE、诊断
// 或持久化引用的消息。assistant 的工具调用消息保持不动，于是每个被预览化的工具结果都
// 保留供应商要求的「调用/结果」配对（PRD FR-06 规则 2）。
func trimToolResultsToBudget(
	messages []chat.Message,
	counter *contextx.Counter,
	budget int,
) ([]chat.Message, bool) {
	if budget <= 0 || len(messages) == 0 {
		return messages, false
	}

	var toolIndexes []int
	total := 0
	for i := range messages {
		if messages[i].Role == "tool" {
			toolIndexes = append(toolIndexes, i)
			total += compaction.CountMessage(counter, &messages[i])
		}
	}
	if total <= budget || len(toolIndexes) == 0 {
		return messages, false
	}

	out := append([]chat.Message(nil), messages...)
	baseCosts := make(map[int]int, len(toolIndexes))
	remaining := budget
	for _, idx := range toolIndexes {
		out[idx].Content = compactedToolResultMarker(messages[idx].Content)
		cost := compaction.CountMessage(counter, &out[idx])
		baseCosts[idx] = cost
		remaining -= cost
	}
	if remaining < 0 {
		remaining = 0
	}

	// 剩余预算从最新的往前花。装不下完整内容的结果拿到能装下的最大头/尾预览。
	for i := len(toolIndexes) - 1; i >= 0; i-- {
		idx := toolIndexes[i]
		fullCost := compaction.CountMessage(counter, &messages[idx])
		extra := fullCost - baseCosts[idx]
		if extra <= remaining {
			out[idx] = messages[idx]
			remaining -= extra
			continue
		}
		out[idx] = compactToolMessage(messages[idx], baseCosts[idx]+remaining, counter)
		remaining = 0
	}
	return out, true
}

// compactedToolResultMarker 是工具结果被压缩后留下的明确占位说明（PRD FR-06 规则 3）。
func compactedToolResultMarker(content string) string {
	return fmt.Sprintf(
		"[工具结果已压缩：原始 %d 字节已移除。如需更多细节，请用更窄的过滤条件或更小的范围重新调用该工具。]",
		len(content),
	)
}

func compactToolMessage(msg chat.Message, maxTokens int, counter *contextx.Counter) chat.Message {
	runes := []rune(msg.Content)
	base := msg
	base.Content = compactedToolResultMarker(msg.Content)
	if len(runes) == 0 || compaction.CountMessage(counter, &base) >= maxTokens {
		return base
	}

	best := base
	low, high := 1, len(runes)
	for low <= high {
		keep := low + (high-low)/2
		head := keep / 4
		tail := keep - head
		candidate := base
		candidate.Content = fmt.Sprintf(
			"%s\n\n%s\n...[工具结果预览已省略]...\n%s",
			base.Content,
			string(runes[:head]),
			string(runes[len(runes)-tail:]),
		)
		if compaction.CountMessage(counter, &candidate) <= maxTokens {
			best = candidate
			low = keep + 1
		} else {
			high = keep - 1
		}
	}
	return best
}

// responseVerdict captures the result of analyzing an LLM response to determine
// whether the agent loop should stop and what the final answer is (if any).
type responseVerdict struct {
	isDone       bool
	finalAnswer  string
	emptyContent bool // LLM returned stop with no tool calls and empty content
	step         types.AgentStep
}

// isNaturalStopFinishReason reports whether a provider finish reason means the
// assistant has ended its message without requesting more tool work.
func isNaturalStopFinishReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "stop", "end_turn", "stop_sequence":
		return true
	default:
		return false
	}
}

// analyzeResponse inspects the LLM response for stop conditions:
//   - natural finish reason with no tool calls → agent is done (natural stop)
//   - finish_reason == "content_filter" with no tool calls → agent is done (content filtered)
//
// The agent ends a turn by stopping naturally with its answer as plain
// assistant text (there is no dedicated final_answer tool). Any round that
// still requests tool calls is non-terminal and the caller continues the loop.
// It returns a responseVerdict. If isDone is true the caller should break out of the loop.
func (e *AgentEngine) analyzeResponse(
	ctx context.Context, response *types.ChatResponse,
	step types.AgentStep, iteration int, sessionID string, roundStart time.Time,
) responseVerdict {
	// Case 0: Content was blocked by the model's content filter.
	// Treat this as a terminal condition to avoid an infinite loop where
	// the same filtered response accumulates in the context.
	if response.FinishReason == "content_filter" && len(response.ToolCalls) == 0 {
		logger.Warnf(ctx, "[Agent][Round-%d] Content filter triggered, stopping agent loop (content=%d chars)",
			iteration+1, len(response.Content))
		common.PipelineWarn(ctx, "Agent", "content_filter_stop", map[string]interface{}{
			"iteration":   iteration,
			"round":       iteration + 1,
			"content_len": len(response.Content),
		})

		answer := response.Content
		if answer == "" {
			answer = "Sorry, this request was blocked by the content safety policy. Please try rephrasing your question."
		}

		answerID := generateEventID("answer")
		e.eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: answer,
				Done:    false,
			},
		})
		e.eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: "",
				Done:    true,
			},
		})

		return responseVerdict{
			isDone:      true,
			finalAnswer: answer,
			step:        step,
		}
	}

	// Case 1: LLM stopped naturally without requesting any tool calls.
	if isNaturalStopFinishReason(response.FinishReason) && len(response.ToolCalls) == 0 {
		// Strip <think>…</think> blocks that some models embed in content
		// (DeepSeek, Qwen, etc.) before processing or displaying.
		response.Content = agenttools.StripThinkBlocks(response.Content)
		logger.Infof(ctx, "[Agent][Round-%d] Agent finished naturally: answer=%d chars, duration=%dms",
			iteration+1, len(response.Content), time.Since(roundStart).Milliseconds())
		common.PipelineInfo(ctx, "Agent", "round_final_answer", map[string]interface{}{
			"iteration":  iteration,
			"round":      iteration + 1,
			"answer_len": len(response.Content),
		})

		// Emit the final answer. The answer text reaches the UI by one of two
		// paths:
		//   (a) Already streamed live during the think phase — the common case
		//       now that plain assistant content is routed straight to
		//       EventAgentFinalAnswer (response.AnswerStreamed). Re-emitting the
		//       full content here would render it twice and produce the
		//       end-of-stream "jump from Thinking to Answer" the user reported,
		//       so we only close the existing stream with a Done marker on the
		//       same event ID.
		//   (b) Not streamed live (e.g. the content only surfaced in the
		//       accumulated result) — emit the full content, then Done.
		var answerID string
		if response.AnswerStreamed && response.AnswerEventID != "" {
			answerID = response.AnswerEventID
		} else {
			answerID = generateEventID("answer")
			if response.Content != "" {
				e.eventBus.Emit(ctx, event.Event{
					ID:        answerID,
					Type:      event.EventAgentFinalAnswer,
					SessionID: sessionID,
					Data: event.AgentFinalAnswerData{
						Content: response.Content,
						Done:    false,
					},
				})
			}
		}
		e.eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: "",
				Done:    true,
			},
		})

		return responseVerdict{
			isDone:       true,
			finalAnswer:  response.Content,
			emptyContent: response.Content == "",
			step:         step,
		}
	}

	// Any round that still requests tool calls is non-terminal: the caller
	// executes the tools and loops again. The agent only ends by stopping
	// naturally (Case 1) with its answer as plain assistant text.
	return responseVerdict{isDone: false, step: step}
}

// indentLines prefixes every line of s with indent. Used to nest pre-rendered
// XML blocks inside the `<runtime_context>` envelope without losing readability.
func indentLines(s, indent string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}

// escapeXMLAttr escapes a string for safe inclusion in an XML attribute value.
// Titles and names may contain user-supplied characters like <, >, &, ".
func escapeXMLAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// buildRuntimeContextBlock builds a metadata block with current time, session
// info, and the *active retrieval scope for this turn only*. It is injected
// into the current user message for the LLM call and is not persisted into
// conversation history — replayed user turns keep bare Content so stale scope
// snapshots do not steer follow-up questions.
//
// Per-turn communication_instruction and answer_instruction remind the model
// not to leak internal tool names or IDs in user-visible text, and to end the
// turn by writing its complete answer as plain assistant text.
//
// Emitted as an XML-ish block (not free prose) so it is a visually distinct,
// non-instruction envelope that is hard to conflate with user text and
// prompt-injection-safe.
func buildRuntimeContextBlock(
	sessionID string,
	kbs []*KnowledgeBaseInfo,
	docs []*SelectedDocumentInfo,
) string {
	var sb strings.Builder
	sb.WriteString("<runtime_context scope=\"this_turn\">\n")
	fmt.Fprintf(&sb, "  <current_time>%s</current_time>\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&sb, "  <session>%s</session>\n", escapeXMLAttr(sessionID))

	if len(kbs) > 0 {
		// Render the full bound-KB detail (capabilities + recent docs) so the
		// model has everything it needs to route its retrieval in one place.
		// `formatKnowledgeBaseList` already emits a `<knowledge_bases>…</knowledge_bases>`
		// envelope; we wrap it in `<bound_knowledge_bases>` to make the scope
		// semantics explicit and to match the naming the prompt templates use
		// when referring back to this block.
		sb.WriteString("  <bound_knowledge_bases>\n")
		sb.WriteString(indentLines(formatKnowledgeBaseList(kbs), "    "))
		sb.WriteString("\n  </bound_knowledge_bases>\n")
	}

	if len(docs) > 0 {
		sb.WriteString("  <pinned_documents scope=\"authoritative_for_this_turn\">\n")
		for _, d := range docs {
			if d == nil {
				continue
			}
			title := d.Title
			if title == "" {
				title = d.FileName
			}
			if title == "" {
				title = d.KnowledgeID
			}
			if d.FileType != "" {
				fmt.Fprintf(&sb, "    <document knowledge_id=\"%s\" title=\"%s\" file_type=\"%s\" />\n",
					escapeXMLAttr(d.KnowledgeID), escapeXMLAttr(title), escapeXMLAttr(d.FileType))
			} else {
				fmt.Fprintf(&sb, "    <document knowledge_id=\"%s\" title=\"%s\" />\n",
					escapeXMLAttr(d.KnowledgeID), escapeXMLAttr(title))
			}
		}
		sb.WriteString("  </pinned_documents>\n")
		sb.WriteString("  <note>The pinned-document set above is authoritative for THIS turn. ")
		sb.WriteString("Prioritize retrieving content from these documents (e.g. list_knowledge_chunks with the knowledge_id). ")
		sb.WriteString("If an earlier turn analysed a different document, do NOT reuse that analysis — re-query against the current scope.</note>\n")
	}

	sb.WriteString("  <communication_instruction>Do not use internal tool names or identifiers in your answers or in Thought. Say \"keyword retrieval\" instead of grep_chunks, \"semantic retrieval\" instead of knowledge_search, \"browse full document\" instead of list_knowledge_chunks; likewise never expose chunk_id, knowledge_id, or other internal IDs—refer to documents by title or name.</communication_instruction>\n")
	sb.WriteString("  <answer_instruction>When you have gathered enough information, write your complete user-facing answer as your reply and stop—do not request any more tools in that final message. Until then, keep using tools; do not give a partial answer mid-investigation.</answer_instruction>\n")

	sb.WriteString("</runtime_context>")
	return sb.String()
}

// buildMustUseBlock emits a short per-turn hint when the user @mentioned MCP/Skill.
// Tool names are not listed here — they are already in the function-calling schema.
func buildMustUseBlock(mcpServices []*PinnedMCPServiceInfo, skills []*PinnedSkillInfo) string {
	var lines []string
	for _, svc := range mcpServices {
		if svc == nil {
			continue
		}
		prefix := mcpToolNamePrefix(svc)
		if prefix == "" {
			continue
		}
		display := sanitizeMustUseField(svc.Name)
		if display == "" {
			display = sanitizeMustUseField(svc.ID)
		}
		lines = append(lines, fmt.Sprintf("Must use MCP tools whose names start with %s (@%s) to answer the question below.", prefix, display))
	}
	for _, skill := range skills {
		if skill == nil || skill.Name == "" {
			continue
		}
		name := sanitizeMustUseField(skill.Name)
		lines = append(lines, fmt.Sprintf("Must call read_skill(skill_name=\"%s\") for @Skill \"%s\" before answering.", name, name))
	}
	if len(lines) == 0 {
		return ""
	}
	return "<must_use>\n" + strings.Join(lines, "\n") + "\n</must_use>"
}

// sanitizeMustUseField strips newlines and angle brackets so an MCP/skill name
// cannot break out of the <must_use> block or inject extra instruction lines.
func sanitizeMustUseField(s string) string {
	replacer := strings.NewReplacer("\n", " ", "\r", " ", "<", " ", ">", " ")
	return strings.TrimSpace(replacer.Replace(s))
}

// mcpToolNamePrefix returns the shared prefix for an MCP service's registered
// tools (e.g. mcp_iwiki_ from mcp_iwiki_getdocument). Tool names are
// mcp_{sanitized_service_name}_{tool}, and the service slug itself may contain
// underscores (sanitizeName turns spaces/hyphens into "_"), so we take the
// longest common prefix across the service's tools and trim it back to the last
// segment boundary instead of naively cutting at the first underscore.
func mcpToolNamePrefix(svc *PinnedMCPServiceInfo) string {
	if svc == nil || len(svc.ToolNames) == 0 {
		return ""
	}
	const head = "mcp_"
	var mcpNames []string
	for _, toolName := range svc.ToolNames {
		if strings.HasPrefix(toolName, head) {
			mcpNames = append(mcpNames, toolName)
		}
	}
	if len(mcpNames) == 0 {
		return ""
	}
	prefix := mcpNames[0]
	for _, name := range mcpNames[1:] {
		prefix = commonStringPrefix(prefix, name)
	}
	// Trim to the last underscore so the hint names the service prefix
	// (mcp_{service}_) rather than a partial tool name.
	if idx := strings.LastIndex(prefix, "_"); idx >= len(head)-1 {
		prefix = prefix[:idx+1]
	}
	if len(prefix) <= len(head) {
		return ""
	}
	return prefix
}

func commonStringPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

// RenderUserTurnContent builds the user-turn payload for the current LLM call
// (runtime_context + must_use + query). Used by Execute and finalize paths only;
// not written to rendered_content / history.
func (e *AgentEngine) RenderUserTurnContent(sessionID, query string) string {
	e.registerRuntimeReferences()
	runtimeCtx := buildRuntimeContextBlock(sessionID, e.knowledgeBasesInfo, e.selectedDocs)
	runtimeCtx = e.modelContext.CompactKnownText(runtimeCtx)
	mustUse := buildMustUseBlock(e.pinnedMCPServices, e.pinnedSkills)
	return composeUserTurnContent(runtimeCtx, mustUse, query)
}

// registerRuntimeReferences makes bound KBs, pinned documents and recent
// chunks addressable without exposing their durable IDs to the model.
func (e *AgentEngine) registerRuntimeReferences() {
	if e == nil || e.modelContext == nil {
		return
	}
	for _, kb := range e.knowledgeBasesInfo {
		if kb == nil {
			continue
		}
		e.modelContext.RegisterKnowledgeBase(kb.ID)
		for _, doc := range kb.RecentDocs {
			e.modelContext.RegisterDocument(doc.KnowledgeID)
			if doc.ChunkID != "" {
				title := doc.Title
				if title == "" {
					title = doc.FileName
				}
				e.modelContext.RegisterChunk(modelcontext.ChunkReference{
					ChunkID:         doc.ChunkID,
					KnowledgeID:     doc.KnowledgeID,
					KnowledgeBaseID: firstNonEmptyAgent(doc.KnowledgeBaseID, kb.ID),
					DocumentTitle:   title,
					ChunkType:       doc.Type,
				})
			}
		}
	}
	for _, doc := range e.selectedDocs {
		if doc == nil {
			continue
		}
		e.modelContext.RegisterDocument(doc.KnowledgeID)
		e.modelContext.RegisterKnowledgeBase(doc.KnowledgeBaseID)
	}
}

func firstNonEmptyAgent(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func composeUserTurnContent(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			nonEmpty = append(nonEmpty, part)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

// listToolNames returns tool.function names for logging
func listToolNames(ts []chat.Tool) []string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Function.Name)
	}
	return names
}

// buildToolsForLLM builds the tools list for LLM function calling
func (e *AgentEngine) buildToolsForLLM() []chat.Tool {
	functionDefs := e.toolRegistry.GetFunctionDefinitions()
	tools := make([]chat.Tool, 0, len(functionDefs))
	for _, def := range functionDefs {
		tools = append(tools, chat.Tool{
			Type: "function",
			Function: chat.FunctionDef{
				Name:        def.Name,
				Description: def.Description,
				Parameters:  def.Parameters,
			},
		})
	}

	return tools
}

// appendToolResults adds tool results to the in-turn message history following
// OpenAI's tool-calling format. Cross-turn persistence is handled separately:
// the final AgentSteps are written to the assistant message by the SSE handler,
// and rebuilt from DB on the next turn by service.LoadAgentHistory.
func (e *AgentEngine) appendToolResults(
	messages []chat.Message,
	step types.AgentStep,
) []chat.Message {
	if stepContainsMarkdownImage(step) {
		// 要求以独立消息插在稳定前缀之后，因此无论自定义 Agent 提示词是否替换了
		// 内置模板都同样生效；只在首次出现含图证据时注入一次，纯文本轮次不背这个
		// 负担，也不会每轮改写系统提示击穿 prompt 缓存（FR-11）。
		messages = appendAgentRetrievedImageRequirement(messages)
	}

	// Add assistant message with tool calls (if any)
	if step.Thought != "" || len(step.ToolCalls) > 0 || step.ReasoningContent != "" {
		assistantMsg := chat.Message{
			Role:             "assistant",
			Content:          step.Thought,
			ReasoningContent: step.ReasoningContent,
		}

		// Add tool calls to assistant message (following OpenAI format)
		if len(step.ToolCalls) > 0 {
			assistantMsg.ToolCalls = make([]chat.ToolCall, 0, len(step.ToolCalls))
			for _, tc := range step.ToolCalls {
				// Convert arguments back to JSON string
				argsJSON, _ := json.Marshal(tc.Args)

				assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, chat.ToolCall{
					ID:               tc.ID,
					Type:             "function",
					ProviderMetadata: tc.ProviderMetadata,
					Function: chat.FunctionCall{
						Name:      tc.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}

		messages = append(messages, assistantMsg)
	}

	// Add tool result messages (role: "tool", following OpenAI format)
	for _, toolCall := range step.ToolCalls {
		resultContent := e.modelContext.ModelToolResultForTool(toolCall.Name, toolCall.Result)

		toolMsg := chat.Message{
			Role:       "tool",
			Content:    resultContent,
			ToolCallID: toolCall.ID,
			Name:       toolCall.Name,
		}

		messages = append(messages, toolMsg)
	}

	return messages
}

// countTotalToolCalls counts total tool calls across all steps
func countTotalToolCalls(steps []types.AgentStep) int {
	total := 0
	for _, step := range steps {
		total += len(step.ToolCalls)
	}
	return total
}

// kbToolNames lists tools whose results contain knowledge base content that
// may become stale across turns (KB can be switched, updated, or deleted).
// Historical results from these tools are redacted to force fresh retrieval.
var kbToolNames = map[string]bool{
	agenttools.ToolKnowledgeSearch:     true,
	agenttools.ToolGrepChunks:          true,
	agenttools.ToolListKnowledgeChunks: true,
	agenttools.ToolQueryKnowledgeGraph: true,
	agenttools.ToolGetDocumentInfo:     true,
	agenttools.ToolWikiSearch:          true,
	agenttools.ToolWikiReadPage:        true,
	agenttools.ToolWikiReadSourceDoc:   true,
}

// redactHistoryKBResults replaces full KB tool results in historical context
// with brief markers. This prevents the LLM from reusing stale retrieval data
// when the knowledge base has been modified or switched between turns.
func redactHistoryKBResults(llmContext []chat.Message) []chat.Message {
	redacted := make([]chat.Message, 0, len(llmContext))
	for _, msg := range llmContext {
		if msg.Role == "tool" && kbToolNames[msg.Name] {
			redacted = append(redacted, chat.Message{
				Role:       msg.Role,
				Content:    "[Previous retrieval result omitted — knowledge base may have changed. Please perform a fresh search.]",
				ToolCallID: msg.ToolCallID,
				Name:       msg.Name,
			})
		} else {
			redacted = append(redacted, msg)
		}
	}
	return redacted
}

// buildMessagesWithLLMContext builds the message array with LLM context
func (e *AgentEngine) buildMessagesWithLLMContext(
	systemPrompt, currentQuery, sessionID string,
	llmContext []chat.Message,
	imageURLs []string,
) []chat.Message {
	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
	}
	e.historyAppended = 0

	if len(llmContext) > 0 {
		var sanitized []chat.Message
		if e.config.RetainRetrievalHistory {
			sanitized = llmContext
			logger.Infof(context.Background(), "Retaining full retrieval history in context (RetainRetrievalHistory=true)")
		} else {
			// Redact KB tool results from previous turns to prevent the LLM
			// from reusing stale retrieval data when the KB has been modified.
			sanitized = redactHistoryKBResults(llmContext)
			logger.Infof(context.Background(), "Added %d history messages to context (KB tool results redacted)", len(llmContext))
		}

		appended := 0
		for _, msg := range sanitized {
			if msg.Role == "system" {
				continue
			}
			if msg.Role == "user" || msg.Role == "assistant" || msg.Role == "tool" {
				messages = append(messages, msg)
				appended++
			}
		}
		// 记录实际转写的条数：装载侧给出的轮次区间以 llmContext 下标表示，只有 1:1
		// 转写时才能平移到消息下标（PRD FR-10，见 attachHistoryView）。
		e.historyAppended = appended
	}

	// Build the current user message through the same registration path used by
	// final synthesis. Calling buildRuntimeContextBlock directly here would put
	// durable bound-KB/document IDs into the first model request before the
	// request-local source registry had seen them.
	userMsg := chat.Message{
		Role:    "user",
		Content: e.RenderUserTurnContent(sessionID, currentQuery),
		Images:  imageURLs,
	}
	messages = append(messages, userMsg)

	return messages
}
