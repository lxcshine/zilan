package chatpipeline

import (
	"context"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

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

// loadSmart implements the smart history-compression branch:
//
//  1. Fetch up to SummarizeThreshold rounds (deeper than the sliding window).
//  2. Map-Reduce: rounds older than the recent window are chunked and
//     summarized by a lightweight LLM call; partial summaries are merged into
//     one running summary (MaxSummaryTokens-capped).
//  3. Sticky rounds (decisions, deadlines, key numbers, explicit approval)
//     are exempt from compression and kept verbatim.
//  4. Alias compression replaces long entity names repeated across the kept
//     rounds with short codes; the legend rides at the top of the summary.
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

	historyList, err := loadAndProcessHistory(ctx, p.messageService, chatManage.SessionID, fetchRounds, fetchRounds*2+10)
	if err != nil {
		pipelineWarn(ctx, "LoadHistory", "smart_history_fetch", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"error":      err.Error(),
		})
		return false
	}
	if len(historyList) <= recentRounds {
		// Nothing to compress — behaves exactly like the sliding window.
		chatManage.History = historyList
		return true
	}

	turns := make([]contextx.Turn, 0, len(historyList))
	for i, h := range historyList {
		turns = append(turns, contextx.Turn{User: h.Query, Assistant: h.Answer, Ref: i})
	}

	counter := contextx.CounterForModel(chatManage.ChatModelName, "", "")
	summary, kept, err := contextx.SmartCompress(ctx, turns, "",
		contextx.SmartHistoryConfig{RecentRounds: recentRounds},
		p.summarizeFunc(chatManage), counter)
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
		if t.Ref < 0 || t.Ref >= len(historyList) {
			continue
		}
		src := historyList[t.Ref]
		cp := *src
		cp.Query = t.User
		cp.Answer = t.Assistant
		keptHistory = append(keptHistory, &cp)
	}

	chatManage.History = keptHistory
	chatManage.HistorySummary = summary

	pipelineInfo(ctx, "LoadHistory", "smart_output", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"fetched_rounds": len(historyList),
		"kept_rounds":    len(keptHistory),
		"summary_tokens": counter.Count(summary),
	})
	return true
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
