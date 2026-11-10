package chatpipeline

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/contextx"
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
// FR-20（上下文预算 × 记忆预算统一调度）：渲染前先按 L1 的 token 预算裁剪，
// 计量口径是精确 tokenizer，条数上限与 token 预算双重生效取更严者。
//
// The stage is strictly best-effort: any recall failure degrades to "no
// memory injected" and never fails the turn.
type PluginMemoryRecall struct {
	memoryService interfaces.MemoryService
}

// memoryPromptFitter 是 MemoryService 的可选扩展能力（FR-20）：在精确 token 预算内
// 渲染召回记忆，并返回结构化记忆块交给五层装配器复核。用可选接口而不是直接扩
// interfaces.MemoryService，避免牵动其它实现与测试替身。
type memoryPromptFitter interface {
	FormatRecalledForPromptWithin(
		memories []*types.RecalledMemory,
		counter *contextx.Counter,
		budgetTokens int,
	) (contextx.MemoryPrompt, contextx.MemoryFitResult)
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

	// L1 预算与精确 tokenizer：L0/L1 占比是常量（见 contextx.StableMemoryFraction），
	// 所以意图分类之前算出的额度与装配阶段完全一致。注意本阶段早于 prepareChatModel，
	// ChatModelName 可能尚未解析，此时退回通用 cl100k_base 计量——仍是精确 tokenizer，
	// 不是字符近似；smart 模式下装配器随后会用厂商 tokenizer 复核一次。
	counter := contextx.CounterForModel(chatManage.ChatModelName, "", "")
	budgetTokens := memoryTokenBudget(ctx, chatManage)

	var (
		block  string
		blocks *types.MemoryPrompt
		fit    contextx.MemoryFitResult
	)
	if fitter, ok := p.memoryService.(memoryPromptFitter); ok {
		prompt, fitted := fitter.FormatRecalledForPromptWithin(memories, counter, budgetTokens)
		fit = fitted
		block = fitted.Text
		blocks = memoryPromptToTypes(prompt)
	} else {
		block = p.memoryService.FormatRecalledForPrompt(memories)
	}
	if strings.TrimSpace(block) == "" {
		return next()
	}
	chatManage.MemoryContext = block
	// 结构化记忆块随请求下传：smart 模式由五层装配器按厂商 tokenizer 复核 L1，
	// 非 smart 模式忽略它（MemoryContext 已经过同一套预算裁剪）。
	chatManage.MemoryBlocks = blocks

	pipelineInfo(ctx, "MemoryRecall", "injected", map[string]interface{}{
		"session_id":            chatManage.SessionID,
		"memory_count":          len(memories),
		"memory_count_resident": residentCount(memories),
		"memory_count_semantic": len(memories) - residentCount(memories),
		// 预算归因口径：精确 tokenizer（FR-20 ⑪），与下面的字符口径日志严格分开，
		// 字符数只用于模块级可观测，绝不参与预算或持久化。
		"memory_budget_tokens": budgetTokens,
		"memory_used_tokens":   counter.Count(block),
		"memory_dropped":       fit.Dropped,
		"memory_degraded":      fit.Degraded,
		// 模块级字符口径（PRD P0-2 §7 既有可观测字段，仅用于日志）。
		"block_chars":        len(block),
		"block_chars_soul":   injectedCharsByModule(memories, types.MemoryCategorySoul),
		"block_chars_user":   injectedCharsByModule(memories, types.MemoryCategoryProfile) + injectedCharsByModule(memories, types.MemoryCategoryFact),
		"block_chars_memory": injectedCharsByModule(memories, types.MemoryCategoryPreference) + injectedCharsByModule(memories, types.MemoryCategoryTodo),
		"block_chars_agent":  injectedCharsByModule(memories, types.MemoryCategorySkill),
	})
	return next()
}

// memoryTokenBudget 计算 L1（记忆层）的 token 预算：与 prepareMessagesFiveLayer
// 使用同一套窗口解析与固定占比，因此在意图分类之前就能得到与装配阶段一致的额度。
func memoryTokenBudget(ctx context.Context, chatManage *types.ChatManage) int {
	_, memory := contextx.StableLayerBudgets(
		resolveContextWindow(ctx, chatManage), reserveOutputFor(chatManage))
	return memory
}

// memoryPromptToTypes 把 contextx 的结构化记忆块转换成 types 侧镜像，
// 以便随 ChatManage 在流水线阶段之间传递（types 不能反向依赖 contextx）。
func memoryPromptToTypes(prompt contextx.MemoryPrompt) *types.MemoryPrompt {
	if prompt.IsEmpty() {
		return nil
	}
	out := &types.MemoryPrompt{Header: prompt.Header}
	for _, section := range prompt.Sections {
		target := types.MemoryPromptSection{Title: section.Title}
		for _, blk := range section.Blocks {
			target.Blocks = append(target.Blocks, types.MemoryPromptBlock{
				Text:     blk.Text,
				Summary:  blk.Summary,
				Score:    blk.Score,
				Resident: blk.Resident,
			})
		}
		out.Sections = append(out.Sections, target)
	}
	return out
}

// memoryPromptFromTypes 是 memoryPromptToTypes 的逆变换，供五层装配使用。
func memoryPromptFromTypes(prompt *types.MemoryPrompt) contextx.MemoryPrompt {
	if prompt == nil {
		return contextx.MemoryPrompt{}
	}
	out := contextx.MemoryPrompt{Header: prompt.Header}
	for _, section := range prompt.Sections {
		target := contextx.MemorySection{Title: section.Title}
		for _, blk := range section.Blocks {
			target.Blocks = append(target.Blocks, contextx.MemoryBlock{
				Text:     blk.Text,
				Summary:  blk.Summary,
				Score:    blk.Score,
				Resident: blk.Resident,
			})
		}
		out.Sections = append(out.Sections, target)
	}
	return out
}

// residentCount counts always-inject memories in the recall set (PRD P0-3
// FR3: dual-path observability).
func residentCount(memories []*types.RecalledMemory) int {
	count := 0
	for _, m := range memories {
		if m.Resident {
			count++
		}
	}
	return count
}

// injectedCharsByModule sums the rendered content length of injected facts of
// one category (module-level observability, PRD P0-2 §7).
func injectedCharsByModule(memories []*types.RecalledMemory, category string) int {
	chars := 0
	for _, m := range memories {
		if m.Kind == "fact" && m.Fact != nil && m.Fact.Category == category {
			chars += len(m.Fact.Content)
		}
	}
	return chars
}
