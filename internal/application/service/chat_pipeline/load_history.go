package chatpipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// maxStickyVerbatimBytes 是写进检查点的「原样保留轮次」块的字节上限。检查点声明覆盖了
// 旧区间的全部轮次，其中 sticky 轮次没有进入摘要，只能把原文附在摘要后面；上限保证
// 摘要不会反过来撑爆它要替代的那段历史。
const maxStickyVerbatimBytes = 6000

type PluginLoadHistory struct {
	messageService interfaces.MessageService
	modelService   interfaces.ModelService
	config         *config.Config
}

func NewPluginLoadHistory(eventManager *EventManager,
	messageService interfaces.MessageService,
	modelService interfaces.ModelService,
	config *config.Config,
) *PluginLoadHistory {
	res := &PluginLoadHistory{
		messageService: messageService,
		modelService:   modelService,
		config:         config,
	}
	eventManager.Register(res)
	return res
}

func (p *PluginLoadHistory) ActivationEvents() []types.EventType {
	return []types.EventType{types.LOAD_HISTORY}
}

func (p *PluginLoadHistory) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	// chatManage.MaxRounds == 0 means multi-turn is explicitly disabled
	// (e.g. by a custom agent with MultiTurnEnabled=false). Skip loading so
	// history doesn't leak into the LLM context. We do NOT fall back to the
	// global Conversation.MaxRounds default here, otherwise the disable flag
	// would be silently overridden.
	if chatManage.MaxRounds <= 0 {
		pipelineInfo(ctx, "LoadHistory", "skipped", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"reason":     "multi_turn_disabled",
		})
		return next()
	}
	maxRounds := chatManage.MaxRounds

	// Smart strategy (ima-grade): load a deeper window, Map-Reduce the old
	// rounds into a running summary, keep sticky + recent rounds verbatim.
	// The summary rides along in chatManage.HistorySummary and is prepended
	// to the history layer at assembly time, so compressed context keeps
	// conversation continuity.
	if smartCompressionEnabled(ctx, chatManage) {
		if p.loadSmart(ctx, chatManage, maxRounds) {
			return next()
		}
		// On any smart-path failure fall through to the sliding window so the
		// conversation never breaks because of compression.
	}

	pipelineInfo(ctx, "LoadHistory", "input", map[string]interface{}{
		"session_id": chatManage.SessionID,
		"max_rounds": maxRounds,
	})

	historyList, err := loadAndProcessHistory(ctx, p.messageService, chatManage.SessionID, maxRounds, maxRounds*2+10)
	if err != nil {
		pipelineWarn(ctx, "LoadHistory", "history_fetch", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"error":      err.Error(),
		})
		return next()
	}

	chatManage.History = historyList

	pipelineInfo(ctx, "LoadHistory", "output", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"history_rounds": len(historyList),
		"max_rounds":     maxRounds,
	})

	return next()
}

// smartTurn 是一个已持久化轮次在增量摘要缓存中的身份与文本（PRD FR-10）。
//
// 与 loadAndProcessHistory 的差别只在于它保住了轮次身份（RequestID）与检查点落点
// （assistant 消息 ID），以及参与前缀哈希的原始 Content——没有身份就无法判断
// "历史前缀是否与已有检查点严格一致"，也就无法安全地复用摘要。
type smartTurn struct {
	history            *types.History
	requestID          string
	assistantMessageID string
	userContent        string
	assistantContent   string
	createdAt          time.Time
}

// storedMessages 返回该轮参与前缀哈希的持久化消息（时间序）。与 Agent 侧
// service.LoadAgentHistory 完全同口径：只取原始 Content，不含时间戳与 token 计数，
// 因此两条路径写下的检查点可以互相校验。
func (t *smartTurn) storedMessages() []types.StoredMessage {
	out := make([]types.StoredMessage, 0, 2)
	if t.userContent != "" {
		out = append(out, types.StoredMessage{TurnID: t.requestID, Role: "user", Content: t.userContent})
	}
	if t.assistantMessageID != "" {
		out = append(out, types.StoredMessage{TurnID: t.requestID, Role: "assistant", Content: t.assistantContent})
	}
	return out
}

// loadSmart implements the smart history-compression branch:
//
//  1. Fetch up to SummarizeThreshold rounds (deeper than the sliding window).
//  2. 复用压缩检查点（PRD FR-10）：被检查点覆盖的旧轮次直接由持久化摘要替代，
//     只对检查点之后的新轮次做增量 Map-Reduce，不再每轮全量重算。
//  3. Map-Reduce: rounds older than the recent window are chunked and
//     summarized by a lightweight LLM call; partial summaries are merged into
//     one running summary (MaxSummaryTokens-capped).
//  4. Sticky rounds (decisions, deadlines, key numbers, explicit approval)
//     are exempt from compression and kept verbatim.
//  5. Alias compression replaces long entity names repeated across the kept
//     rounds with short codes; the legend rides at the top of the summary.
//  6. 新摘要写回持久化检查点，供下一轮（乃至会话恢复后）复用。
//
// Returns false when the smart path could not run (caller falls back to the
// sliding window).
func (p *PluginLoadHistory) loadSmart(ctx context.Context, chatManage *types.ChatManage, maxRounds int) bool {
	cc := contextConfigFromContext(ctx, chatManage)

	// Recent window: explicit RecentMessageCount wins; otherwise MaxRounds.
	recentRounds := maxRounds
	if cc != nil && cc.RecentMessageCount > 0 {
		recentRounds = cc.RecentMessageCount
	}
	// Deep fetch window: explicit SummarizeThreshold wins; otherwise 3x the
	// recent window so there is something meaningful to compress.
	fetchRounds := recentRounds * 3
	if cc != nil && cc.SummarizeThreshold > 0 {
		fetchRounds = cc.SummarizeThreshold
	}
	if fetchRounds <= recentRounds {
		fetchRounds = recentRounds + 1
	}

	// 校验检查点的前缀要回读被它覆盖的那些轮次，因此抓取窗口必须能装下
	// 「已覆盖轮次 + 本轮要重放的轮次」。
	checkpoint := p.loadUsableCheckpoint(ctx, chatManage.SessionID)
	if checkpoint != nil {
		if need := checkpoint.ContextCheckpoint.CoveredTurns + recentRounds + 1; need > fetchRounds {
			fetchRounds = need
		}
	}

	turns, err := p.loadSmartTurns(ctx, chatManage.SessionID, fetchRounds)
	if err != nil {
		pipelineWarn(ctx, "LoadHistory", "smart_history_fetch", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"error":      err.Error(),
		})
		return false
	}

	// 检查点复用：定位覆盖边界并严格校验前缀（PRD FR-10 规则 2、3）。任何一步不成立
	// 都丢弃检查点、退回全量重算。
	coveredCount := 0
	existingSummary := ""
	var reuse *types.ContextCheckpoint
	if checkpoint != nil {
		if n, ok := verifySmartCheckpoint(ctx, chatManage.SessionID, checkpoint, turns); ok {
			coveredCount = n
			reuse = checkpoint.ContextCheckpoint
			existingSummary = reuse.Summary
		} else {
			checkpoint = nil
		}
	}
	pending := turns[coveredCount:]

	if len(pending) == 0 {
		// 全部历史都已被检查点覆盖：这一轮的历史层只有摘要。
		chatManage.History = nil
		chatManage.HistorySummary = existingSummary
		pipelineInfo(ctx, "LoadHistory", "smart_output", map[string]interface{}{
			"session_id":       chatManage.SessionID,
			"checkpoint_reuse": true,
			"covered_rounds":   coveredCount,
			"kept_rounds":      0,
		})
		return true
	}

	cfg := contextx.SmartHistoryConfig{RecentRounds: recentRounds}.Normalize()
	xturns := make([]contextx.Turn, 0, len(pending))
	for i, t := range pending {
		xturns = append(xturns, contextx.Turn{User: t.history.Query, Assistant: t.history.Answer, Ref: i})
	}

	counter := contextx.CounterForModel(chatManage.ChatModelName, "", "")
	summary, kept, report, err := contextx.SmartCompressWithReport(ctx, xturns, existingSummary,
		cfg, p.summarizeFunc(chatManage), counter)
	if err != nil {
		pipelineWarn(ctx, "LoadHistory", "smart_compress_failed", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"error":      err.Error(),
		})
		return false
	}

	// Context-alias compression on the verbatim survivors.
	if alias := contextx.CompressAliases(kept, contextx.DefaultAliasConfig()); alias.Count > 0 {
		kept = alias.Turns
		summary = "【实体别名】" + alias.Legend + "\n" + summary
	}

	// Map kept turns back to their original history entries. Alias-compressed
	// text replaces Query/Answer on copies so persisted messages stay intact.
	keptHistory := make([]*types.History, 0, len(kept))
	for _, t := range kept {
		if t.Ref < 0 || t.Ref >= len(pending) {
			continue
		}
		src := pending[t.Ref].history
		cp := *src
		cp.Query = t.User
		cp.Answer = t.Assistant
		keptHistory = append(keptHistory, &cp)
	}

	chatManage.History = keptHistory
	chatManage.HistorySummary = summary

	// FR-10：把新摘要写回持久化检查点，下一轮不再重算这段历史。
	p.saveSmartCheckpoint(ctx, chatManage.SessionID, pending, cfg.SplitPoint(len(xturns)),
		kept, report, summary, reuse)

	pipelineInfo(ctx, "LoadHistory", "smart_output", map[string]interface{}{
		"session_id":       chatManage.SessionID,
		"fetched_rounds":   len(turns),
		"checkpoint_reuse": reuse != nil,
		"covered_rounds":   coveredCount,
		"pending_rounds":   len(pending),
		"kept_rounds":      len(keptHistory),
		"summary_tokens":   counter.Count(summary),
		"summarize_calls":  report.SummarizeCalls,
	})
	return true
}

// loadSmartTurns 按 loadAndProcessHistory 的口径把消息行分组成轮次，但保住轮次身份
// 与检查点落点，并按时间升序返回最新的 fetchRounds 轮。
func (p *PluginLoadHistory) loadSmartTurns(
	ctx context.Context, sessionID string, fetchRounds int,
) ([]*smartTurn, error) {
	rows, err := p.messageService.GetRecentMessagesBySession(ctx, sessionID, fetchRounds*2+10)
	if err != nil {
		return nil, err
	}

	index := make(map[string]*smartTurn, len(rows))
	order := make([]string, 0, len(rows))
	for _, msg := range rows {
		t, ok := index[msg.RequestID]
		if !ok {
			t = &smartTurn{requestID: msg.RequestID, history: &types.History{}}
			index[msg.RequestID] = t
			order = append(order, msg.RequestID)
		}
		switch msg.Role {
		case "user":
			// 与 loadAndProcessHistory 同口径：Content + 图片说明 + 附件。RenderedContent
			// 是旧提示词格式的快照，不参与重放，也不参与前缀哈希。
			t.userContent = msg.Content
			t.createdAt = msg.CreatedAt
			query := msg.Content
			if desc := extractImageCaptions(msg.Images); desc != "" {
				query += "\n\n[用户上传图片内容]\n" + desc
			}
			if len(msg.Attachments) > 0 {
				query += msg.Attachments.BuildPrompt()
			}
			t.history.Query = query
			t.history.CreateAt = msg.CreatedAt
		case "assistant":
			t.assistantMessageID = msg.ID
			t.assistantContent = msg.Content
			t.history.Answer = regThinkTags.ReplaceAllString(msg.Content, "")
			t.history.KnowledgeReferences = msg.KnowledgeReferences
		}
	}

	turns := make([]*smartTurn, 0, len(order))
	for _, requestID := range order {
		t := index[requestID]
		if t.history.Query != "" && t.history.Answer != "" {
			turns = append(turns, t)
		}
	}
	sort.Slice(turns, func(i, j int) bool {
		return turns[i].createdAt.Before(turns[j].createdAt)
	})
	if len(turns) > fetchRounds {
		turns = turns[len(turns)-fetchRounds:]
	}
	return turns, nil
}

// loadUsableCheckpoint 返回会话中最新的一份可用压缩检查点；没有、不可用或读取失败都
// 返回 nil。读取失败只意味着本轮重算一次摘要，绝不因此中断装载。
func (p *PluginLoadHistory) loadUsableCheckpoint(ctx context.Context, sessionID string) *types.Message {
	msg, err := p.messageService.GetLatestContextCheckpoint(ctx, sessionID)
	if err != nil {
		pipelineWarn(ctx, "LoadHistory", "context_checkpoint_read_failed", map[string]interface{}{
			"session_id": sessionID,
			"error":      err.Error(),
		})
		return nil
	}
	if msg == nil || !msg.ContextCheckpoint.Usable() {
		return nil
	}
	return msg
}

// verifySmartCheckpoint 校验检查点覆盖区间 [AnchorTurnID, 检查点所在轮次] 的前缀是否
// 仍然逐字节一致（PRD FR-10 规则 3）。返回被覆盖的轮次数。
func verifySmartCheckpoint(
	ctx context.Context, sessionID string, checkpoint *types.Message, turns []*smartTurn,
) (int, bool) {
	cp := checkpoint.ContextCheckpoint
	covered := -1
	anchor := -1
	for i, t := range turns {
		if t.assistantMessageID == checkpoint.ID {
			covered = i
		}
		if cp.AnchorTurnID != "" && t.requestID == cp.AnchorTurnID {
			anchor = i
		}
	}
	reason := ""
	switch {
	case covered < 0:
		reason = "covered_turn_not_loaded"
	case anchor < 0 || anchor > covered:
		reason = "anchor_turn_not_loaded"
	}
	if reason == "" {
		stored := make([][]types.StoredMessage, 0, covered-anchor+1)
		for _, t := range turns[anchor : covered+1] {
			stored = append(stored, t.storedMessages())
		}
		got := compaction.FoldStoredTurns(
			types.CheckpointChainSeed(compaction.CheckpointPromptVersion, cp.BaseHash, cp.AnchorTurnID), stored)
		if got != cp.PrefixHash {
			reason = "prefix_hash_mismatch"
		}
	}
	if reason != "" {
		pipelineWarn(ctx, "LoadHistory", "context_checkpoint_invalidated", map[string]interface{}{
			"session_id":    sessionID,
			"message_id":    checkpoint.ID,
			"reason":        reason,
			"source":        cp.Source,
			"covered_turns": cp.CoveredTurns,
		})
		return 0, false
	}
	return covered + 1, true
}

// saveSmartCheckpoint 把本轮的增量摘要写回持久化检查点（PRD FR-10 规则 1、4）。
//
// 只有真正被折进摘要的轮次才算被覆盖：摘要模型不可用或没有可压缩轮次时（report
// 为 0）不写检查点，否则那些轮次会在下一轮既不被重放、也没有摘要替代。
func (p *PluginLoadHistory) saveSmartCheckpoint(
	ctx context.Context,
	sessionID string,
	pending []*smartTurn,
	splitAt int,
	kept []contextx.Turn,
	report contextx.SmartCompressReport,
	summary string,
	reuse *types.ContextCheckpoint,
) {
	if splitAt <= 0 || splitAt > len(pending) || report.CompressedRounds == 0 {
		return
	}
	last := pending[splitAt-1]
	if last.assistantMessageID == "" {
		return
	}

	anchorID, baseHash, chain, covered := "", "", "", 0
	if reuse != nil {
		anchorID, baseHash, chain, covered = reuse.AnchorTurnID, reuse.BaseHash, reuse.PrefixHash, reuse.CoveredTurns
	}
	if anchorID == "" {
		anchorID = pending[0].requestID
	}
	if chain == "" {
		chain = types.CheckpointChainSeed(compaction.CheckpointPromptVersion, baseHash, anchorID)
	}
	stored := make([][]types.StoredMessage, 0, splitAt)
	for _, t := range pending[:splitAt] {
		stored = append(stored, t.storedMessages())
	}
	chain = compaction.FoldStoredTurns(chain, stored)
	covered += splitAt

	// sticky 轮次没有进入摘要，但检查点声明覆盖了它们：把原文附在持久化摘要末尾，
	// 下一轮丢弃这些轮次时不至于只剩散文（决定、期限、关键数字必须原样活着）。
	persisted := summary + stickyVerbatimBlock(kept, len(kept)-(len(pending)-splitAt))

	if err := p.messageService.SaveContextCheckpoint(ctx, sessionID, last.assistantMessageID,
		&types.ContextCheckpoint{
			Summary:      persisted,
			CreatedAt:    time.Now(),
			PrefixHash:   chain,
			AnchorTurnID: anchorID,
			BaseHash:     baseHash,
			CoveredTurns: covered,
			Source:       types.ContextCheckpointSourceSmart,
		}); err != nil {
		pipelineWarn(ctx, "LoadHistory", "context_checkpoint_save_failed", map[string]interface{}{
			"session_id": sessionID,
			"message_id": last.assistantMessageID,
			"error":      err.Error(),
		})
		return
	}
	pipelineInfo(ctx, "LoadHistory", "context_checkpoint_saved", map[string]interface{}{
		"session_id":      sessionID,
		"message_id":      last.assistantMessageID,
		"covered_rounds":  covered,
		"newly_covered":   splitAt,
		"summarize_calls": report.SummarizeCalls,
		"sticky_verbatim": report.StickyKeptRounds,
		"summary_chars":   len(persisted),
		"incremental":     reuse != nil,
	})
}

// stickyVerbatimBlock 把旧区间里原样保留的 sticky 轮次渲染成一段附加文本。
// stickyCount 是 kept 头部属于旧区间的轮次数（kept = sticky + recent）。
func stickyVerbatimBlock(kept []contextx.Turn, stickyCount int) string {
	if stickyCount <= 0 {
		return ""
	}
	if stickyCount > len(kept) {
		stickyCount = len(kept)
	}
	var b strings.Builder
	b.WriteString("\n\n[以下轮次因包含决定/期限/关键数字而原样保留]\n")
	used := 0
	written := 0
	for i := 0; i < stickyCount; i++ {
		entry := fmt.Sprintf("User: %s\nAssistant: %s\n", kept[i].User, kept[i].Assistant)
		if used+len(entry) > maxStickyVerbatimBytes {
			break
		}
		b.WriteString(entry)
		used += len(entry)
		written++
	}
	if written == 0 {
		return ""
	}
	return b.String()
}

// summarizeFunc wires the Map-Reduce summarizer to the session's chat model
// with conservative decoding parameters. A nil function (model resolution
// failure) makes SmartCompress keep raw rounds instead of failing the turn.
func (p *PluginLoadHistory) summarizeFunc(chatManage *types.ChatManage) contextx.SummarizeFunc {
	return func(ctx context.Context, prompt string) (string, error) {
		m, err := p.modelService.GetChatModel(ctx, chatManage.ChatModelID)
		if err != nil {
			return "", err
		}
		resp, err := m.Chat(ctx, []chat.Message{{Role: "user", Content: prompt}}, &chat.ChatOptions{
			Temperature:         0.2,
			MaxCompletionTokens: 1024,
		})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}
