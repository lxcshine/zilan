package chatpipeline

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/types"
)

// ---------------------------------------------------------------------------
// Five-layer context management helpers (ima-grade context governance)
// ---------------------------------------------------------------------------

// contextConfigFromContext extracts the tenant's ContextConfig from the
// request context, falling back to the pipeline request's GlobalContextConfig
// when unset or zero-valued.
func contextConfigFromContext(ctx context.Context, chatManage *types.ChatManage) *types.ContextConfig {
	var tenantCC *types.ContextConfig
	if tenant, ok := types.TenantInfoFromContext(ctx); ok && tenant != nil {
		tenantCC = tenant.ContextConfig
	}
	var globalCC *types.ContextConfig
	if chatManage != nil {
		globalCC = chatManage.GlobalContextConfig
	}
	if tenantCC == nil {
		return globalCC
	}
	if globalCC == nil {
		return tenantCC
	}

	// Merge field by field: non-zero tenant fields override global defaults.
	merged := *globalCC
	if tenantCC.MaxTokens > 0 {
		merged.MaxTokens = tenantCC.MaxTokens
	}
	if tenantCC.CompressionStrategy != "" {
		merged.CompressionStrategy = tenantCC.CompressionStrategy
	}
	if tenantCC.RecentMessageCount > 0 {
		merged.RecentMessageCount = tenantCC.RecentMessageCount
	}
	if tenantCC.SummarizeThreshold > 0 {
		merged.SummarizeThreshold = tenantCC.SummarizeThreshold
	}
	return &merged
}

// smartCompressionEnabled reports whether the effective context configuration
// selected the smart five-layer context architecture (compression_strategy = "smart").
func smartCompressionEnabled(ctx context.Context, chatManage *types.ChatManage) bool {
	cc := contextConfigFromContext(ctx, chatManage)
	return cc != nil && cc.CompressionStrategy == types.ContextCompressionSmart
}

// intentClassFor maps the pipeline query intent onto the coarse budget
// intent classes used by the five-layer allocator:
//
//	代码/技术问题（kb_search / follow_up / clarification） → tech（检索层 50%）
//	闲聊/创意（greeting / chitchat）                        → chat（历史层 45%）
//	摘要/分析（summarize）                                  → analysis（L2+L3 均衡）
//	其它                                                    → general
func intentClassFor(intent types.QueryIntent) contextx.IntentClass {
	switch intent {
	case types.IntentChitchat, types.IntentGreeting:
		return contextx.IntentChat
	case types.IntentSummarize:
		return contextx.IntentAnalysis
	case types.IntentKBSearch, types.IntentFollowUp, types.IntentClarification:
		return contextx.IntentTech
	default:
		return contextx.IntentGeneral
	}
}

// reserveOutputFor derives the completion-token reserve from the chat
// parameters; falls back to the package default when unset.
func reserveOutputFor(chatManage *types.ChatManage) int {
	if chatManage != nil && chatManage.SummaryConfig.MaxCompletionTokens > 0 {
		return chatManage.SummaryConfig.MaxCompletionTokens
	}
	return contextx.DefaultCompletionReserve
}

// ---------------------------------------------------------------------------
// Retrieval tier mapping + heading-path citation
// ---------------------------------------------------------------------------

// searchResultsToTierResults maps merged search results into contextx tier
// results, preserving enriched passage content (images inlined) and deriving
// the section path used for precise citation.
func searchResultsToTierResults(ctx context.Context, results []*types.SearchResult) []*contextx.TierResult {
	out := make([]*contextx.TierResult, 0, len(results))
	for _, r := range results {
		if r == nil {
			continue
		}
		out = append(out, &contextx.TierResult{
			ID:          r.ID,
			KnowledgeID: r.KnowledgeID,
			Content:     getEnrichedPassageForChat(ctx, r),
			Title:       firstPipelineTitle(r),
			HeadingPath: headingPathOf(r),
			Score:       r.Score,
		})
	}
	return out
}

// metadataKeysHeadingPath lists chunk-metadata keys that may carry a
// precomputed section path (written by newer splitters).
var metadataKeysHeadingPath = []string{"section_path", "heading_path", "chapter_path"}

// headingPathOf resolves the full section path of a chunk for citation
// (e.g. "1.2 安装指南 > 1.2.3 Docker 部署"). Resolution order:
//  1. explicit chunk metadata (section_path / heading_path / chapter_path)
//  2. leading markdown heading lines of the chunk content — markdown-aware
//     splitters with header continuation prepend the heading lineage
//  3. empty string (citation simply omits the path)
func headingPathOf(r *types.SearchResult) string {
	if r == nil {
		return ""
	}
	for _, k := range metadataKeysHeadingPath {
		if v := strings.TrimSpace(r.Metadata[k]); v != "" {
			return v
		}
	}
	return leadingHeadingPath(r.Content)
}

// leadingHeadingPath extracts up to 3 heading lines from the head of the
// chunk content and joins them into a " > " path.
func leadingHeadingPath(content string) string {
	const maxHeadRunes = 600
	runes := []rune(content)
	if len(runes) > maxHeadRunes {
		content = string(runes[:maxHeadRunes])
	}
	var parts []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			// Stop at the first non-heading, non-empty line: the heading
			// lineage is always a contiguous block at the chunk head.
			if trimmed != "" && len(parts) > 0 {
				break
			}
			if trimmed != "" {
				// Content does not start with headings.
				return ""
			}
			continue
		}
		title := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		if title != "" {
			parts = append(parts, title)
		}
		if len(parts) >= 3 {
			break
		}
	}
	return strings.Join(parts, " > ")
}

// ---------------------------------------------------------------------------
// Diagnostics conversion (contextx → types, keeps types import-light)
// ---------------------------------------------------------------------------

func diagToTypes(d contextx.Diagnostics) *types.ContextDiagnostics {
	out := &types.ContextDiagnostics{
		Window:         d.Window,
		Usable:         d.Usable,
		ReservedOutput: d.ReservedOutput,
		Intent:         string(d.Intent),
		Actions:        append([]string(nil), d.Actions...),
	}
	if v, ok := d.BudgetByLayer[contextx.LayerSystem]; ok {
		out.Budget.System = v
	}
	if v, ok := d.BudgetByLayer[contextx.LayerMemory]; ok {
		out.Budget.Memory = v
	}
	if v, ok := d.BudgetByLayer[contextx.LayerRetrieval]; ok {
		out.Budget.Retrieval = v
	}
	if v, ok := d.BudgetByLayer[contextx.LayerHistory]; ok {
		out.Budget.History = v
	}
	if v, ok := d.BudgetByLayer[contextx.LayerQuery]; ok {
		out.Budget.Query = v
	}
	if v, ok := d.UsedByLayer[contextx.LayerSystem]; ok {
		out.Used.System = v
	}
	if v, ok := d.UsedByLayer[contextx.LayerMemory]; ok {
		out.Used.Memory = v
	}
	if v, ok := d.UsedByLayer[contextx.LayerRetrieval]; ok {
		out.Used.Retrieval = v
	}
	if v, ok := d.UsedByLayer[contextx.LayerHistory]; ok {
		out.Used.History = v
	}
	if v, ok := d.UsedByLayer[contextx.LayerQuery]; ok {
		out.Used.Query = v
	}
	return out
}
