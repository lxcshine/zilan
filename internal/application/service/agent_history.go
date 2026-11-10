package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// agentHistoryFetchMultiplier controls how many raw DB messages to fetch
// when assembling history. Each turn contributes ~2 rows (user + assistant);
// we ask for a generous multiple so we never under-fetch when some pairs are
// incomplete (e.g. an in-flight turn).
const agentHistoryFetchMultiplier = 4

// agentHistoryFetchMin is the floor for the DB fetch limit, used when
// maxRounds is small or unset.
const agentHistoryFetchMin = 50

var agentHistoryThinkTagRegex = regexp.MustCompile(`(?s)<think>.*?</think>`)

// AgentHistory 是一次 Agent 历史装载的结果（PRD FR-10）。
type AgentHistory struct {
	// Messages 按时间序排列，可直接作为 llmContext 交给引擎。复用了压缩检查点时，
	// 第一条是摘要占位消息，它替代了被覆盖的那些轮次。
	Messages []chat.Message
	// Summary 是复用的持久化摘要；未复用检查点时为空。
	Summary string
	// Reused 报告本轮是否真的复用了检查点。false 意味着摘要要重新算一遍。
	Reused bool
	// CheckpointMessageID 是被复用检查点所在的 assistant 行 ID（诊断用）。
	CheckpointMessageID string
	// View 是轮次身份视图，区间坐标与 Messages 一致。交给引擎后，压缩才能判断被摘要
	// 区间是否正好结束在某个已持久化轮次上，并把新摘要存到那一轮。
	View *compaction.HistoryView
}

// LoadAgentHistory rebuilds the multi-turn LLM context for an Agent-mode
// session directly from the persistent messages table. The result is a
// chronologically ordered list of chat.Message entries suitable for prepending
// to the current turn (without system prompt; the engine adds that itself).
//
// For each historical turn it emits:
//  1. A user message (RenderedContent if present, else Content, plus any
//     image captions appended).
//  2. For each AgentStep with non-terminal tool calls (i.e. excluding
//     final_answer), an assistant message carrying the step's thought and
//     tool_calls, followed by one tool message per tool result.
//  3. A final assistant message with the canonical answer (msg.Content with
//     <think> blocks stripped).
//
// Turns lacking either user or assistant content are skipped. The newest
// maxRounds turns are returned in chronological order.
//
// 压缩检查点（PRD FR-10）：会话里有可用检查点时，历史从检查点之后起读——检查点覆盖的
// 那些轮次由一条摘要占位消息替代，不再逐轮重放，也不再重付摘要模型调用。复用要求前缀
// 严格一致：装载时按持久化行重算 PrefixHash，与被覆盖区间内任何一条消息的改动（编辑、
// 撤回、重新生成）都会让校验失败，此时丢弃检查点、退回全量装载并记录诊断。
//
// DB is treated as the single source of truth — there is no Redis/in-memory
// cache layer above this function. Callers are expected to invoke it once
// per turn before handing the messages to the agent engine.
func LoadAgentHistory(
	ctx context.Context,
	messageRepo interfaces.MessageRepository,
	sessionID string,
	maxRounds int,
) (*AgentHistory, error) {
	empty := &AgentHistory{Messages: []chat.Message{}}
	if maxRounds <= 0 {
		return empty, nil
	}

	checkpoint := loadContextCheckpoint(ctx, messageRepo, sessionID)

	fetchLimit := maxRounds * agentHistoryFetchMultiplier
	if fetchLimit < agentHistoryFetchMin {
		fetchLimit = agentHistoryFetchMin
	}
	if checkpoint != nil {
		// 校验前缀要回读被覆盖的那些轮次，而它们不在重放窗口内。
		fetchLimit += checkpoint.ContextCheckpoint.CoveredTurns*2 + agentHistoryFetchMin
	}

	rows, err := messageRepo.GetRecentMessagesBySession(ctx, sessionID, fetchLimit)
	if err != nil {
		return nil, fmt.Errorf("load agent history: %w", err)
	}
	if len(rows) == 0 {
		return empty, nil
	}

	turns := groupHistoryTurns(rows)
	if checkpoint != nil {
		if loaded, ok := historyFromCheckpoint(ctx, sessionID, checkpoint, turns, maxRounds); ok {
			return loaded, nil
		}
	}
	return historyFromScratch(turns, maxRounds), nil
}

// agentHistoryTurn 是一个已持久化的完整轮次：一行 user 加一行已完成的 assistant。
type agentHistoryTurn struct {
	requestID string
	user      *types.Message
	assistant *types.Message
	createdAt time.Time
}

// storedMessages 返回该轮参与前缀哈希的持久化消息（时间序）。
//
// 只取参与重放的那两行的原始 Content：保存检查点与校验检查点因此看到同一组输入，
// 而重放时的加工（图片说明、附件、剥离 <think>）不会污染哈希。刻意不含时间戳与
// token 计数——换 tokenizer 或时钟漂移不该让检查点失效（PRD FR-10 规则 3）。
func (t *agentHistoryTurn) storedMessages() []types.StoredMessage {
	out := make([]types.StoredMessage, 0, 2)
	if t.user != nil {
		out = append(out, types.StoredMessage{TurnID: t.requestID, Role: t.user.Role, Content: t.user.Content})
	}
	if t.assistant != nil {
		out = append(out, types.StoredMessage{TurnID: t.requestID, Role: t.assistant.Role, Content: t.assistant.Content})
	}
	return out
}

// groupHistoryTurns 按 RequestID 把消息行配对成轮次，只保留 user 与已完成 assistant
// 都在的轮次，并按时间升序返回。
func groupHistoryTurns(rows []*types.Message) []*agentHistoryTurn {
	index := make(map[string]*agentHistoryTurn, len(rows))
	order := make([]string, 0, len(rows))
	for _, msg := range rows {
		t, ok := index[msg.RequestID]
		if !ok {
			t = &agentHistoryTurn{requestID: msg.RequestID}
			index[msg.RequestID] = t
			order = append(order, msg.RequestID)
		}
		switch msg.Role {
		case "user":
			t.user = msg
			if t.createdAt.IsZero() || msg.CreatedAt.Before(t.createdAt) {
				t.createdAt = msg.CreatedAt
			}
		case "assistant":
			t.assistant = msg
		}
	}

	turns := make([]*agentHistoryTurn, 0, len(order))
	for _, requestID := range order {
		t := index[requestID]
		if t.user != nil && t.assistant != nil && t.assistant.IsCompleted {
			turns = append(turns, t)
		}
	}
	sort.Slice(turns, func(i, j int) bool {
		return turns[i].createdAt.Before(turns[j].createdAt)
	})
	return turns
}

// historyFromCheckpoint 尝试复用检查点装载历史。ok=false 表示检查点不可用（边界轮次
// 没被读到，或前缀重算不一致），调用方必须退回全量装载。
func historyFromCheckpoint(
	ctx context.Context, sessionID string, checkpoint *types.Message, turns []*agentHistoryTurn, maxRounds int,
) (*AgentHistory, bool) {
	cp := checkpoint.ContextCheckpoint
	cpIdx := indexByAssistantID(turns, checkpoint.ID)
	if cpIdx < 0 {
		discardCheckpoint(ctx, sessionID, checkpoint, "covered_turn_not_loaded")
		return nil, false
	}
	anchorIdx := indexByRequestID(turns, cp.AnchorTurnID)
	if anchorIdx < 0 || anchorIdx > cpIdx {
		discardCheckpoint(ctx, sessionID, checkpoint, "anchor_turn_not_loaded")
		return nil, false
	}
	// 规则 3：前缀严格一致。重算 [anchor, covered] 的链式哈希，逐字节相等才复用。
	if got := foldTurnChain(cp, turns[anchorIdx:cpIdx+1]); got != cp.PrefixHash {
		discardCheckpoint(ctx, sessionID, checkpoint, "prefix_hash_mismatch")
		return nil, false
	}

	replayed := turns[cpIdx+1:]
	if len(replayed) > maxRounds {
		// 摘要与保留轮次之间出现空洞：正常压缩不会造成这种情况，它需要连续多轮
		// 检查点写入失败。记下诊断，本轮带着不完整的中间历史继续。
		logger.Warnf(ctx, "会话 %s 检查点 %s 之后仍有 %d 轮，超出窗口 %d，较早的轮次被丢弃",
			sessionID, checkpoint.ID, len(replayed), maxRounds)
		replayed = replayed[len(replayed)-maxRounds:]
	}

	out := []chat.Message{compaction.SummaryMessage(cp.Summary)}
	view := compaction.NewHistoryView(cp.AnchorTurnID, cp.BaseHash, cp.PrefixHash, cp.CoveredTurns)
	out = appendHistoryTurns(out, view, replayed)

	logger.Infof(ctx, "会话 %s 复用上下文压缩检查点 %s：覆盖 %d 轮，摘要 %d 字符，重放 %d 轮",
		sessionID, checkpoint.ID, cp.CoveredTurns, len(cp.Summary), len(replayed))
	return &AgentHistory{
		Messages:            out,
		Summary:             cp.Summary,
		Reused:              true,
		CheckpointMessageID: checkpoint.ID,
		View:                view,
	}, true
}

// historyFromScratch 是没有任何可用检查点时的装载：重放最新的 maxRounds 轮，并把它们
// 作为新检查点的候选覆盖区间交给引擎。
func historyFromScratch(turns []*agentHistoryTurn, maxRounds int) *AgentHistory {
	if len(turns) > maxRounds {
		turns = turns[len(turns)-maxRounds:]
	}
	out := make([]chat.Message, 0, len(turns)*4)
	var view *compaction.HistoryView
	if len(turns) > 0 {
		view = compaction.NewHistoryView(turns[0].requestID, "", "", 0)
	}
	out = appendHistoryTurns(out, view, turns)
	return &AgentHistory{Messages: out, View: view}
}

// appendHistoryTurns 逐轮重放消息，并把每轮的区间与链式前缀哈希登记进视图。
func appendHistoryTurns(
	out []chat.Message, view *compaction.HistoryView, turns []*agentHistoryTurn,
) []chat.Message {
	for _, t := range turns {
		start := len(out)
		out = append(out, buildUserHistoryMessage(t.user))
		out = append(out, buildAssistantHistoryMessages(t.assistant)...)
		view.AppendStoredTurn(t.requestID, t.assistant.ID, t.storedMessages(), start, len(out))
	}
	return out
}

// foldTurnChain 按持久化行重算一段轮次区间的链式前缀哈希。
func foldTurnChain(cp *types.ContextCheckpoint, turns []*agentHistoryTurn) string {
	stored := make([][]types.StoredMessage, 0, len(turns))
	for _, t := range turns {
		stored = append(stored, t.storedMessages())
	}
	return compaction.FoldStoredTurns(
		types.CheckpointChainSeed(compaction.CheckpointPromptVersion, cp.BaseHash, cp.AnchorTurnID), stored)
}

// discardCheckpoint 记录一次检查点失效（PRD FR-10 规则 3）。失效的代价只是本轮全量
// 重算，因此任何校验失败都只诊断、不中断装载。
func discardCheckpoint(ctx context.Context, sessionID string, checkpoint *types.Message, reason string) {
	cp := checkpoint.ContextCheckpoint
	logger.Warnf(ctx, "会话 %s 的上下文压缩检查点 %s 失效（%s），本轮全量重算历史摘要",
		sessionID, checkpoint.ID, reason)
	common.PipelineWarn(ctx, "AgentHistory", "context_checkpoint_invalidated", map[string]interface{}{
		"session_id":    sessionID,
		"message_id":    checkpoint.ID,
		"reason":        reason,
		"source":        cp.Source,
		"covered_turns": cp.CoveredTurns,
		"degraded":      cp.Degraded,
	})
}

// loadContextCheckpoint 返回会话中最新的一份可用检查点，没有则返回 nil。
// 读取失败退化为不带检查点装载：本轮照跑，只是再付一次压缩。
func loadContextCheckpoint(
	ctx context.Context, messageRepo interfaces.MessageRepository, sessionID string,
) *types.Message {
	msg, err := messageRepo.GetLatestContextCheckpoint(ctx, sessionID)
	if err != nil {
		logger.Warnf(ctx, "读取会话 %s 的上下文压缩检查点失败，本轮不带检查点装载历史: %v", sessionID, err)
		return nil
	}
	if msg == nil || !msg.ContextCheckpoint.Usable() {
		return nil
	}
	return msg
}

func indexByAssistantID(turns []*agentHistoryTurn, assistantID string) int {
	if assistantID == "" {
		return -1
	}
	for i, t := range turns {
		if t.assistant != nil && t.assistant.ID == assistantID {
			return i
		}
	}
	return -1
}

func indexByRequestID(turns []*agentHistoryTurn, requestID string) int {
	if requestID == "" {
		return -1
	}
	for i, t := range turns {
		if t.requestID == requestID {
			return i
		}
	}
	return -1
}

// MessageCheckpointSink 把引擎产出的压缩检查点写到会话的 assistant 行上，
// LoadAgentHistory 在下一轮从同一处读回（PRD FR-10 规则 4）。
type MessageCheckpointSink struct {
	repo      interfaces.MessageRepository
	sessionID string
}

// NewMessageCheckpointSink 构造检查点写入通道。repo 为 nil 时返回 nil，
// 于是引擎侧「没有 sink」与「功能关闭」是同一条路径。
func NewMessageCheckpointSink(
	repo interfaces.MessageRepository, sessionID string,
) types.ContextCheckpointSink {
	if repo == nil {
		return nil
	}
	return MessageCheckpointSink{repo: repo, sessionID: sessionID}
}

func (s MessageCheckpointSink) SaveContextCheckpoint(
	ctx context.Context, turnMessageID string, checkpoint *types.ContextCheckpoint,
) error {
	return s.repo.UpdateMessageContextCheckpoint(ctx, s.sessionID, turnMessageID, checkpoint)
}

// buildUserHistoryMessage converts a stored user message into the chat.Message
// form that should appear in LLM history. It deliberately ignores
// RenderedContent: that field is a snapshot of the old prompt and retrieval
// context format, which must not be mixed into the current request protocol.
// Image captions and attachments are reconstructed from their canonical DB
// columns so useful user-provided context is retained without stale RAG data.
func buildUserHistoryMessage(m *types.Message) chat.Message {
	content := m.Content
	if captions := extractImageCaptionsFromMessage(m.Images); captions != "" {
		content += "\n\n[用户上传图片内容]\n" + captions
	}
	if len(m.Attachments) > 0 {
		content += m.Attachments.BuildPrompt()
	}
	return chat.Message{Role: "user", Content: content}
}

// buildAssistantHistoryMessages reconstructs the assistant side of one
// historical turn. It walks AgentSteps to expand intermediate tool calls into
// proper OpenAI-shaped assistant + tool messages, then emits the canonical
// final answer as a trailing assistant message.
//
// AgentSteps from KnowledgeQA-mode turns are empty, in which case the result
// is just the single final-answer assistant message — exactly mirroring how
// the KnowledgeQA pipeline replays history today.
func buildAssistantHistoryMessages(m *types.Message) []chat.Message {
	msgs := make([]chat.Message, 0, len(m.AgentSteps)*2+1)
	for _, step := range m.AgentSteps {
		nonTerminalCalls := filterNonTerminalToolCalls(step.ToolCalls)
		if len(nonTerminalCalls) == 0 {
			continue
		}
		assistantMsg := chat.Message{
			Role:             "assistant",
			Content:          step.Thought,
			ReasoningContent: step.ReasoningContent,
			ToolCalls:        make([]chat.ToolCall, 0, len(nonTerminalCalls)),
		}
		for _, tc := range nonTerminalCalls {
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
		msgs = append(msgs, assistantMsg)
		for _, tc := range nonTerminalCalls {
			msgs = append(msgs, chat.Message{
				Role:       "tool",
				Content:    toolCallOutput(tc),
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
		}
	}

	finalContent := agentHistoryThinkTagRegex.ReplaceAllString(m.Content, "")
	finalContent = strings.TrimSpace(finalContent)
	if finalContent != "" {
		msgs = append(msgs, chat.Message{Role: "assistant", Content: finalContent})
	}
	return msgs
}

// legacyFinalAnswerToolName is the name of the now-removed final_answer tool.
// It is retained here only to filter such calls out of OLD persisted agent
// histories: pre-existing conversations recorded a final_answer tool call as
// the terminal step, and the canonical answer text is replayed via the
// trailing assistant message instead. Re-injecting it would duplicate the
// answer or confuse the model into thinking the previous turn is mid-flight.
const legacyFinalAnswerToolName = "final_answer"

// filterNonTerminalToolCalls drops legacy final_answer entries from historical
// tool calls (see legacyFinalAnswerToolName). New turns never produce them.
func filterNonTerminalToolCalls(calls []types.ToolCall) []types.ToolCall {
	out := make([]types.ToolCall, 0, len(calls))
	for _, tc := range calls {
		if tc.Name == legacyFinalAnswerToolName {
			continue
		}
		out = append(out, tc)
	}
	return out
}

// toolCallOutput returns the textual content to use for a historical tool
// message: the recorded Output on success, or an "Error: …" line otherwise so
// the model can tell that an earlier tool call failed.
func toolCallOutput(tc types.ToolCall) string {
	if tc.Result == nil {
		return ""
	}
	if !tc.Result.Success {
		if tc.Result.Error != "" {
			return "Error: " + tc.Result.Error
		}
		return "Error: tool call failed"
	}
	return agenttools.CompactToolOutputForHistory(tc.Name, tc.Result)
}

// extractImageCaptionsFromMessage concatenates non-empty Caption fields from
// stored message images. Mirrors the helper used in chat_pipeline so both
// modes surface previous-turn image descriptions identically.
func extractImageCaptionsFromMessage(images types.MessageImages) string {
	var parts []string
	for _, img := range images {
		if img.Caption != "" {
			parts = append(parts, img.Caption)
		}
	}
	return strings.Join(parts, "\n")
}
