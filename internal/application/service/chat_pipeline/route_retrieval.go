package chatpipeline

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginRouteRetrieval resolves the per-request retrieval strategy
// (RetrievalPlan) after query understanding and before search.
//
// It implements section 2.1 of the retrieval optimization design:
//   - Fine-grained intent classification (factual / summary / comparison /
//     reasoning / exploratory), rule-based fast path first, falling back to
//     the LLM-provided classification emitted by query-understand.
//   - Per-intent retrieval parameter shaping (thresholds, top-k, MMR lambda).
//   - Per-intent channel activation (sparse / HyDE / multi-query / step-back /
//     graph) consumed by QUERY_ENHANCE and CHUNK_SEARCH_PARALLEL.
//   - Comparison decomposition: extracts the A/B entities so the search stage
//     can issue per-entity sub-queries.
//
// The plugin never fails the pipeline: on any uncertainty it falls back to the
// factual default profile, preserving legacy behavior.
type PluginRouteRetrieval struct {
	cfgProvider      retrievalConfigProvider
	kbService        interfaces.KnowledgeBaseService
	knowledgeService interfaces.KnowledgeService
	chunkRepo        interfaces.ChunkRepository
}

// retrievalConfigProvider abstracts access to the tenant's RetrievalConfig so
// the plugin stays testable without a full tenant service.
type retrievalConfigProvider func(ctx context.Context) *types.RetrievalConfig

// NewPluginRouteRetrieval creates and registers the routing plugin.
func NewPluginRouteRetrieval(eventManager *EventManager,
	kbService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkRepo interfaces.ChunkRepository,
) *PluginRouteRetrieval {
	res := &PluginRouteRetrieval{
		cfgProvider: func(ctx context.Context) *types.RetrievalConfig {
			tenant, ok := types.TenantInfoFromContext(ctx)
			if !ok || tenant == nil {
				return nil
			}
			return tenant.RetrievalConfig
		},
		kbService:        kbService,
		knowledgeService: knowledgeService,
		chunkRepo:        chunkRepo,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles.
func (p *PluginRouteRetrieval) ActivationEvents() []types.EventType {
	return []types.EventType{types.ROUTE_RETRIEVAL}
}

// OnEvent classifies the retrieval intent and builds the RetrievalPlan.
func (p *PluginRouteRetrieval) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	retrievalCfg := p.cfgProvider(ctx)

	// Routing disabled at tenant level, or retrieval not needed at all:
	// leave Plan nil so downstream stages use legacy request params.
	if !retrievalCfg.IsIntentRoutingEnabled() || !chatManage.NeedsRetrieval() {
		return next()
	}

	intent, source := p.classify(chatManage)
	plan := types.ResolveRetrievalPlan(retrievalCfg, intent, source)

	// KB-class presets: when every KB in scope was profiled into the same
	// document class, blend its preset in underneath the intent profile.
	p.loadKBClasses(ctx, chatManage)
	p.applyKBClassPreset(chatManage, plan, retrievalCfg)

	// Comparison decomposition: rule-based split first, then fall back to the
	// entity extractor output (populated during QUERY_UNDERSTAND when the
	// graph stack is enabled).
	if plan.UseDecompose {
		plan.CompareEntities = extractComparisonEntities(chatManage.RewriteQuery)
		if len(plan.CompareEntities) < 2 {
			plan.CompareEntities = extractComparisonEntities(chatManage.Query)
		}
		if len(plan.CompareEntities) < 2 && len(chatManage.Entity) >= 2 {
			plan.CompareEntities = append([]string(nil), chatManage.Entity[:2]...)
		}
		if len(plan.CompareEntities) < 2 {
			// Not enough sides to decompose: degrade comparison to reasoning,
			// which keeps the multi-document + graph strategy.
			plan.UseDecompose = false
		}
	}

	chatManage.Plan = plan

	// Push the shaped recall parameters into the request fields consumed by
	// the legacy search path, so all downstream code (incl. agent tools and
	// the non-parallel CHUNK_SEARCH path) benefits without modification.
	chatManage.VectorThreshold = plan.VectorThreshold
	chatManage.KeywordThreshold = plan.KeywordThreshold
	chatManage.EmbeddingTopK = plan.EmbeddingTopK
	chatManage.RerankTopK = plan.RerankTopK

	pipelineInfo(ctx, "RouteRetrieval", "plan", map[string]interface{}{
		"session_id":        chatManage.SessionID,
		"intent":            plan.Intent,
		"intent_source":     plan.IntentSource,
		"vector_threshold":  plan.VectorThreshold,
		"keyword_threshold": plan.KeywordThreshold,
		"embedding_top_k":   plan.EmbeddingTopK,
		"rerank_top_k":      plan.RerankTopK,
		"mmr_lambda":        plan.MMRLambda,
		"use_hyde":          plan.UseHyDE,
		"use_multi_query":   plan.UseMultiQuery,
		"use_step_back":     plan.UseStepBack,
		"use_sparse":        plan.UseSparse,
		"use_graph":         plan.UseGraphChannel,
		"use_doc_boost":     plan.UseDocBoost,
		"compare_entities":  plan.CompareEntities,
	})
	return next()
}

// classify picks the retrieval intent: deterministic rules first (zero cost),
// then the LLM-provided hint stored during query understanding, then default.
func (p *PluginRouteRetrieval) classify(chatManage *types.ChatManage) (types.RetrievalIntent, string) {
	// 1. Rule-based fast path on the rewritten query (richest signal), then on
	// the raw query in case rewriting mangled the markers.
	if intent, ok := classifyRetrievalIntentByRules(chatManage.RewriteQuery); ok {
		return intent, "rule"
	}
	if intent, ok := classifyRetrievalIntentByRules(chatManage.Query); ok {
		return intent, "rule"
	}
	// 2. LLM hint from query understanding (parsed from structured output).
	if intent := types.RetrievalIntent(chatManage.LLMRetrievalIntent); intent.Valid() {
		return intent, "llm"
	}
	// 3. Safe default.
	return types.DefaultRetrievalIntent, "default"
}

// applyKBClassPreset blends the auto-detected KB document-class preset into
// the plan when (a) the tenant has not overridden that dimension and (b) all
// in-scope KBs share one class. Class presets only scale recall breadth; they
// never flip enhancement toggles off.
func (p *PluginRouteRetrieval) applyKBClassPreset(
	chatManage *types.ChatManage, plan *types.RetrievalPlan, cfg *types.RetrievalConfig,
) {
	class := dominantKBClass(chatManage.KBClasses)
	if class == "" {
		return
	}
	// Tenant-defined presets win; otherwise fall back to the built-in table.
	var preset *types.IntentRetrievalProfile
	if cfg != nil && cfg.KBClassProfiles != nil {
		preset = cfg.KBClassProfiles[class]
	}
	if preset == nil {
		preset = types.DefaultKBClassProfiles()[class]
	}
	if preset == nil {
		return
	}
	// Scale recall parameters multiplicatively on top of the intent-shaped
	// values; class presets express corpus shape, intent expresses task shape.
	if preset.VectorThresholdScale > 0 {
		plan.VectorThreshold = clamp01f(plan.VectorThreshold * preset.VectorThresholdScale)
	}
	if preset.KeywordThresholdScale > 0 {
		plan.KeywordThreshold = clamp01f(plan.KeywordThreshold * preset.KeywordThresholdScale)
	}
	if preset.TopKScale > 0 {
		plan.EmbeddingTopK = maxInt(1, int(float64(plan.EmbeddingTopK)*preset.TopKScale+0.5))
	}
	if preset.RerankTopKScale > 0 {
		plan.RerankTopK = maxInt(1, int(float64(plan.RerankTopK)*preset.RerankTopKScale+0.5))
	}
	for ch, w := range preset.ChannelWeights {
		if w > 0 {
			plan.ChannelWeights[ch] = w
		}
	}
	// Class presets may switch enhancements ON (e.g. HyDE for papers) but
	// never OFF — the intent profile owns the task-level decision.
	plan.UseHyDE = plan.UseHyDE || preset.EnableHyDE
	plan.UseMultiQuery = plan.UseMultiQuery || preset.EnableMultiQuery
	plan.UseStepBack = plan.UseStepBack || preset.EnableStepBack
	plan.UseSparse = plan.UseSparse || preset.EnableSparse
	plan.UseGraphChannel = plan.UseGraphChannel || preset.EnableGraphChannel
	plan.UseDocBoost = plan.UseDocBoost || preset.DocEvidenceBoost
	chatManage.VectorThreshold = plan.VectorThreshold
	chatManage.KeywordThreshold = plan.KeywordThreshold
	chatManage.EmbeddingTopK = plan.EmbeddingTopK
	chatManage.RerankTopK = plan.RerankTopK
}

// loadKBClasses batch-loads the in-scope KBs once and records each KB's
// auto-detected document class (empty string when unprofiled). Failures are
// non-fatal: class presets simply don't apply. When a KB is unprofiled the
// function profiles it lazily and persists the result.
func (p *PluginRouteRetrieval) loadKBClasses(ctx context.Context, chatManage *types.ChatManage) {
	kbIDs := chatManage.SearchTargets.GetAllKnowledgeBaseIDs()
	if len(kbIDs) == 0 {
		kbIDs = chatManage.KnowledgeBaseIDs
	}
	if len(kbIDs) == 0 || p.kbService == nil {
		return
	}
	kbs, err := p.kbService.GetKnowledgeBasesByIDsOnly(ctx, kbIDs)
	if err != nil {
		pipelineWarn(ctx, "RouteRetrieval", "kb_class_load", map[string]interface{}{
			"error": err.Error(),
		})
		return
	}
	classes := make([]string, 0, len(kbs))
	for _, kb := range kbs {
		if kb == nil {
			continue
		}
		class := kb.DocumentClass
		// Lazy profiling: classify on first access when the column is empty.
		if class == "" && p.knowledgeService != nil && p.chunkRepo != nil {
			if profiled, err := profileKnowledgeBase(ctx, p.kbService, p.knowledgeService, p.chunkRepo, kb.ID, kb.TenantID); err == nil && profiled != "" {
				class = profiled
			}
		}
		classes = append(classes, class)
	}
	chatManage.KBClasses = classes
}

// dominantKBClass returns the shared class when every in-scope KB agrees,
// else "" (mixed/unprofiled scopes keep intent-only shaping).
func dominantKBClass(classes []string) string {
	if len(classes) == 0 {
		return ""
	}
	first := classes[0]
	if first == "" {
		return ""
	}
	for _, c := range classes[1:] {
		if c != first {
			return ""
		}
	}
	return first
}

func clamp01f(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
