package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// AgentStreamHandler handles agent events for SSE streaming
// It uses a dedicated EventBus per request to avoid SessionID filtering
// Events are appended to StreamManager without accumulation
type AgentStreamHandler struct {
	ctx                context.Context
	sessionID          string
	assistantMessageID string
	requestID          string
	receivedAt         time.Time // Handler entry timestamp, used for TTFB logging
	ttfbLogged         bool      // Guards one-shot TTFB log on first answer chunk
	assistantMessage   *types.Message
	streamManager      interfaces.StreamManager

	eventBus *event.EventBus

	// State tracking
	knowledgeRefs   []*types.SearchResult
	finalAnswer     string
	answerSegments  []*answerSegment     // Per-answer-event-ID accumulation, so superseded preambles can be dropped
	eventStartTimes map[string]time.Time // Track start time for duration calculation
	mu              sync.Mutex
}

// answerSegment accumulates the streamed content of a single final-answer event
// ID. A non-terminal round may stream a preamble ("let me search…") under its
// own answer ID and then be marked superseded once the round turns out to call
// tools; tracking segments separately lets us exclude that preamble from the
// persisted assistant message instead of leaking it into the final answer.
type answerSegment struct {
	id         string
	content    string
	superseded bool
}

// findAnswerSegment returns the segment for an answer event ID, or nil.
// Callers must hold h.mu.
func (h *AgentStreamHandler) findAnswerSegment(id string) *answerSegment {
	for _, seg := range h.answerSegments {
		if seg.id == id {
			return seg
		}
	}
	return nil
}

// composeFinalAnswer rebuilds the persisted answer from all non-superseded
// segments in arrival order. Callers must hold h.mu.
func (h *AgentStreamHandler) composeFinalAnswer() string {
	var b strings.Builder
	for _, seg := range h.answerSegments {
		if !seg.superseded {
			b.WriteString(seg.content)
		}
	}
	return b.String()
}

// NewAgentStreamHandler creates a new handler for agent SSE streaming
func NewAgentStreamHandler(
	ctx context.Context,
	sessionID, assistantMessageID, requestID string,
	receivedAt time.Time,
	assistantMessage *types.Message,
	streamManager interfaces.StreamManager,
	eventBus *event.EventBus,
) *AgentStreamHandler {
	return &AgentStreamHandler{
		ctx:                ctx,
		sessionID:          sessionID,
		assistantMessageID: assistantMessageID,
		requestID:          requestID,
		receivedAt:         receivedAt,
		assistantMessage:   assistantMessage,
		streamManager:      streamManager,
		eventBus:           eventBus,
		knowledgeRefs:      make([]*types.SearchResult, 0),
		eventStartTimes:    make(map[string]time.Time),
	}
}

// Subscribe subscribes to all agent streaming events on the dedicated EventBus
// No SessionID filtering needed since we have a dedicated EventBus per request
func (h *AgentStreamHandler) Subscribe() {
	// Subscribe to all agent streaming events on the dedicated EventBus
	h.eventBus.On(event.EventAgentThought, h.handleThought)
	h.eventBus.On(event.EventAgentToolCall, h.handleToolCall)
	h.eventBus.On(event.EventAgentToolResult, h.handleToolResult)
	h.eventBus.On(event.EventAgentReferences, h.handleReferences)
	h.eventBus.On(event.EventAgentFinalAnswer, h.handleFinalAnswer)
	h.eventBus.On(event.EventAgentReflection, h.handleReflection)
	h.eventBus.On(event.EventError, h.handleError)
	h.eventBus.On(event.EventSessionTitle, h.handleSessionTitle)
	h.eventBus.On(event.EventAgentComplete, h.handleComplete)
	h.eventBus.On(event.EventToolApprovalRequired, h.handleToolApprovalRequired)
	h.eventBus.On(event.EventToolApprovalResolved, h.handleToolApprovalResolved)
	h.eventBus.On(event.EventMCPOAuthRequired, h.handleMCPOAuthRequired)
	h.eventBus.On(event.EventMCPOAuthResolved, h.handleMCPOAuthResolved)
}

// handleThought handles agent thought events
func (h *AgentStreamHandler) handleThought(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentThoughtData)
	if !ok {
		return nil
	}

	h.mu.Lock()

	// Track start time on first chunk
	if _, exists := h.eventStartTimes[evt.ID]; !exists {
		h.eventStartTimes[evt.ID] = time.Now()
	}

	// Calculate duration if done
	var metadata map[string]interface{}
	if data.Done {
		startTime := h.eventStartTimes[evt.ID]
		duration := time.Since(startTime)
		metadata = map[string]interface{}{
			"event_id":     evt.ID,
			"duration_ms":  duration.Milliseconds(),
			"completed_at": time.Now().Unix(),
		}
		delete(h.eventStartTimes, evt.ID)
	} else {
		metadata = map[string]interface{}{
			"event_id": evt.ID,
		}
	}

	h.mu.Unlock()

	// Append this chunk to stream (no accumulation - frontend will accumulate)
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeThinking,
		Content:   data.Content, // Just this chunk
		Done:      data.Done,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append thought event to stream failed", "error", err)
	}

	return nil
}

// handleToolCall handles tool call events
func (h *AgentStreamHandler) handleToolCall(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentToolCallData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	// Track start time for this tool call (use tool_call_id as key)
	h.eventStartTimes[data.ToolCallID] = time.Now()
	// Any answer text streamed before this tool call was a non-terminal round's
	// preamble, not the final answer (the agent only ends by stopping naturally
	// with plain text and no tool calls). Drop those segments from the persisted
	// answer so the preamble never leaks into Message.Content.
	supersededAny := false
	for _, seg := range h.answerSegments {
		if !seg.superseded && seg.content != "" {
			seg.superseded = true
			supersededAny = true
		}
	}
	if supersededAny {
		h.finalAnswer = h.composeFinalAnswer()
	}
	h.mu.Unlock()

	metadata := map[string]interface{}{
		"tool_name":    data.ToolName,
		"arguments":    data.Arguments,
		"tool_call_id": data.ToolCallID,
	}

	// Append event to stream
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeToolCall,
		Content:   fmt.Sprintf("Calling tool: %s", data.ToolName),
		Done:      false,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append tool call event to stream failed", "error", err)
	}

	return nil
}

// handleToolResult handles tool result events
func (h *AgentStreamHandler) handleToolResult(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentToolResultData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	// Calculate duration from start time if available, otherwise use provided duration
	var durationMs int64
	if startTime, exists := h.eventStartTimes[data.ToolCallID]; exists {
		durationMs = time.Since(startTime).Milliseconds()
		delete(h.eventStartTimes, data.ToolCallID)
	} else if data.Duration > 0 {
		// Fallback to provided duration if start time not tracked
		durationMs = data.Duration
	}
	h.mu.Unlock()

	// Send SSE response (both success and failure)
	responseType := types.ResponseTypeToolResult
	content := agenttools.StreamContentForToolResult(data.ToolName, data.Success, data.Error, data.Data)
	if !data.Success {
		responseType = types.ResponseTypeError
		if content == "" && data.Error != "" {
			content = data.Error
		}
	}

	// Build metadata including tool result data for rich frontend rendering
	metadata := map[string]interface{}{
		"tool_name":    data.ToolName,
		"success":      data.Success,
		"error":        data.Error,
		"duration_ms":  durationMs,
		"tool_call_id": data.ToolCallID,
	}

	clientData := agenttools.SanitizeToolResultForClient(data.ToolName, &types.ToolResult{
		Success: data.Success,
		Output:  data.Output,
		Error:   data.Error,
		Data:    data.Data,
	})
	for k, v := range clientData {
		metadata[k] = v
	}

	// Append event to stream
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      responseType,
		Content:   content,
		Done:      false,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append tool result event to stream failed", "error", err)
	}

	return nil
}

func toolApprovalDataToMap(v interface{}) map[string]interface{} {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]interface{}{}
	}
	return m
}

// handleToolApprovalRequired persists MCP tool human-approval prompts for SSE / replay (issue #1173).
func (h *AgentStreamHandler) handleToolApprovalRequired(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.ToolApprovalRequiredData)
	if !ok {
		return nil
	}
	meta := toolApprovalDataToMap(data)
	meta["pending_id"] = data.PendingID
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeToolApprovalRequired,
		Content:   "MCP tool requires human approval",
		Done:      true,
		Timestamp: time.Now(),
		Data:      meta,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append tool approval required event failed", "error", err)
	}
	return nil
}

// handleToolApprovalResolved persists the outcome of a tool approval (issue #1173).
func (h *AgentStreamHandler) handleToolApprovalResolved(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.ToolApprovalResolvedData)
	if !ok {
		return nil
	}
	meta := toolApprovalDataToMap(data)
	meta["pending_id"] = data.PendingID
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeToolApprovalResolved,
		Content:   "MCP tool approval resolved",
		Done:      true,
		Timestamp: time.Now(),
		Data:      meta,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append tool approval resolved event failed", "error", err)
	}
	return nil
}

// handleMCPOAuthRequired forwards an in-conversation "authorize this MCP
// service" prompt to the SSE stream so the UI can render an Authorize card.
func (h *AgentStreamHandler) handleMCPOAuthRequired(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.MCPOAuthRequiredData)
	if !ok {
		return nil
	}
	meta := toolApprovalDataToMap(data)
	meta["pending_id"] = data.PendingID
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeMCPOAuthRequired,
		Content:   "MCP service requires OAuth authorization",
		Done:      true,
		Timestamp: time.Now(),
		Data:      meta,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append mcp oauth required event failed", "error", err)
	}
	return nil
}

// handleMCPOAuthResolved forwards the outcome of an in-conversation OAuth prompt.
func (h *AgentStreamHandler) handleMCPOAuthResolved(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.MCPOAuthResolvedData)
	if !ok {
		return nil
	}
	meta := toolApprovalDataToMap(data)
	meta["pending_id"] = data.PendingID
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeMCPOAuthResolved,
		Content:   "MCP OAuth authorization resolved",
		Done:      true,
		Timestamp: time.Now(),
		Data:      meta,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append mcp oauth resolved event failed", "error", err)
	}
	return nil
}

// handleReferences handles knowledge references events
func (h *AgentStreamHandler) handleReferences(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentReferencesData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Extract knowledge references
	// Try to cast directly to []*types.SearchResult first
	if searchResults, ok := data.References.([]*types.SearchResult); ok {
		h.knowledgeRefs = append(h.knowledgeRefs, searchResults...)
	} else if refs, ok := data.References.([]interface{}); ok {
		// Fallback: convert from []interface{}
		for _, ref := range refs {
			if sr, ok := ref.(*types.SearchResult); ok {
				h.knowledgeRefs = append(h.knowledgeRefs, sr)
			} else if refMap, ok := ref.(map[string]interface{}); ok {
				// Parse from map if needed
				searchResult := &types.SearchResult{
					ID:                   getString(refMap, "id"),
					Content:              getString(refMap, "content"),
					Score:                getFloat64(refMap, "score"),
					KnowledgeID:          getString(refMap, "knowledge_id"),
					KnowledgeTitle:       getString(refMap, "knowledge_title"),
					ChunkIndex:           int(getFloat64(refMap, "chunk_index")),
					KnowledgeDescription: getString(refMap, "knowledge_description"),
					KnowledgeBaseID:      getString(refMap, "knowledge_base_id"),
				}

				if meta, ok := refMap["metadata"].(map[string]interface{}); ok {
					metadata := make(map[string]string)
					for k, v := range meta {
						if strVal, ok := v.(string); ok {
							metadata[k] = strVal
						}
					}
					searchResult.Metadata = metadata
				}

				// FR-05 引用溯源：解析 source_locators（容忍缺失/格式错误）
				if rawLocs, ok := refMap["source_locators"]; ok && rawLocs != nil {
					if locBytes, err := json.Marshal(rawLocs); err == nil {
						var locs types.SourceLocators
						if err := json.Unmarshal(locBytes, &locs); err == nil {
							searchResult.SourceLocators = locs
						}
					}
				}

				h.knowledgeRefs = append(h.knowledgeRefs, searchResult)
			}
		}
	}

	// Update assistant message references
	h.assistantMessage.KnowledgeReferences = h.knowledgeRefs

	// Append references event to stream
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeReferences,
		Content:   "",
		Done:      false,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"references": types.References(h.knowledgeRefs),
		},
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append references event to stream failed", "error", err)
	}

	return nil
}

// handleFinalAnswer handles final answer events
func (h *AgentStreamHandler) handleFinalAnswer(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentFinalAnswerData)
	if !ok {
		return nil
	}

	h.mu.Lock()

	// Track start time on first chunk
	if _, exists := h.eventStartTimes[evt.ID]; !exists {
		h.eventStartTimes[evt.ID] = time.Now()
	}

	// Emit a one-shot TTFB log the first time *any* answer chunk reaches
	// the stream handler. This lets us compare the backend's "request in →
	// first token out" timing against the frontend-observed TTFB and pin
	// down where latency lives (network vs server vs LLM).
	if !h.ttfbLogged && !h.receivedAt.IsZero() {
		h.ttfbLogged = true
		ttfb := time.Since(h.receivedAt)
		logger.GetLogger(h.ctx).Infof("TTFB:first_answer_chunk request_id=%s, session_id=%s, ttfb_ms=%d",
			h.requestID, h.sessionID, ttfb.Milliseconds())
	}

	// Accumulate final answer locally for assistant message (database). Track
	// per event ID so a later supersede can subtract this segment's content.
	if data.Content != "" {
		seg := h.findAnswerSegment(evt.ID)
		if seg == nil {
			seg = &answerSegment{id: evt.ID}
			h.answerSegments = append(h.answerSegments, seg)
		}
		seg.content += data.Content
		h.finalAnswer = h.composeFinalAnswer()
	}
	if data.IsFallback {
		h.assistantMessage.IsFallback = true
	}

	// Calculate duration if done
	var metadata map[string]interface{}
	if data.Done {
		startTime := h.eventStartTimes[evt.ID]
		duration := time.Since(startTime)
		metadata = map[string]interface{}{
			"event_id":     evt.ID,
			"duration_ms":  duration.Milliseconds(),
			"completed_at": time.Now().Unix(),
		}
		delete(h.eventStartTimes, evt.ID)
	} else {
		metadata = map[string]interface{}{
			"event_id": evt.ID,
		}
	}
	if data.IsFallback {
		metadata["is_fallback"] = true
	}
	h.mu.Unlock()

	// Append this chunk to stream (frontend will accumulate by event ID)
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeAnswer,
		Content:   data.Content, // Just this chunk
		Done:      data.Done,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append answer event to stream failed", "error", err)
	}

	return nil
}

// handleReflection handles agent reflection events
func (h *AgentStreamHandler) handleReflection(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentReflectionData)
	if !ok {
		return nil
	}

	// Append this chunk to stream (frontend will accumulate by event ID)
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeReflection,
		Content:   data.Content, // Just this chunk
		Done:      data.Done,
		Timestamp: time.Now(),
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append reflection event to stream failed", "error", err)
	}

	return nil
}

// handleError handles error events
func (h *AgentStreamHandler) handleError(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.ErrorData)
	if !ok {
		return nil
	}

	// Build error metadata
	metadata := map[string]interface{}{
		"stage": data.Stage,
		"error": data.Error,
	}

	// Append error event to stream
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeError,
		Content:   data.Error,
		Done:      true,
		Timestamp: time.Now(),
		Data:      metadata,
	}); err != nil {
		logger.GetLogger(h.ctx).Error("Append error event to stream failed", "error", err)
	}

	return nil
}

// handleSessionTitle handles session title update events
func (h *AgentStreamHandler) handleSessionTitle(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.SessionTitleData)
	if !ok {
		return nil
	}

	// Use background context for title event since it may arrive after stream completion
	bgCtx := context.Background()

	// Append title event to stream
	if err := h.streamManager.AppendEvent(bgCtx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeSessionTitle,
		Content:   data.Title,
		Done:      true,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"session_id": data.SessionID,
			"title":      data.Title,
		},
	}); err != nil {
		logger.GetLogger(h.ctx).Warn("Append session title event to stream failed (stream may have ended)", "error", err)
	}

	return nil
}

// applyTurnUsage 把一轮的用量与归因写到 assistant 消息的内存态上，持久化由调用方负责
// （两条路径的写入时机不同，见 qa.go 与 handleComplete）。
//
// 只覆盖非空值：供应商没有回报 usage 时保留原有列，不用 NULL 抹掉先前写入的数字。
func applyTurnUsage(msg *types.Message, usage *types.TokenUsage, stats *types.ContextTurnStats) {
	if msg == nil {
		return
	}
	if usage != nil && !usage.IsZero() {
		msg.Usage = usage
	}
	if stats != nil && !stats.Empty() {
		msg.ContextStats = stats
	}
}

// handleComplete handles agent complete events
func (h *AgentStreamHandler) handleComplete(ctx context.Context, evt event.Event) error {
	data, ok := evt.Data.(event.AgentCompleteData)
	if !ok {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// 本轮的 token 用量与上下文归因（PRD FR-12）。普通模式下这个 Extra 由 qa.go 从
	// chat.complete 事件转挂过来，Agent 模式下由引擎直接挂上，两条路径同一个形状。
	turnUsage, turnStats := types.TurnUsageFromExtra(data.Extra)

	// Update assistant message with final data
	if data.MessageID == h.assistantMessageID {
		// h.assistantMessage.Content = data.FinalAnswer
		h.assistantMessage.IsCompleted = true
		h.assistantMessage.AgentDurationMs = data.TotalDurationMs

		// Update knowledge references if provided
		if len(data.KnowledgeRefs) > 0 {
			knowledgeRefs := make([]*types.SearchResult, 0, len(data.KnowledgeRefs))
			for _, ref := range data.KnowledgeRefs {
				if sr, ok := ref.(*types.SearchResult); ok {
					knowledgeRefs = append(knowledgeRefs, sr)
				}
			}
			h.assistantMessage.KnowledgeReferences = knowledgeRefs
		}

		h.assistantMessage.Content += data.FinalAnswer

		// Update agent steps if provided
		if data.AgentSteps != nil {
			if steps, ok := data.AgentSteps.([]types.AgentStep); ok {
				h.assistantMessage.AgentSteps = agenttools.SanitizeAgentStepsForStorage(steps)
			}
		}

		// Agent 模式：completeAssistantMessage 在 AgentQA 返回之后才跑（见 qa.go 的
		// defer），因此这里只改内存态，落库由那次 UpdateMessage 一并完成。
		applyTurnUsage(h.assistantMessage, turnUsage, turnStats)
	}

	// Fallback: if no answer events were streamed but we have a final answer,
	// emit it as answer events so the frontend can render it properly.
	// This guards against edge cases where the LLM stops without calling final_answer.
	if h.finalAnswer == "" && data.FinalAnswer != "" {
		logger.GetLogger(h.ctx).Warnf(
			"No answer events were streamed, emitting fallback answer (len=%d). "+
				"This typically happens when: (1) model stopped naturally and content was sent as thought events, "+
				"or (2) Ollama model returned tool calls non-incrementally. "+
				"total_steps=%d, total_duration_ms=%d",
			len(data.FinalAnswer), data.TotalSteps, data.TotalDurationMs,
		)
		fallbackID := fmt.Sprintf("answer-fallback-%d", time.Now().UnixMilli())
		if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
			ID:        fallbackID,
			Type:      types.ResponseTypeAnswer,
			Content:   data.FinalAnswer,
			Done:      false,
			Timestamp: time.Now(),
			Data: map[string]interface{}{
				"event_id":    fallbackID,
				"is_fallback": true,
			},
		}); err != nil {
			logger.GetLogger(h.ctx).Errorf("Append fallback answer event failed: %v", err)
		}
		if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
			ID:        fallbackID,
			Type:      types.ResponseTypeAnswer,
			Content:   "",
			Done:      true,
			Timestamp: time.Now(),
			Data: map[string]interface{}{
				"event_id":    fallbackID,
				"is_fallback": true,
			},
		}); err != nil {
			logger.GetLogger(h.ctx).Errorf("Append fallback answer done event failed: %v", err)
		}
	}

	// Send completion event to stream manager so SSE can detect completion
	completeData := map[string]interface{}{
		"total_steps":       data.TotalSteps,
		"total_duration_ms": data.TotalDurationMs,
	}
	// 本轮用量随 complete 事件一起推给前端（PRD FR-12）：既放进 Data 供 map 消费者
	// 读取，也放到强类型的 Usage 字段上，由 buildStreamResponse 提升到
	// StreamResponse.Usage。归因明细不推流——它随消息持久化，由会话详情接口带出。
	if turnUsage != nil {
		completeData["usage"] = turnUsage
	}
	if err := h.streamManager.AppendEvent(h.ctx, h.sessionID, h.assistantMessageID, interfaces.StreamEvent{
		ID:        evt.ID,
		Type:      types.ResponseTypeComplete,
		Content:   "",
		Done:      true,
		Timestamp: time.Now(),
		Data:      completeData,
		Usage:     turnUsage,
	}); err != nil {
		logger.GetLogger(h.ctx).Errorf("Append complete event to stream failed: %v", err)
	}

	return nil
}
