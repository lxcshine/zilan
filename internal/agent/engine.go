package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	"github.com/Tencent/WeKnora/internal/agent/skills"
	agenttoken "github.com/Tencent/WeKnora/internal/agent/token"
	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/common"
	appconfig "github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/modelcontext"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
)

// langfuseQueryPreview caps the query length we ship as the agent.execute
// span input — long quoted-context queries can be many KB of prose.
const langfuseQueryPreview = 2000

// AgentEngine is the core engine for running ReAct agents.
//
// History persistence note: the engine is stateless across turns. Conversation
// history is rebuilt from the DB once per turn by the caller
// (see service.LoadAgentHistory) and passed into Execute as llmContext. The
// engine therefore does not maintain its own cache, system-prompt store, or
// cross-turn buffer.
type AgentEngine struct {
	config                   *types.AgentConfig
	toolRegistry             *agenttools.ToolRegistry
	chatModel                chat.Chat
	eventBus                 *event.EventBus
	knowledgeBasesInfo       []*KnowledgeBaseInfo    // Detailed knowledge base information for prompt
	selectedDocs             []*SelectedDocumentInfo // User-selected documents (via @ mention)
	pinnedMCPServices        []*PinnedMCPServiceInfo // User @mentioned MCP services for this turn
	pinnedSkills             []*PinnedSkillInfo      // User @mentioned skills for this turn
	sessionID                string                  // Session ID for logging and event emission
	systemPromptTemplate     string                  // System prompt template (optional, uses default if empty)
	skillsManager            *skills.Manager         // Skills manager for Progressive Disclosure (optional)
	appConfig                *appconfig.Config       // Application config for prompt template resolution (optional)
	imageDescriber           ImageDescriberFunc      // VLM function for describing images in tool results (optional)
	tokenEstimator           *agenttoken.Estimator   // Token estimator for context window management
	lastUsage                types.TokenUsage        // Token usage from the most recent LLM call
	lastSentMsgCount         int                     // Number of messages sent in the most recent LLM call
	modelContext             *modelcontext.Registry  // single request-local boundary for every model handle
	govTokenCounter          *contextx.Counter       // vendor-calibrated counter for context governance (lazy)
	lastScratchpadCompressAt int                     // tool-step count at the last scratchpad compression

	// turnUsage 是本轮所有模型往返（每个 ReAct 轮次 + 最终答案合成，含溢出重试）的
	// 累加用量（PRD FR-12）。lastUsage 只是"最近一次"的内存态基线，用于估算当前上下文
	// 规模；持久化到 messages.usage 上的是本轮总量，因此两者不能互相替代。
	turnUsage types.TokenUsage
	// turnRounds 是本轮真正发生的模型往返数，写进 ContextTurnStats.Rounds。
	turnRounds int
	// turnStats 是本轮的上下文归因（PRD FR-12），懒创建：窗口口径与压缩/截断事件
	// 都从这里进出，最终随 EventAgentComplete 的 Extra 交给 handler 落库。
	turnStats *types.ContextTurnStats

	// compactor 把较早的历史摘要掉以适配上下文窗口（nil 表示未启用），承载 PRD FR-06。
	compactor *compaction.Compactor
	// overflowRecovered 记录本轮已经用掉了那唯一一次「压缩后重试」。第二次尝试意味着历史
	// 体积从来不是问题所在，继续循环只会白烧轮次预算。每个 turn 复位一次。
	overflowRecovered bool
	// compactionExhaustedAt 是压缩上一次报告"无可释放"时的消息数。低于该计数时答案不会
	// 变化，因此没有理由再付一次摘要调用。
	compactionExhaustedAt int

	// checkpointSink 持久化压缩检查点（PRD FR-10）。nil（默认）表示每次压缩只在本轮内
	// 生效，下一轮重新付一次摘要调用。
	checkpointSink types.ContextCheckpointSink
	// historyView 是本轮装载历史得到的轮次身份视图（坐标为 llmContext 下标），
	// Execute 会把它平移到引擎消息坐标后交给压缩器。
	historyView *compaction.HistoryView
	// historyAppended 是 buildMessagesWithLLMContext 实际转写进消息列表的历史条数，
	// 用于校验轮次区间与消息下标仍然 1:1 对齐（见 attachHistoryView）。
	historyAppended int
}

// ImageDescriberFunc generates a text description of an image.
// Signature matches vlm.VLM.Predict so it can be injected without importing the vlm package.
type ImageDescriberFunc func(ctx context.Context, imgBytes []byte, prompt string) (string, error)

// NewAgentEngine creates a new agent engine
func NewAgentEngine(
	config *types.AgentConfig,
	chatModel chat.Chat,
	toolRegistry *agenttools.ToolRegistry,
	eventBus *event.EventBus,
	knowledgeBasesInfo []*KnowledgeBaseInfo,
	selectedDocs []*SelectedDocumentInfo,
	sessionID string,
	systemPromptTemplate string,
) *AgentEngine {
	if eventBus == nil {
		eventBus = event.NewEventBus()
	}
	tokenEst, err := agenttoken.NewEstimator()
	if err != nil {
		return nil
	}
	engine := &AgentEngine{
		config:               config,
		toolRegistry:         toolRegistry,
		chatModel:            chatModel,
		eventBus:             eventBus,
		knowledgeBasesInfo:   knowledgeBasesInfo,
		selectedDocs:         selectedDocs,
		sessionID:            sessionID,
		systemPromptTemplate: systemPromptTemplate,
		tokenEstimator:       tokenEst,
		modelContext:         modelcontext.NewRegistry(config.CitationsEnabled()),
	}

	// 挂载上下文压缩器（PRD FR-06）。token 计量走 internal/contextx 的多厂商精确
	// tokenizer（govCounter），而非 BPE 估算——这是 PRD §4.9 的净领先点。
	engine.compactor = compaction.New(chatModel, engine.govCounter(), compaction.Settings{
		Enabled:          config.MaxContextTokens > 0,
		MaxContextTokens: config.MaxContextTokens,
		ReserveTokens:    reserveTokensFor(config),
	})

	return engine
}

// contextSafetyTokens 是「我们估算的请求开销」与「供应商实际计数」之间的余量。token
// 计数是近似的，供应商还会加上自己的脚手架；没有余量时，那个按我们的算术恰好装得下的
// 请求正是会被拒绝的那个。
const contextSafetyTokens = 4096

// defaultCompletionTokenBudget 是每轮 ReAct 请求假定的补全预算。Zilan 不显式设置
// max_tokens（由供应商默认），这里取一个保守值用于推导上下文预留量。
const defaultCompletionTokenBudget = 8192

// reserveTokensFor 是历史不得占用的那部分窗口，因为下一次回复必须放得进去。
func reserveTokensFor(cfg *types.AgentConfig) int {
	reserve := max(defaultCompletionTokenBudget+contextSafetyTokens, compaction.DefaultReserveTokens)
	// 小窗口下把预留压到窗口的一半：否则阈值退化为 1，每一轮都会触发压缩，而保留预算
	// 也会大于窗口本身，压缩根本无处可切。
	if cfg != nil && cfg.MaxContextTokens > 0 {
		reserve = min(reserve, max(cfg.MaxContextTokens/2, 1))
	}
	return reserve
}

// SetPinnedMentions sets per-turn @mention scope for MCP services and skills.
func (e *AgentEngine) SetPinnedMentions(mcpServices []*PinnedMCPServiceInfo, skills []*PinnedSkillInfo) {
	e.pinnedMCPServices = mcpServices
	e.pinnedSkills = skills
}

func (e *AgentEngine) systemPromptOptions(ctx context.Context) *BuildSystemPromptOptions {
	opts := &BuildSystemPromptOptions{
		Language: types.LanguageNameFromContext(ctx),
		Config:   e.appConfig,
	}
	if e.skillsManager != nil && e.skillsManager.IsEnabled() {
		opts.SkillsMetadata = e.skillsManager.GetAllMetadata()
	}
	return opts
}

func (e *AgentEngine) buildSystemPrompt(ctx context.Context) string {
	prompt := BuildSystemPromptWithOptions(
		e.knowledgeBasesInfo,
		e.config.WebSearchEnabled,
		e.systemPromptOptions(ctx),
		e.systemPromptTemplate,
	)
	return strings.TrimRight(prompt, " \t\r\n") + e.modelContext.ProtocolPrompt()
}

// NewAgentEngineWithSkills creates a new agent engine with skills support
func NewAgentEngineWithSkills(
	config *types.AgentConfig,
	chatModel chat.Chat,
	toolRegistry *agenttools.ToolRegistry,
	eventBus *event.EventBus,
	knowledgeBasesInfo []*KnowledgeBaseInfo,
	selectedDocs []*SelectedDocumentInfo,
	sessionID string,
	systemPromptTemplate string,
	skillsManager *skills.Manager,
) *AgentEngine {
	engine := NewAgentEngine(
		config,
		chatModel,
		toolRegistry,
		eventBus,
		knowledgeBasesInfo,
		selectedDocs,
		sessionID,
		systemPromptTemplate,
	)
	engine.skillsManager = skillsManager
	return engine
}

// SetAppConfig sets the application config for prompt template resolution.
// This allows the engine to read default prompts from config/prompt_templates/ YAML files.
func (e *AgentEngine) SetAppConfig(cfg *appconfig.Config) {
	e.appConfig = cfg
}

// SetImageDescriber sets the VLM function for generating text descriptions of images
// in tool results. When set, MCP tool result images are automatically analyzed and
// their descriptions are appended to the tool message content.
// This follows the same pattern as Handler.analyzeImageAttachments() in the handler layer.
func (e *AgentEngine) SetImageDescriber(fn ImageDescriberFunc) {
	e.imageDescriber = fn
}

// SetSkillsManager sets the skills manager for the engine
func (e *AgentEngine) SetSkillsManager(manager *skills.Manager) {
	e.skillsManager = manager
}

// GetSkillsManager returns the skills manager
func (e *AgentEngine) GetSkillsManager() *skills.Manager {
	return e.skillsManager
}

// estimateCurrentTokens 返回当前上下文 token 数的最佳估计。上一轮有 API 报告的 usage 时，
// 以它为基线，只对新追加的消息计量增量；否则对全部消息计量。
//
// 计量走 internal/contextx 的多厂商精确 tokenizer（含图片的保守估值），与压缩阈值同口径
// （PRD §4.9：Zilan 使用精确 tokenizer 而非估算）。
func (e *AgentEngine) estimateCurrentTokens(messages []chat.Message) int {
	if e.lastUsage.TotalTokens > 0 && e.lastSentMsgCount > 0 && e.lastSentMsgCount < len(messages) {
		delta := e.countContextTokens(messages[e.lastSentMsgCount:])
		return e.lastUsage.TotalTokens + delta
	}
	return e.countContextTokens(messages)
}

// ---------------------------------------------------------------------------
// 每轮用量与上下文归因的采集（PRD FR-12）
//
// 引擎每轮新建（见 CreateAgentEngine），因此这里的状态天然只描述当前这一轮；
// Execute 仍然显式复位一次，避免任何未来的复用把上一轮的数字带进来。
// 口径约束：token 数只来自供应商回报的 usage 与 internal/contextx 的精确
// tokenizer（compaction.CountMessages / govCounter），绝不用字符数估算。
// ---------------------------------------------------------------------------

// resetTurnAccounting 复位本轮的用量与归因采集。
func (e *AgentEngine) resetTurnAccounting() {
	e.turnUsage = types.TokenUsage{}
	e.turnRounds = 0
	e.turnStats = nil
}

// accumulateTurnUsage 把一次模型调用的用量累加进本轮总量。
//
// 一轮对话包含多次模型往返（每个 ReAct 轮次、溢出后的重试、最终答案合成），
// 而 messages.usage 上持久化的是本轮总量，因此每一次回报都要累加（见
// types.TokenUsage.Accumulate）。供应商未回报 usage（nil）时不计入往返数：
// 那一次调用没有任何可归因的数字。
func (e *AgentEngine) accumulateTurnUsage(usage *types.TokenUsage) {
	if e == nil || usage == nil {
		return
	}
	e.turnUsage.Accumulate(*usage)
	e.turnRounds++
}

// turnContextStats 返回本轮的上下文归因，首次调用时按当前配置建好骨架。
//
// Agent 链路不做 L0–L4 五层预算切分（那是 chat pipeline 装配器的职责），因此
// Budget / Used 保持零值；看板据此把 Agent 轮次排除在五层占比的分母之外，
// 只统计它确实提供的窗口口径、往返数与压缩/截断事件。
func (e *AgentEngine) turnContextStats() *types.ContextTurnStats {
	if e.turnStats != nil {
		return e.turnStats
	}
	stats := &types.ContextTurnStats{Source: types.ContextTurnStatsSourceAgent}
	if e.chatModel != nil {
		stats.Model = e.chatModel.GetModelName()
	}
	// compactor 为 nil（未配置窗口）时 Settings() 返回零值，于是窗口口径全为 0。
	if settings := e.compactor.Settings(); settings.MaxContextTokens > 0 {
		stats.Window = settings.MaxContextTokens
		stats.ReservedOutput = settings.ReserveTokens
		stats.Usable = settings.MaxContextTokens - settings.ReserveTokens
		stats.CompactThreshold = settings.Threshold()
	}
	e.turnStats = stats
	return stats
}

// recordContextEvent 记录一次压缩/截断事件。round 是 Agent 轮次（从 1 开始），
// 五层链路没有轮次概念，因此该字段是 Agent 归因独有的定位信息。
func (e *AgentEngine) recordContextEvent(round int, ev types.ContextTurnEvent) {
	if e == nil {
		return
	}
	ev.Round = round
	e.turnContextStats().RecordEvent(ev)
}

// turnUsagePayload 返回本轮累计用量的副本；一次都没有回报时返回 nil，
// 于是消息行上的 usage 存 NULL 而不是一份全零的假计量。
func (e *AgentEngine) turnUsagePayload() *types.TokenUsage {
	if e.turnUsage.IsZero() {
		return nil
	}
	usage := e.turnUsage
	return &usage
}

// turnStatsPayload 收尾本轮归因：补上往返数，没有任何可展示内容时返回 nil
// （ContextTurnStats.Value 对空值同样存 NULL）。
func (e *AgentEngine) turnStatsPayload() *types.ContextTurnStats {
	// 即使本轮没有触发任何压缩/截断也要建出骨架：窗口口径与模型往返数本身就是
	// 看板要的归因，而且压缩触发频率以"有归因的轮次"为分母，只在压缩时才有归因
	// 会让那个比率恒等于 1。
	stats := e.turnContextStats()
	stats.Rounds = e.turnRounds
	if stats.Empty() {
		return nil
	}
	return stats
}

// turnAccountingExtra 把本轮的用量与归因挂到事件 Extra 上（PRD FR-12）。
// 形状由 types.NewTurnUsageExtra 统一，chat pipeline 那条通道用的是同一个。
func (e *AgentEngine) turnAccountingExtra() map[string]interface{} {
	return types.NewTurnUsageExtra(e.turnUsagePayload(), e.turnStatsPayload())
}

// Execute executes the agent with conversation history and streaming output
// All events are emitted to EventBus and handled by subscribers (like Handler layer)
func (e *AgentEngine) Execute(
	ctx context.Context,
	sessionID, messageID, query string,
	llmContext []chat.Message,
	imageURLs ...[]string,
) (*types.AgentState, error) {
	logger.Infof(ctx, "[Agent] Starting execution: session=%s, message=%s, query_len=%d, context_msgs=%d",
		sessionID, messageID, len(query), len(llmContext))
	// Ensure tools are cleaned up after execution
	defer e.toolRegistry.Cleanup(ctx)

	common.PipelineInfo(ctx, "Agent", "execute_start", map[string]interface{}{
		"session_id":   sessionID,
		"message_id":   messageID,
		"query":        query,
		"context_msgs": len(llmContext),
	})

	// Open a top-level Langfuse span so the agent run — including every
	// round's LLM call and every tool execution — groups under a single
	// node in the Langfuse UI instead of being flat children of the HTTP
	// trace. No-op when Langfuse is disabled.
	imgCount := 0
	if len(imageURLs) > 0 {
		imgCount = len(imageURLs[0])
	}
	kbIDs := make([]string, 0, len(e.knowledgeBasesInfo))
	for _, kb := range e.knowledgeBasesInfo {
		if kb != nil {
			kbIDs = append(kbIDs, kb.ID)
		}
	}
	spanCtx, agentSpan := langfuse.GetManager().StartSpan(ctx, langfuse.SpanOptions{
		Name: "agent.execute",
		Input: map[string]interface{}{
			"query":        truncateRunes(query, langfuseQueryPreview),
			"query_len":    len(query),
			"context_msgs": len(llmContext),
			"image_count":  imgCount,
		},
		Metadata: map[string]interface{}{
			"session_id":          sessionID,
			"message_id":          messageID,
			"max_iterations":      e.config.MaxIterations,
			"parallel_tool_calls": e.config.ParallelToolCalls,
			"web_search":          e.config.WebSearchEnabled,
			"multi_turn":          e.config.MultiTurnEnabled,
			"knowledge_base_ids":  kbIDs,
			"allowed_tools":       e.config.AllowedTools,
		},
	})
	ctx = spanCtx

	// Initialize state
	state := &types.AgentState{
		RoundSteps:    []types.AgentStep{},
		KnowledgeRefs: []*types.SearchResult{},
		IsComplete:    false,
		CurrentRound:  0,
	}

	// 每个 turn 各复位一次：溢出恢复的「压缩后重试」上限是每 turn 一次（PRD FR-06 规则 4），
	// 压缩耗尽标记描述的也只是上一 turn 的历史规模。
	e.overflowRecovered = false
	e.compactionExhaustedAt = 0
	// 用量与上下文归因同样是每 turn 一份（PRD FR-12）。
	e.resetTurnAccounting()

	// Build system prompt using progressive RAG prompt
	// If skills are enabled, include skills metadata (Level 1 - Progressive Disclosure)
	systemPrompt := e.buildSystemPrompt(ctx)
	logger.Debugf(ctx, "[Agent] SystemPrompt: %d chars", len(systemPrompt))

	// Initialize messages with history
	var imgs []string
	if len(imageURLs) > 0 {
		imgs = imageURLs[0]
	}
	messages := e.buildMessagesWithLLMContext(systemPrompt, query, sessionID, llmContext, imgs)
	// FR-10：把装载侧给出的轮次身份平移到引擎消息坐标，压缩才能把摘要存到正确的轮次上。
	e.attachHistoryView(ctx, messages, llmContext)

	// Get tool definitions for function calling
	tools := e.buildToolsForLLM()
	toolListStr := strings.Join(listToolNames(tools), ", ")
	logger.Infof(ctx, "[Agent] Ready: %d messages, %d tools [%s], %d images",
		len(messages), len(tools), toolListStr, len(imgs))
	common.PipelineInfo(ctx, "Agent", "tools_ready", map[string]interface{}{
		"session_id": sessionID,
		"tool_count": len(tools),
		"tools":      toolListStr,
	})

	_, err := e.executeLoop(ctx, state, query, messages, tools, sessionID, messageID)
	if err != nil {
		logger.Errorf(ctx, "[Agent] Execution failed: %v", err)
		e.eventBus.Emit(ctx, event.Event{
			ID:        generateEventID("error"),
			Type:      event.EventError,
			SessionID: sessionID,
			Data: event.ErrorData{
				Error:     err.Error(),
				Stage:     "agent_execution",
				SessionID: sessionID,
			},
		})
		finishAgentSpan(agentSpan, state, err)
		return nil, err
	}

	logger.Infof(ctx, "[Agent] Completed: %d rounds, %d steps, complete=%v",
		state.CurrentRound, len(state.RoundSteps), state.IsComplete)
	common.PipelineInfo(ctx, "Agent", "execute_complete", map[string]interface{}{
		"session_id": sessionID,
		"rounds":     state.CurrentRound,
		"steps":      len(state.RoundSteps),
		"complete":   state.IsComplete,
	})
	finishAgentSpan(agentSpan, state, nil)
	return state, nil
}

// finishAgentSpan records the final outcome of an agent execution onto the
// top-level Langfuse span. Extracted so the same payload is used for both
// success and error return paths in Execute().
func finishAgentSpan(span *langfuse.Span, state *types.AgentState, err error) {
	if span == nil {
		return
	}
	totalToolCalls := 0
	for _, step := range state.RoundSteps {
		totalToolCalls += len(step.ToolCalls)
	}
	output := map[string]interface{}{
		"rounds":           state.CurrentRound,
		"steps":            len(state.RoundSteps),
		"tool_calls":       totalToolCalls,
		"complete":         state.IsComplete,
		"final_answer_len": len(state.FinalAnswer),
		"final_answer":     truncateRunes(state.FinalAnswer, langfuseQueryPreview),
	}
	span.Finish(output, map[string]interface{}{
		"rounds":     state.CurrentRound,
		"steps":      len(state.RoundSteps),
		"tool_calls": totalToolCalls,
		"complete":   state.IsComplete,
	}, err)
}

// truncateRunes caps s to n runes and appends "…" when truncated. Identical
// in spirit to the helper in act.go, but kept locally so both files stay
// independent and the truncation budget can diverge if needed.
func truncateRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// executeLoop executes the main ReAct loop
// All events are emitted through EventBus with the given sessionID
func (e *AgentEngine) executeLoop(
	ctx context.Context,
	state *types.AgentState,
	query string,
	messages []chat.Message,
	tools []chat.Tool,
	sessionID string,
	messageID string,
) (*types.AgentState, error) {
	startTime := time.Now()
	common.PipelineInfo(ctx, "Agent", "loop_start", map[string]interface{}{
		"max_iterations": e.config.MaxIterations,
	})

	// Guarantee exactly-one EventAgentComplete emission on every exit path
	// (normal finish, ctx cancel observed at the loop head, or iteration error
	// bubbling up while ctx was cancelled). The emission triggers
	// agent_stream_handler.handleComplete, which is where state.RoundSteps
	// (thinking + tool_call history) gets written onto assistantMessage.AgentSteps
	// so it can be persisted by the caller's defer. Use WithoutCancel so a
	// cancelled ctx does not short-circuit the emit on the stop path.
	completionEmitted := false
	emitCompletion := func() {
		if completionEmitted {
			return
		}
		completionEmitted = true
		e.emitCompletionEvent(context.WithoutCancel(ctx), state, sessionID, messageID, startTime)
	}
	defer emitCompletion()

	emptyRetries := 0
	consecutiveSameContent := 0
	lastResponseContent := ""
loop:
	for state.CurrentRound < e.config.MaxIterations {
		// Check for context cancellation (request timeout, user cancel, etc.)
		select {
		case <-ctx.Done():
			logger.Warnf(ctx, "[Agent] Context cancelled at round %d: %v",
				state.CurrentRound+1, ctx.Err())
			// Try to salvage existing results
			if totalTC := countTotalToolCalls(state.RoundSteps); totalTC > 0 {
				logger.Infof(ctx, "[Agent] Synthesizing final answer from %d existing tool results",
					totalTC)
				_ = e.streamFinalAnswerToEventBus(ctx, query, state, sessionID)
				state.IsComplete = true
			}
			return state, ctx.Err()
		default:
		}

		// Each iteration runs inside an "agent.round.<N>" Langfuse span.
		// We execute the body in a closure so `defer span.Finish()` fires at
		// every exit path (break/continue/next) without having to sprinkle
		// manual finish calls throughout the many branches below.
		outcome, iterErr := e.runReActIteration(ctx, state, &messages, tools,
			sessionID, messageID, query, &emptyRetries, &consecutiveSameContent, &lastResponseContent)
		if iterErr != nil {
			return state, iterErr
		}
		switch outcome {
		case iterOutcomeContinue:
			continue loop
		case iterOutcomeBreak:
			break loop
		case iterOutcomeNext:
			state.CurrentRound++
		}
	}

	// If loop finished without final answer, generate one — but skip this
	// when the context was cancelled (user pressed stop). In that case the
	// fallback LLM call would fail on the already-cancelled ctx and set
	// state.FinalAnswer to the generic "Sorry, I was unable to generate a
	// complete answer." message, which then leaks to the UI as the final
	// answer for a conversation the user deliberately stopped.
	if !state.IsComplete && ctx.Err() == nil {
		e.handleMaxIterations(ctx, query, state, sessionID)
	}

	return state, nil
}

// iterOutcome directs executeLoop's control flow after one ReAct iteration.
// Using a sentinel (rather than bare return values from runReActIteration)
// keeps the loop's break/continue/next branches explicit in one place.
type iterOutcome int

const (
	// iterOutcomeNext advances state.CurrentRound and loops again.
	iterOutcomeNext iterOutcome = iota
	// iterOutcomeContinue re-runs the loop without advancing the round
	// counter. Used by the empty-content retry path.
	iterOutcomeContinue
	// iterOutcomeBreak exits the loop (final answer, stuck loop, or end).
	iterOutcomeBreak
)

// runReActIteration executes one ReAct step: think → analyze → act → observe.
// Extracted from executeLoop so the whole iteration body can live inside a
// single `defer span.Finish()` scope — otherwise we'd need to sprinkle
// manual finish calls across every break/continue/return branch.
//
// The mutable loop state (messages, empty-retry counter, stuck-loop detector)
// is passed by pointer so iterations share progress.
func (e *AgentEngine) runReActIteration(
	parentCtx context.Context,
	state *types.AgentState,
	messagesPtr *[]chat.Message,
	tools []chat.Tool,
	sessionID, assistantMessageID, query string,
	emptyRetries, consecutiveSameContent *int,
	lastResponseContent *string,
) (outcome iterOutcome, retErr error) {
	roundStart := time.Now()
	round := state.CurrentRound + 1

	// Open the round-level Langfuse span. Any chat/tool calls made inside
	// this iteration will attach under it via ctx, giving the UI a clean
	// trace → agent.execute → agent.round.N → (chat + tools) structure.
	ctx, roundSpan := langfuse.GetManager().StartSpan(parentCtx, langfuse.SpanOptions{
		Name: fmt.Sprintf("agent.round.%d", round),
		Input: map[string]interface{}{
			"round":          round,
			"message_count":  len(*messagesPtr),
			"max_iterations": e.config.MaxIterations,
		},
		Metadata: map[string]interface{}{
			"iteration":  state.CurrentRound,
			"round":      round,
			"session_id": sessionID,
		},
	})

	var (
		response      *types.ChatResponse
		toolCallCount int
	)
	defer func() {
		if roundSpan == nil {
			return
		}
		out := map[string]interface{}{
			"round":      round,
			"outcome":    outcome.String(),
			"tool_calls": toolCallCount,
		}
		if response != nil {
			out["has_tool_calls"] = len(response.ToolCalls) > 0
			out["finish_reason"] = response.FinishReason
			out["content_len"] = len(response.Content)
			if response.Usage.TotalTokens > 0 {
				out["prompt_tokens"] = response.Usage.PromptTokens
				out["completion_tokens"] = response.Usage.CompletionTokens
				out["total_tokens"] = response.Usage.TotalTokens
			}
		}
		out["duration_ms"] = time.Since(roundStart).Milliseconds()
		roundSpan.Finish(out, map[string]interface{}{
			"round":       round,
			"tool_calls":  toolCallCount,
			"outcome":     outcome.String(),
			"duration_ms": time.Since(roundStart).Milliseconds(),
		}, retErr)
	}()

	// 上下文窗口管理（PRD FR-06）：用上一轮 API 报告的 usage 加上对新追加消息
	// （assistant 回复 + 工具结果）的精确计量作为当前规模，越阈则先压缩再发请求。
	currentTokens := e.estimateCurrentTokens(*messagesPtr)
	if managed, changed := e.manageContextWindow(ctx, *messagesPtr, round, currentTokens); changed {
		*messagesPtr = managed
		currentTokens = e.countContextTokens(managed)
	}

	logger.Infof(ctx, "[Agent][Round-%d/%d] Starting: %d messages, %d tools, est_tokens=%d",
		round, e.config.MaxIterations, len(*messagesPtr), len(tools), currentTokens)
	common.PipelineInfo(ctx, "Agent", "round_start", map[string]interface{}{
		"iteration":      state.CurrentRound,
		"round":          round,
		"message_count":  len(*messagesPtr),
		"pending_tools":  len(tools),
		"max_iterations": e.config.MaxIterations,
	})

	// 1. Think: Call LLM with function calling (includes retry + graceful degradation)
	e.lastSentMsgCount = len(*messagesPtr)
	resp, err := e.callLLMWithRetry(ctx, messagesPtr, tools, state, query, state.CurrentRound, sessionID)
	if err != nil {
		retErr = err
		return iterOutcomeNext, err
	}
	if resp == nil {
		return iterOutcomeBreak, nil
	}

	// 本轮自己的 token 估算可能是错的——历史是计量出来的，而供应商按自己的方式计数——
	// 于是一个看起来安全的请求仍可能回来时已经撞上了窗口。压缩后重试一次，把这一轮从
	// 浪费变成恢复。每 turn 只此一次：如果重试仍然溢出，问题就不在历史体积上
	// （PRD FR-06 规则 1、规则 4）。
	if !e.overflowRecovered && e.responseHitContextLimit(resp) {
		e.overflowRecovered = true
		logger.Warnf(ctx, "[Agent][Round-%d] 响应撞上上下文窗口 (finish=%s, prompt=%d, completion=%d)；"+
			"压缩后重试一次", round, resp.FinishReason, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		common.PipelineWarn(ctx, "Agent", "context_overflow_retry", map[string]interface{}{
			"round":         round,
			"finish_reason": resp.FinishReason,
			"prompt_tokens": resp.Usage.PromptTokens,
			"message_count": len(*messagesPtr),
		})
		*messagesPtr = e.forceCompaction(ctx, *messagesPtr, round)
		e.lastSentMsgCount = len(*messagesPtr)
		resp, err = e.callLLMWithRetry(ctx, messagesPtr, tools, state, query, state.CurrentRound, sessionID)
		if err != nil {
			retErr = err
			return iterOutcomeNext, err
		}
		if resp == nil {
			return iterOutcomeBreak, nil
		}
	}

	response = resp
	if response.Usage.TotalTokens > 0 {
		e.lastUsage = response.Usage
		logger.Debugf(ctx, "[Agent][Round-%d] Usage: prompt=%d, completion=%d, total=%d",
			round, response.Usage.PromptTokens,
			response.Usage.CompletionTokens, response.Usage.TotalTokens)
	}

	// Detect stuck loops: if the LLM keeps returning the same content
	// without tool calls (e.g., an unhandled finish reason), break early.
	if len(response.ToolCalls) == 0 && response.Content != "" {
		if response.Content == *lastResponseContent {
			*consecutiveSameContent++
		} else {
			*consecutiveSameContent = 0
		}
		*lastResponseContent = response.Content
		if *consecutiveSameContent >= maxRepeatedResponseRounds {
			logger.Warnf(ctx, "[Agent][Round-%d] Detected stuck loop: same content repeated %d times (finish=%s), stopping",
				round, *consecutiveSameContent+1, response.FinishReason)
			state.FinalAnswer = response.Content
			state.IsComplete = true
			return iterOutcomeBreak, nil
		}
	} else {
		*consecutiveSameContent = 0
		*lastResponseContent = ""
	}

	// Create agent step
	step := types.AgentStep{
		Iteration:        state.CurrentRound,
		Thought:          response.Content,
		ReasoningContent: response.ReasoningContent,
		ToolCalls:        make([]types.ToolCall, 0),
		Timestamp:        time.Now(),
	}

	// If the request was cancelled while the LLM was streaming (e.g. the
	// user pressed "stop"), the stream driver still returns a usable
	// response (partial content / finish_reason="stop" / no tool calls).
	// Do NOT let analyzeResponse treat that partial thinking text as the
	// final answer — it would pollute Message.Content with mid-stream
	// thinking and show up as a duplicate card next to the intermediate-
	// steps tree. Preserve the partial thinking as an AgentStep and break
	// out of the loop without marking state.IsComplete. executeLoop's
	// deferred emitCompletionEvent will persist the step onto
	// Message.AgentSteps so it still appears in the tree on refresh.
	if ctx.Err() != nil {
		logger.Warnf(ctx, "[Agent][Round-%d] Context cancelled during LLM call; preserving partial step",
			round)
		if step.Thought != "" || len(step.ToolCalls) > 0 {
			state.RoundSteps = append(state.RoundSteps, step)
		}
		return iterOutcomeBreak, nil
	}

	// 2. Analyze: Check for stop conditions (natural stop with no tool calls)
	verdict := e.analyzeResponse(ctx, response, step, state.CurrentRound, sessionID, roundStart)
	if verdict.isDone {
		// Guard against empty content: when the LLM stops naturally with no
		// content and no tool calls (e.g., thinking-only loop without KB),
		// retry with a nudge message instead of accepting an empty answer.
		if verdict.emptyContent {
			*emptyRetries++
			if *emptyRetries <= maxEmptyResponseRetries {
				logger.Warnf(ctx, "[Agent][Round-%d] Empty content with stop - retrying (%d/%d)",
					round, *emptyRetries, maxEmptyResponseRetries)
				*messagesPtr = append(*messagesPtr, chat.Message{
					Role:    "user",
					Content: "Please provide your complete answer now as plain text.",
				})
				return iterOutcomeContinue, nil
			}
			// Retries exhausted — use fallback message rather than empty answer
			logger.Warnf(ctx, "[Agent][Round-%d] Empty content after %d retries - using fallback",
				round, maxEmptyResponseRetries)
			state.FinalAnswer = "I'm sorry, I was unable to generate a response. Please try again."
			state.IsComplete = true
			state.RoundSteps = append(state.RoundSteps, verdict.step)
			return iterOutcomeBreak, nil
		}
		state.FinalAnswer = verdict.finalAnswer
		state.IsComplete = true
		state.RoundSteps = append(state.RoundSteps, verdict.step)
		return iterOutcomeBreak, nil
	}

	// This round is non-terminal (it will execute tools and loop again). Any
	// plain assistant text streamed live to the answer area this round was a
	// preamble (e.g. "let me search the knowledge base…"), not the final
	// answer. No explicit retraction signal is emitted: the agent only ends by
	// stopping naturally with plain text and no tool calls, so the upcoming
	// tool-call events are themselves the authoritative "that wasn't the final
	// answer" marker. Both the stream handler and the UI treat any answer text
	// preceding a tool call in the same stream as a preamble and relocate it
	// into the steps tree. The preamble is still preserved as this round's
	// Thought.

	// 3. Act: Execute tool calls
	e.executeToolCalls(ctx, response, &step, state.CurrentRound, sessionID, assistantMessageID)
	toolCallCount = len(step.ToolCalls)

	// 4. Observe: Add tool results to messages and write to context
	state.RoundSteps = append(state.RoundSteps, step)
	*messagesPtr = e.appendToolResults(*messagesPtr, step)
	// Context governance (ima-grade P1): JSON-normalize and inner-summarize
	// oversized fresh tool results before they enter the window, then
	// checkpoint-compress the scratchpad on the configured cadence.
	if toolCallCount > 0 {
		*messagesPtr = e.governToolResults(ctx, *messagesPtr,
			len(*messagesPtr)-toolCallCount, query)
	}
	messagesBeforeScratchpad := len(*messagesPtr)
	*messagesPtr = e.maybeCompressScratchpad(ctx, *messagesPtr, query,
		countTotalToolCalls(state.RoundSteps))
	if len(*messagesPtr) != messagesBeforeScratchpad {
		// 草稿压缩把较早的工具结果折叠成一条摘要，消息下标整体前移：轮次身份视图
		// 不再可信，本轮不再产生压缩检查点（PRD FR-10）。
		e.invalidateHistoryView()
	}
	common.PipelineInfo(ctx, "Agent", "round_end", map[string]interface{}{
		"iteration":   state.CurrentRound,
		"round":       round,
		"tool_calls":  toolCallCount,
		"thought_len": len(step.Thought),
	})

	return iterOutcomeNext, nil
}

// String returns a stable label for Langfuse output payloads.
func (o iterOutcome) String() string {
	switch o {
	case iterOutcomeNext:
		return "next"
	case iterOutcomeContinue:
		return "continue"
	case iterOutcomeBreak:
		return "break"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Tool result image VLM description helpers
// ---------------------------------------------------------------------------

const toolImageAnalysisPrompt = "Describe the content of this image in detail. " +
	"If it contains text, extract all readable text. " +
	"If it contains charts or diagrams, describe the data and structure."

// describeImages generates text descriptions for tool result images using the
// configured imageDescriber (VLM). Each image is decoded from a data URI and
// analyzed independently. Failures are logged and skipped gracefully.
// This follows the same pattern as Handler.analyzeImageAttachments().
func (e *AgentEngine) describeImages(ctx context.Context, imageDataURIs []string) []string {
	if e.imageDescriber == nil {
		return nil
	}
	var descriptions []string
	for i, dataURI := range imageDataURIs {
		if ctx.Err() != nil {
			logger.Warnf(ctx, "[Agent] Context cancelled, skipping remaining %d tool result images", len(imageDataURIs)-i)
			break
		}
		imgBytes, err := decodeDataURIBytes(dataURI)
		if err != nil {
			logger.Warnf(ctx, "[Agent] Failed to decode tool result image %d: %v", i, err)
			continue
		}
		desc, err := e.imageDescriber(ctx, imgBytes, toolImageAnalysisPrompt)
		if err != nil {
			logger.Warnf(ctx, "[Agent] VLM analysis failed for tool result image %d: %v", i, err)
			continue
		}
		descriptions = append(descriptions, strings.TrimSpace(desc))
	}
	return descriptions
}

// decodeDataURIBytes extracts raw bytes from a "data:mime;base64,..." URI.
// Retries with RawStdEncoding when standard base64 decoding fails (some MCP
// servers omit trailing '=' padding).
func decodeDataURIBytes(dataURI string) ([]byte, error) {
	if !strings.HasPrefix(dataURI, "data:") {
		return nil, fmt.Errorf("not a data URI")
	}
	idx := strings.Index(dataURI, ";base64,")
	if idx < 0 {
		return nil, fmt.Errorf("unsupported data URI encoding (expected base64)")
	}
	raw := dataURI[idx+8:]
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Retry without padding — some MCP servers omit trailing '='
		decoded, err = base64.RawStdEncoding.DecodeString(raw)
	}
	return decoded, err
}
