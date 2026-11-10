package chatpipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginSearch implements search functionality for chat pipeline
type PluginSearch struct {
	knowledgeBaseService  interfaces.KnowledgeBaseService
	knowledgeService      interfaces.KnowledgeService
	chunkService          interfaces.ChunkService
	config                *config.Config
	webSearchService      interfaces.WebSearchService
	tenantService         interfaces.TenantService
	sessionService        interfaces.SessionService
	webSearchStateService interfaces.WebSearchStateService
	webSearchProviderRepo interfaces.WebSearchProviderRepository
}

func NewPluginSearch(eventManager *EventManager,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	config *config.Config,
	webSearchService interfaces.WebSearchService,
	tenantService interfaces.TenantService,
	sessionService interfaces.SessionService,
	webSearchStateService interfaces.WebSearchStateService,
	webSearchProviderRepo interfaces.WebSearchProviderRepository,
) *PluginSearch {
	res := &PluginSearch{
		knowledgeBaseService:  knowledgeBaseService,
		knowledgeService:      knowledgeService,
		chunkService:          chunkService,
		config:                config,
		webSearchService:      webSearchService,
		tenantService:         tenantService,
		sessionService:        sessionService,
		webSearchStateService: webSearchStateService,
		webSearchProviderRepo: webSearchProviderRepo,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginSearch) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHUNK_SEARCH}
}

// OnEvent handles search events in the chat pipeline
func (p *PluginSearch) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	// Check if we have search targets or web search enabled
	hasKBTargets := types.HasKnowledgeRetrievalScope(
		chatManage.SearchTargets,
		chatManage.KnowledgeBaseIDs,
		chatManage.KnowledgeIDs,
	)
	if !hasKBTargets && !chatManage.WebSearchEnabled {
		pipelineError(ctx, "Search", "kb_not_found", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		return nil
	}

	pipelineInfo(ctx, "Search", "input", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"rewrite_query":  chatManage.RewriteQuery,
		"search_targets": len(chatManage.SearchTargets),
		"tenant_id":      chatManage.TenantID,
		"web_enabled":    chatManage.WebSearchEnabled,
	})

	// Run KB search and web search concurrently
	pipelineInfo(ctx, "Search", "plan", map[string]interface{}{
		"search_targets":    len(chatManage.SearchTargets),
		"embedding_top_k":   chatManage.EmbeddingTopK,
		"vector_threshold":  chatManage.VectorThreshold,
		"keyword_threshold": chatManage.KeywordThreshold,
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	allResults := make([]*types.SearchResult, 0)

	wg.Add(2)
	// Goroutine 1: Knowledge base search using SearchTargets
	go func() {
		defer wg.Done()
		kbResults := p.searchByTargets(ctx, chatManage)
		if len(kbResults) > 0 {
			mu.Lock()
			allResults = append(allResults, kbResults...)
			mu.Unlock()
		}
	}()

	// Goroutine 2: Web search (if enabled)
	go func() {
		defer wg.Done()
		webResults := p.searchWebIfEnabled(ctx, chatManage)
		if len(webResults) > 0 {
			mu.Lock()
			allResults = append(allResults, webResults...)
			mu.Unlock()
		}
	}()

	wg.Wait()

	chatManage.SearchResult = allResults

	logSearchScoreSample(ctx, "result_score_before_normalize", chatManage.SearchResult)

	// If recall is low, attempt query expansion with keyword-focused search
	if chatManage.EnableQueryExpansion && len(chatManage.SearchResult) < max(1, chatManage.EmbeddingTopK) {
		expResults := p.runQueryExpansion(ctx, chatManage)
		if len(expResults) > 0 {
			chatManage.SearchResult = append(chatManage.SearchResult, expResults...)
		}
	}

	logSearchScoreSample(ctx, "final_score", chatManage.SearchResult)

	// Return if we have results
	if len(chatManage.SearchResult) != 0 {
		pipelineInfo(ctx, "Search", "output", map[string]interface{}{
			"session_id":   chatManage.SessionID,
			"result_count": len(chatManage.SearchResult),
		})
		return next()
	}
	pipelineWarn(ctx, "Search", "output", map[string]interface{}{
		"session_id":   chatManage.SessionID,
		"result_count": 0,
	})
	return ErrSearchNothing
}

// getSearchResultFromHistory retrieves relevant knowledge references from chat history
func getSearchResultFromHistory(chatManage *types.ChatManage) []*types.SearchResult {
	if len(chatManage.History) == 0 {
		return nil
	}
	// Search history in reverse chronological order
	for i := len(chatManage.History) - 1; i >= 0; i-- {
		if len(chatManage.History[i].KnowledgeReferences) > 0 {
			// Mark all references as history matches
			for _, reference := range chatManage.History[i].KnowledgeReferences {
				reference.MatchType = types.MatchTypeHistory
			}
			return chatManage.History[i].KnowledgeReferences
		}
	}
	return nil
}

func removeDuplicateResults(results []*types.SearchResult) []*types.SearchResult {
	seen := make(map[string]bool)
	contentSig := make(map[string]string) // sig -> first chunk ID
	var uniqueResults []*types.SearchResult
	for _, r := range results {
		// Only deduplicate by exact chunk ID — do NOT treat shared ParentChunkID
		// as duplicates, because different child chunks of the same parent carry
		// different content segments that may all be relevant.
		if seen[r.ID] {
			logger.Debugf(context.Background(), "Dedup: chunk %s removed due to duplicate ID", r.ID)
			continue
		}
		sig := buildContentSignature(r.Content)
		if sig != "" {
			if firstChunk, exists := contentSig[sig]; exists {
				logger.Debugf(context.Background(), "Dedup: chunk %s removed due to content signature (dup of %s, sig prefix: %.50s...)", r.ID, firstChunk, sig)
				continue
			}
			contentSig[sig] = r.ID
		}
		seen[r.ID] = true
		uniqueResults = append(uniqueResults, r)
	}
	return uniqueResults
}

func buildContentSignature(content string) string {
	return searchutil.BuildContentSignature(content)
}

// removePartialOverlaps drops chunks whose content is largely contained within
// a higher-scored chunk, even across different knowledge sources. This catches
// cross-KB duplicates and near-duplicates that exact-signature dedup misses.
//
// Two thresholds are used:
//   - Substring containment: if the normalized short text is a literal substring
//     of the normalized long text, the shorter chunk is removed.
//   - Token overlap coefficient >= 0.85: if 85%+ of the smaller chunk's tokens
//     appear in the larger chunk, the smaller one is redundant.
//
// The input slice MUST already be deduplicated by ID/signature. Within each
// pair the chunk with the lower score is the candidate for removal; ties are
// broken by content length (longer wins).
func removePartialOverlaps(ctx context.Context, results []*types.SearchResult) []*types.SearchResult {
	const overlapThreshold = 0.85

	if len(results) <= 1 {
		return results
	}

	type normEntry struct {
		norm   string
		result *types.SearchResult
	}

	entries := make([]normEntry, 0, len(results))
	for _, r := range results {
		entries = append(entries, normEntry{
			norm:   searchutil.NormalizeContent(r.Content),
			result: r,
		})
	}

	removed := make(map[int]bool)

	for i := 0; i < len(entries); i++ {
		if removed[i] {
			continue
		}
		for j := i + 1; j < len(entries); j++ {
			if removed[j] {
				continue
			}

			a, b := entries[i], entries[j]

			shortIdx, longIdx := i, j
			if len(a.norm) > len(b.norm) {
				shortIdx, longIdx = j, i
			}

			contained := searchutil.IsContentContained(
				entries[shortIdx].norm, entries[longIdx].norm,
			)

			if !contained {
				ratio := searchutil.ContentOverlapRatio(
					entries[shortIdx].result.Content,
					entries[longIdx].result.Content,
				)
				if ratio < overlapThreshold {
					continue
				}
			}

			victim := shortIdx
			if entries[shortIdx].result.Score > entries[longIdx].result.Score {
				victim = longIdx
			}
			removed[victim] = true

			keptIdx := i
			if victim == i {
				keptIdx = j
			}
			pipelineInfo(ctx, "Merge", "partial_overlap_drop", map[string]interface{}{
				"kept_id":    entries[keptIdx].result.ID,
				"dropped_id": entries[victim].result.ID,
				"contained":  contained,
			})
		}
	}

	out := make([]*types.SearchResult, 0, len(results)-len(removed))
	for i, e := range entries {
		if !removed[i] {
			out = append(out, e.result)
		}
	}
	return out
}

func logSearchScoreSample(ctx context.Context, action string, results []*types.SearchResult) {
	const maxLogRows = 8
	limit := min(maxLogRows, len(results))
	for i := 0; i < limit; i++ {
		r := results[i]
		pipelineInfo(ctx, "Search", action, map[string]interface{}{
			"index":      i,
			"chunk_id":   r.ID,
			"score":      fmt.Sprintf("%.4f", r.Score),
			"match_type": r.MatchType,
		})
	}
	if len(results) > limit {
		pipelineInfo(ctx, "Search", action+"_summary", map[string]interface{}{
			"total":     len(results),
			"logged":    limit,
			"truncated": len(results) - limit,
		})
	}
}

// SearchChannel runs a single-channel KB retrieval using the given query
// text / embedding and vector/keyword switches. It is the building block for
// multi-channel recall in CHUNK_SEARCH_PARALLEL.
//
// 错误契约（FR-19⑤）：返回的结果与错误是**并存**的。部分知识库/分组检索失败
// 时，已成功通道的结果照常返回，失败被聚合成一个 error 一并交给调用方，由调用
// 方决定降级还是整轮失败——绝不再把残缺结果当成完整结果静默吞掉。
func (p *PluginSearch) SearchChannel(
	ctx context.Context,
	chatManage *types.ChatManage,
	queryText string,
	queryEmbedding []float32,
	disableVector bool,
	disableKeyword bool,
) ([]*types.SearchResult, error) {
	return p.searchByTargetsWithOpts(ctx, chatManage, &types.SearchParams{
		QueryText:            queryText,
		QueryEmbedding:       queryEmbedding,
		DisableVectorMatch:   disableVector,
		DisableKeywordsMatch: disableKeyword,
	})
}

// searchErrorCollector 汇总一次检索中各并发子任务（模型分组 / 单目标）的失败。
// 检索是“多条腿”的：一条腿挂了不该丢掉其他腿的结果，但也不能让上层以为拿到
// 的是完整结果，因此这里把失败收集起来，最后与结果一并返回。
type searchErrorCollector struct {
	mu   sync.Mutex
	errs []error
}

// add 记录一次失败。reason 用于拼出可读的聚合错误信息。
func (c *searchErrorCollector) add(reason string, err error) {
	if c == nil || err == nil {
		return
	}
	c.mu.Lock()
	c.errs = append(c.errs, fmt.Errorf("%s: %w", reason, err))
	c.mu.Unlock()
}

// join 返回聚合错误；没有任何失败时返回 nil。
func (c *searchErrorCollector) join() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errs) == 0 {
		return nil
	}
	return fmt.Errorf("retrieval returned partial results (%d failures): %w",
		len(c.errs), errors.Join(c.errs...))
}

// searchByTargets performs KB searches using pre-computed SearchTargets.
// Targets sharing the same underlying embedding model (identified by model
// name + endpoint, not just model ID) are grouped so the query embedding is
// computed once per model AND all full-KB targets in a group are combined into
// a single retrieval call, reducing both embedding API calls and DB round-trips.
func (p *PluginSearch) searchByTargets(
	ctx context.Context,
	chatManage *types.ChatManage,
) []*types.SearchResult {
	results, err := p.searchByTargetsWithOpts(ctx, chatManage, nil)
	if err != nil {
		// 单路检索保持既有“降级可用”的行为：结果照用，失败显式记录到
		// 流水线告警（可观测位置），不再静默丢弃。
		pipelineWarn(ctx, "Search", "partial_retrieval_failure", map[string]interface{}{
			"error": err.Error(),
		})
	}
	return results
}

// searchByTargetsWithOpts is the channel-aware variant of searchByTargets.
// When opts is nil it behaves exactly like the legacy searchByTargets.
// Otherwise it uses the provided query text / embedding and respects
// DisableVectorMatch / DisableKeywordsMatch so the caller can build
// single-channel recall paths (vector-only, keyword-only, HyDE-only, etc.).
func (p *PluginSearch) searchByTargetsWithOpts(
	ctx context.Context,
	chatManage *types.ChatManage,
	opts *types.SearchParams,
) ([]*types.SearchResult, error) {
	if len(chatManage.SearchTargets) == 0 {
		return nil, nil
	}
	// 收集各并发子任务的失败，最后与结果一并返回（保留成功腿 + 附带错误）。
	var errs searchErrorCollector

	queryText := strings.TrimSpace(chatManage.RewriteQuery)
	var queryEmbedding []float32
	var disableVector, disableKeyword bool
	if opts != nil {
		if opts.QueryText != "" {
			queryText = opts.QueryText
		}
		queryEmbedding = opts.QueryEmbedding
		disableVector = opts.DisableVectorMatch
		disableKeyword = opts.DisableKeywordsMatch
	}

	// Batch-fetch KB records to determine embedding model grouping.
	// On failure, all targets fall into an empty-key group and HybridSearch
	// computes the embedding per-KB (graceful degradation).
	kbIDs := make([]string, 0, len(chatManage.SearchTargets))
	for _, t := range chatManage.SearchTargets {
		kbIDs = append(kbIDs, t.KnowledgeBaseID)
	}
	var kbList []*types.KnowledgeBase
	kbMap := make(map[string]*types.KnowledgeBase)
	if kbs, err := p.knowledgeBaseService.GetKnowledgeBasesByIDsOnly(ctx, kbIDs); err == nil {
		kbList = kbs
		for _, kb := range kbs {
			if kb != nil {
				kbMap[kb.ID] = kb
			}
		}
	} else {
		pipelineWarn(ctx, "Search", "batch_kb_fetch_error", map[string]interface{}{
			"error": err.Error(),
		})
		errs.add("batch fetch knowledge bases", err)
	}

	// Resolve actual model identities (name + endpoint) so that cross-tenant
	// KBs backed by the same physical model share one embedding computation.
	modelKeyMap := p.knowledgeBaseService.ResolveEmbeddingModelKeys(ctx, kbList)

	groups := make(map[string][]*types.SearchTarget)
	for _, t := range chatManage.SearchTargets {
		key := modelKeyMap[t.KnowledgeBaseID] // empty string if unresolved
		groups[key] = append(groups[key], t)
	}

	pipelineInfo(ctx, "Search", "embedding_groups", map[string]interface{}{
		"total_targets": len(chatManage.SearchTargets),
		"unique_models": len(groups),
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []*types.SearchResult

	for modelKey, targets := range groups {
		wg.Add(1)
		go func(modelKey string, targets []*types.SearchTarget) {
			defer wg.Done()

			// Compute embedding once for this model group when not provided by
			// the caller and vector matching is enabled.
			var localEmbedding []float32
			if !disableVector {
				if len(queryEmbedding) > 0 {
					localEmbedding = queryEmbedding
				} else if modelKey != "" {
					emb, err := p.knowledgeBaseService.GetQueryEmbedding(ctx, targets[0].KnowledgeBaseID, queryText)
					if err != nil {
						pipelineWarn(ctx, "Search", "group_embed_error", map[string]interface{}{
							"model_key": modelKey,
							"kb_id":     targets[0].KnowledgeBaseID,
							"error":     err.Error(),
						})
						errs.add("query embedding", err)
					} else {
						localEmbedding = emb
					}
				}
			}

			// Separate full-KB targets (can be combined into one retrieval)
			// from specific-knowledge targets (need per-target direct loading).
			var fullKBIDs []string
			var knowledgeTargets []*types.SearchTarget
			for _, t := range targets {
				if t.Type == types.SearchTargetTypeKnowledgeBase && len(t.TagIDs) == 0 {
					fullKBIDs = append(fullKBIDs, t.KnowledgeBaseID)
				} else {
					knowledgeTargets = append(knowledgeTargets, t)
				}
			}

			pipelineInfo(ctx, "Search", "group_plan", map[string]interface{}{
				"model_key":          modelKey,
				"combined_kb_count":  len(fullKBIDs),
				"individual_targets": len(knowledgeTargets),
				"vector_len":         len(localEmbedding),
			})

			var innerWg sync.WaitGroup

			// Combined search: one HybridSearch call spanning all full-KB targets
			if len(fullKBIDs) > 0 {
				innerWg.Add(1)
				go func() {
					defer innerWg.Done()

					params := types.SearchParams{
						QueryText:             queryText,
						QueryEmbedding:        localEmbedding,
						KnowledgeBaseIDs:      fullKBIDs,
						VectorThreshold:       chatManage.VectorThreshold,
						KeywordThreshold:      chatManage.KeywordThreshold,
						MatchCount:            chatManage.EmbeddingTopK,
						SkipContextEnrichment: true,
						DisableVectorMatch:    disableVector,
						DisableKeywordsMatch:  disableKeyword,
					}
					res, err := p.knowledgeBaseService.HybridSearch(ctx, fullKBIDs[0], params)
					if err != nil {
						pipelineWarn(ctx, "Search", "combined_kb_search_error", map[string]interface{}{
							"kb_ids": fullKBIDs,
							"error":  err.Error(),
						})
						errs.add(fmt.Sprintf("hybrid search kb=%v", fullKBIDs), err)
						// 部分失败：仍然把 res 里已拿到的结果并入（契约要求
						// “保留成功腿 + 附带错误”），不因一条腿失败丢弃全部。
						if len(res) > 0 {
							mu.Lock()
							results = append(results, res...)
							mu.Unlock()
						}
						return
					}
					pipelineInfo(ctx, "Search", "combined_kb_result", map[string]interface{}{
						"kb_ids":    fullKBIDs,
						"hit_count": len(res),
					})
					mu.Lock()
					results = append(results, res...)
					mu.Unlock()
				}()
			}

			// Individual search: per-target handling for specific-knowledge targets
			for _, target := range knowledgeTargets {
				innerWg.Add(1)
				go func(t *types.SearchTarget) {
					defer innerWg.Done()
					p.searchSingleTarget(ctx, chatManage, t, queryText, localEmbedding, disableVector, disableKeyword, &mu, &results, &errs)
				}(target)
			}

			innerWg.Wait()
		}(modelKey, targets)
	}

	wg.Wait()

	pipelineInfo(ctx, "Search", "kb_result_summary", map[string]interface{}{
		"total_hits":  len(results),
		"error_count": len(errs.errs),
	})
	return results, errs.join()
}

// searchSingleTarget performs hybrid retrieval inside one constrained target.
func (p *PluginSearch) searchSingleTarget(
	ctx context.Context,
	chatManage *types.ChatManage,
	t *types.SearchTarget,
	queryText string,
	queryEmbedding []float32,
	disableVector bool,
	disableKeyword bool,
	mu *sync.Mutex,
	results *[]*types.SearchResult,
	errs *searchErrorCollector,
) {
	if t.Type == types.SearchTargetTypeKnowledge && len(t.KnowledgeIDs) == 0 {
		return
	}

	vectorThreshold, keywordThreshold := t.RecallThresholds(
		chatManage.VectorThreshold,
		chatManage.KeywordThreshold,
	)
	if t.DisableRecallThresholds {
		pipelineInfo(ctx, "Search", "explicit_scope_threshold_override", map[string]interface{}{
			"kb_id":              t.KnowledgeBaseID,
			"knowledge_id_count": len(t.KnowledgeIDs),
			"tag_id_count":       len(t.TagIDs),
		})
	}
	params := types.SearchParams{
		QueryText:             queryText,
		QueryEmbedding:        queryEmbedding,
		VectorThreshold:       vectorThreshold,
		KeywordThreshold:      keywordThreshold,
		MatchCount:            chatManage.EmbeddingTopK,
		TagIDs:                t.TagIDs,
		ScopeTagIDs:           t.ScopeTagIDs,
		SkipContextEnrichment: true,
		DisableVectorMatch:    disableVector,
		DisableKeywordsMatch:  disableKeyword,
	}
	if t.Type == types.SearchTargetTypeKnowledge {
		params.KnowledgeIDs = t.KnowledgeIDs
	}
	res, err := p.knowledgeBaseService.HybridSearch(ctx, t.KnowledgeBaseID, params)
	if err != nil {
		pipelineWarn(ctx, "Search", "kb_search_error", map[string]interface{}{
			"kb_id":       t.KnowledgeBaseID,
			"target_type": t.Type,
			"query":       params.QueryText,
			"error":       err.Error(),
		})
		errs.add(fmt.Sprintf("hybrid search kb=%s", t.KnowledgeBaseID), err)
		// 与合并检索路径一致：失败腿已取回的结果仍然保留。
		if len(res) > 0 {
			mu.Lock()
			*results = append(*results, res...)
			mu.Unlock()
		}
		return
	}
	pipelineInfo(ctx, "Search", "kb_result", map[string]interface{}{
		"kb_id":       t.KnowledgeBaseID,
		"target_type": t.Type,
		"hit_count":   len(res),
	})
	mu.Lock()
	*results = append(*results, res...)
	mu.Unlock()
}

// searchWebIfEnabled executes web search when enabled and returns converted results
func (p *PluginSearch) searchWebIfEnabled(ctx context.Context, chatManage *types.ChatManage) []*types.SearchResult {
	if !chatManage.WebSearchEnabled || p.webSearchService == nil || p.tenantService == nil {
		return nil
	}
	tenant, _ := types.TenantInfoFromContext(ctx)
	providerID := chatManage.WebSearchProviderID

	if providerID == "" {
		pipelineWarn(ctx, "Search", "web_config_missing", map[string]interface{}{
			"tenant_id": chatManage.TenantID,
		})
		return nil
	}

	webConfig := types.EffectiveWebSearchConfig(nil)
	if tenant != nil {
		webConfig = types.EffectiveWebSearchConfig(tenant.WebSearchConfig)
	}

	// Apply agent-level web search overrides
	if chatManage.WebSearchMaxResults > 0 {
		webConfig.MaxResults = chatManage.WebSearchMaxResults
	}

	pipelineInfo(ctx, "Search", "web_request", map[string]interface{}{
		"tenant_id":   chatManage.TenantID,
		"provider_id": providerID,
	})
	webCtx, webSpan := langfuse.GetManager().StartSpan(ctx, langfuse.SpanOptions{
		Name: "web_search",
		Input: map[string]interface{}{
			"provider_id": providerID,
			"query":       chatManage.RewriteQuery,
			"max_results": webConfig.MaxResults,
		},
	})
	webResults, err := p.webSearchService.Search(webCtx, providerID, webConfig, chatManage.RewriteQuery)
	webSpan.Finish(map[string]interface{}{
		"hit_count": len(webResults),
	}, nil, err)
	if err != nil {
		pipelineWarn(ctx, "Search", "web_search_error", map[string]interface{}{
			"tenant_id": chatManage.TenantID,
			"error":     err.Error(),
		})
		return nil
	}
	// Build questions using RewriteQuery only
	// questions := []string{strings.TrimSpace(chatManage.RewriteQuery)}
	// Load session-scoped temp KB state from Redis using WebSearchStateRepository
	// tempKBID, seen, ids := p.webSearchStateService.GetWebSearchTempKBState(ctx, chatManage.SessionID)
	// compressed, kbID, newSeen, newIDs, err := p.webSearchService.CompressWithRAG(
	// 	ctx, chatManage.SessionID, tempKBID, questions, webResults, webConfig,
	// 	p.knowledgeBaseService, p.knowledgeService, seen, ids,
	// )
	// if err != nil {
	// 	pipelineWarn(ctx, "Search", "web_compress_error", map[string]interface{}{
	// 		"error": err.Error(),
	// 	})
	// } else {
	// 	webResults = compressed
	// 	// Persist temp KB state back into Redis using WebSearchStateRepository
	// 	p.webSearchStateService.SaveWebSearchTempKBState(ctx, chatManage.SessionID, kbID, newSeen, newIDs)
	// }
	res := searchutil.ConvertWebSearchResults(webResults)
	pipelineInfo(ctx, "Search", "web_hits", map[string]interface{}{
		"hit_count": len(res),
	})
	return res
}
