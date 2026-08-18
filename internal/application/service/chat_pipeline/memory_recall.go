package chatpipeline

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginMemoryRecall implements the L1 injection side of the three-layer
// memory architecture: at the start of a turn it recalls the caller's L3
// long-term facts and L2 session summaries (scored by semantic similarity x
// time decay x access frequency) and renders them into
// chatManage.MemoryContext, which prepareMessagesWithHistory appends to the
// system prompt.
//
// The stage is strictly best-effort: any recall failure degrades to "no
// memory injected" and never fails the turn.
type PluginMemoryRecall struct {
	memoryService interfaces.MemoryService
}

// NewPluginMemoryRecall creates and registers the memory recall plugin.
func NewPluginMemoryRecall(eventManager *EventManager, memoryService interfaces.MemoryService) *PluginMemoryRecall {
	res := &PluginMemoryRecall{memoryService: memoryService}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles.
func (p *PluginMemoryRecall) ActivationEvents() []types.EventType {
	return []types.EventType{types.MEMORY_RECALL}
}

// OnEvent runs memory recall for the current query.
func (p *PluginMemoryRecall) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	query := strings.TrimSpace(chatManage.Query)
	if query == "" || p.memoryService == nil {
		return next()
	}

	memories, err := p.memoryService.Recall(ctx, &types.MemoryRecallParams{Query: query})
	if err != nil {
		// Recall is an enhancement layer: log and continue without memory.
		pipelineWarn(ctx, "MemoryRecall", "recall_failed", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"error":      err.Error(),
		})
		return next()
	}
	if len(memories) == 0 {
		pipelineInfo(ctx, "MemoryRecall", "no_match", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		return next()
	}

	block := p.memoryService.FormatRecalledForPrompt(memories)
	if block == "" {
		return next()
	}
	chatManage.MemoryContext = block

	pipelineInfo(ctx, "MemoryRecall", "injected", map[string]interface{}{
		"session_id":   chatManage.SessionID,
		"memory_count": len(memories),
		"block_chars":  len(block),
	})
	return next()
}
