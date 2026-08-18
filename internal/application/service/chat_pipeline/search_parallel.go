package chatpipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginSearchParallel implements parallel multi-channel retrieval with
// weighted RRF fusion (section 2.3 + 2.4 of the retrieval optimization).
//
// When RetrievalPlan is present the stage fans out into these channels:
//   - vector       : dense embedding search on the rewritten query
//   - keyword      : lexical search on the rewritten query
//   - hyde         : dense embedding search on the hypothetical answer
//   - multi_query  : hybrid search per LLM-expanded query variant
//   - step_back    : hybrid search on the abstracted step-back query
//   - sparse       : keyword search over LLM/rule-expanded term sets
//   - graph        : entity / graph recall
//   - comparison_* : per-entity hybrid search for comparison queries
//
// Each channel produces an independent ranked list. The lists are fused
// with weighted RRF using plan.ChannelWeights. When no plan is present the
// stage falls back to the legacy behavior (chunk search + entity search).
type PluginSearchParallel struct {
	// Chunk search dependencies
	knowledgeBaseService interfaces.KnowledgeBaseService
	knowledgeService     interfaces.KnowledgeService
	config               *config.Config
	webSearchService     interfaces.WebSearchService
	tenantService        interfaces.TenantService
	sessionService       interfaces.SessionService

	// Entity search dependencies
	graphRepo     interfaces.RetrieveGraphRepository
	chunkRepo     interfaces.ChunkRepository
	knowledgeRepo interfaces.KnowledgeRepository

	// Internal plugins
	searchPlugin       *PluginSearch
	searchEntityPlugin *PluginSearchEntity
}

// NewPluginSearchParallel creates a new parallel search plugin
func NewPluginSearchParallel(
	eventManager *EventManager,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	config *config.Config,
	webSearchService interfaces.WebSearchService,
	tenantService interfaces.TenantService,
	sessionService interfaces.SessionService,
	webSearchStateService interfaces.WebSearchStateService,
	webSearchProviderRepo interfaces.WebSearchProviderRepository,
	graphRepository interfaces.RetrieveGraphRepository,
	chunkRepository interfaces.ChunkRepository,
	knowledgeRepository interfaces.KnowledgeRepository,
) *PluginSearchParallel {
	// Create internal plugins without registering them
	searchPlugin := &PluginSearch{
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

	searchEntityPlugin := &PluginSearchEntity{
		graphRepo:     graphRepository,
		chunkRepo:     chunkRepository,
		knowledgeRepo: knowledgeRepository,
	}

	res := &PluginSearchParallel{
		knowledgeBaseService: knowledgeBaseService,
		knowledgeService:     knowledgeService,
		config:               config,
		webSearchService:     webSearchService,
		tenantService:        tenantService,
		sessionService:       sessionService,
		graphRepo:            graphRepository,
		chunkRepo:            chunkRepository,
		knowledgeRepo:        knowledgeRepository,
		searchPlugin:         searchPlugin,
		searchEntityPlugin:   searchEntityPlugin,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginSearchParallel) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHUNK_SEARCH_PARALLEL}
}

// OnEvent handles parallel search events.
func (p *PluginSearchParallel) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	// Intent-based skip: query-understand step determined KB retrieval is unnecessary
	if !chatManage.NeedsRetrieval() {
		pipelineInfo(ctx, "SearchParallel", "skip", map[string]interface{}{
			"session_id": chatManage.SessionID,
			"reason":     "intent_no_search",
		})
		return next()
	}

	pipelineInfo(ctx, "SearchParallel", "start", map[string]interface{}{
		"session_id":    chatManage.SessionID,
		"has_entities":  len(chatManage.Entity) > 0,
		"rewrite_query": chatManage.RewriteQuery,
		"has_plan":      chatManage.Plan != nil,
	})

	// Legacy fast path: no plan → keep the old chunk+entity behavior.
	if chatManage.Plan == nil {
		return p.legacySearch(ctx, chatManage, next)
	}

	return p.multiChannelSearch(ctx, chatManage, next)
}

// legacySearch preserves the original two-way parallel search (chunk + entity)
// with simple concat + dedup. Used when intent routing is disabled.
func (p *PluginSearchParallel) legacySearch(ctx context.Context,
	chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	// Deep-copy to avoid concurrent read/write on shared slice fields
	chunkCM := chatManage.Clone()
	chunkCM.SearchResult = nil
	entityCM := chatManage.Clone()
	entityCM.SearchResult = nil

	noop := func() *PluginError { return nil }

	tasks := []ParallelTask{
		{
			Name: "chunk_search",
			Run: func() *PluginError {
				err := p.searchPlugin.OnEvent(ctx, types.CHUNK_SEARCH, chunkCM, noop)
				pipelineInfo(ctx, "SearchParallel", "chunk_search_done", map[string]interface{}{
					"result_count": len(chunkCM.SearchResult),
					"has_error":    err != nil && err != ErrSearchNothing,
				})
				if err == ErrSearchNothing {
					return nil
				}
				return err
			},
		},
		{
			Name: "entity_search",
			Run: func() *PluginError {
				if len(chatManage.Entity) == 0 {
					pipelineInfo(ctx, "SearchParallel", "entity_search_skip", map[string]interface{}{
						"reason": "no_entities",
					})
					return nil
				}
				err := p.searchEntityPlugin.OnEvent(ctx, types.ENTITY_SEARCH, entityCM, noop)
				pipelineInfo(ctx, "SearchParallel", "entity_search_done", map[string]interface{}{
					"result_count": len(entityCM.SearchResult),
					"has_error":    err != nil && err != ErrSearchNothing,
				})
				if err == ErrSearchNothing {
					return nil
				}
				return err
			},
		},
	}

	errs := RunParallel(tasks...)

	// Merge results from both searches
	chatManage.SearchResult = append(chunkCM.SearchResult, entityCM.SearchResult...)
	chatManage.SearchResult = removeDuplicateResults(chatManage.SearchResult)

	for name, err := range errs {
		logger.Warnf(ctx, "[SearchParallel] %s error: %v", name, err.Err)
	}

	pipelineInfo(ctx, "SearchParallel", "complete", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"chunk_results":  len(chunkCM.SearchResult),
		"entity_results": len(entityCM.SearchResult),
		"total_results":  len(chatManage.SearchResult),
		"error_count":    len(errs),
		"mode":           "legacy",
	})

	if len(chatManage.SearchResult) == 0 {
		if err, ok := errs["chunk_search"]; ok {
			return err
		}
		return ErrSearchNothing
	}

	return next()
}

// multiChannelSearch runs the plan-driven multi-channel retrieval and fuses
// the per-channel ranked lists with weighted RRF.
func (p *PluginSearchParallel) multiChannelSearch(ctx context.Context,
	chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	plan := chatManage.Plan
	queryText := strings.TrimSpace(chatManage.RewriteQuery)
	if queryText == "" {
		queryText = strings.TrimSpace(chatManage.Query)
	}

	// Pre-compute embeddings for the queries that need them. We compute at
	// most once per unique text to avoid duplicate embedding API calls.
	embeddingCache := make(map[string][]float32)
	embed := func(text string) []float32 {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		if emb, ok := embeddingCache[text]; ok {
			return emb
		}
		// Use the first available KB for embedding model resolution.
		kbIDs := chatManage.SearchTargets.GetAllKnowledgeBaseIDs()
		if len(kbIDs) == 0 {
			kbIDs = chatManage.KnowledgeBaseIDs
		}
		if len(kbIDs) == 0 {
			return nil
		}
		emb, err := p.knowledgeBaseService.GetQueryEmbedding(ctx, kbIDs[0], text)
		if err != nil {
			pipelineWarn(ctx, "SearchParallel", "embed_error", map[string]interface{}{
				"query": text,
				"error": err.Error(),
			})
			return nil
		}
		embeddingCache[text] = emb
		return emb
	}

	// Build channel tasks.
	type channelTask struct {
		channel string
		weight  float64
		run     func() []*types.SearchResult
	}
	tasks := make([]channelTask, 0, 8)

	// 1. Vector channel (dense embedding on rewritten query)
	if w := plan.ChannelWeight(types.ChannelVector); w > 0 {
		tasks = append(tasks, channelTask{
			channel: types.ChannelVector,
			weight:  w,
			run: func() []*types.SearchResult {
				return p.searchPlugin.SearchChannel(ctx, chatManage, queryText, embed(queryText), false, true)
			},
		})
	}

	// 2. Keyword channel (lexical on rewritten query)
	if w := plan.ChannelWeight(types.ChannelKeyword); w > 0 {
		tasks = append(tasks, channelTask{
			channel: types.ChannelKeyword,
			weight:  w,
			run: func() []*types.SearchResult {
				return p.searchPlugin.SearchChannel(ctx, chatManage, queryText, nil, true, false)
			},
		})
	}

	// 3. HyDE channel (dense embedding on hypothetical answer)
	if plan.UseHyDE && chatManage.HydeDocument != "" {
		if w := plan.ChannelWeight(types.ChannelHyde); w > 0 {
			tasks = append(tasks, channelTask{
				channel: types.ChannelHyde,
				weight:  w,
				run: func() []*types.SearchResult {
					return p.searchPlugin.SearchChannel(ctx, chatManage, chatManage.HydeDocument, embed(chatManage.HydeDocument), false, true)
				},
			})
		}
	}

	// 4. Multi-query expansion channel (hybrid per variant)
	if plan.UseMultiQuery && len(chatManage.EnhancedQueries) > 0 {
		if w := plan.ChannelWeight(types.ChannelMultiQuery); w > 0 {
			for _, q := range chatManage.EnhancedQueries {
				q = strings.TrimSpace(q)
				if q == "" || q == queryText {
					continue
				}
				query := q
				tasks = append(tasks, channelTask{
					channel: types.ChannelMultiQuery,
					weight:  w,
					run: func() []*types.SearchResult {
						return p.searchPlugin.SearchChannel(ctx, chatManage, query, embed(query), false, false)
					},
				})
			}
		}
	}

	// 5. Step-back channel (hybrid on abstracted query)
	if plan.UseStepBack && chatManage.StepBackQuery != "" {
		if w := plan.ChannelWeight(types.ChannelStepBack); w > 0 {
			tasks = append(tasks, channelTask{
				channel: types.ChannelStepBack,
				weight:  w,
				run: func() []*types.SearchResult {
					return p.searchPlugin.SearchChannel(ctx, chatManage, chatManage.StepBackQuery, embed(chatManage.StepBackQuery), false, false)
				},
			})
		}
	}

	// 6. Sparse channel (keyword search over expanded term set)
	if plan.UseSparse {
		if w := plan.ChannelWeight(types.ChannelSparse); w > 0 {
			tasks = append(tasks, channelTask{
				channel: types.ChannelSparse,
				weight:  w,
				run: func() []*types.SearchResult {
					expanded := expandQueryTerms(queryText)
					return p.searchPlugin.SearchChannel(ctx, chatManage, expanded, nil, true, false)
				},
			})
		}
	}

	// 7. Graph channel (entity / graph recall)
	if plan.UseGraphChannel && len(chatManage.Entity) > 0 {
		if w := plan.ChannelWeight(types.ChannelGraph); w > 0 {
			tasks = append(tasks, channelTask{
				channel: types.ChannelGraph,
				weight:  w,
				run: func() []*types.SearchResult {
					entityCM := chatManage.Clone()
					entityCM.SearchResult = nil
					noop := func() *PluginError { return nil }
					if err := p.searchEntityPlugin.OnEvent(ctx, types.ENTITY_SEARCH, entityCM, noop); err != nil && err != ErrSearchNothing {
						pipelineWarn(ctx, "SearchParallel", "graph_channel_error", map[string]interface{}{
							"error": err.Err.Error(),
						})
						return nil
					}
					return entityCM.SearchResult
				},
			})
		}
	}

	// 8. Comparison decomposition channels (per-entity hybrid search)
	if plan.UseDecompose && len(plan.CompareEntities) >= 2 {
		for i, entity := range plan.CompareEntities {
			entity = strings.TrimSpace(entity)
			if entity == "" {
				continue
			}
			channelName := fmt.Sprintf("%s_%d", types.ChannelMultiQuery, i)
			if w := plan.ChannelWeight(types.ChannelMultiQuery); w > 0 {
				tasks = append(tasks, channelTask{
					channel: channelName,
					weight:  w,
					run: func() []*types.SearchResult {
						return p.searchPlugin.SearchChannel(ctx, chatManage, entity, embed(entity), false, false)
					},
				})
			}
		}
	}

	// Execute all channels in parallel.
	parallelTasks := make([]ParallelTask, 0, len(tasks))
	channelResults := make(map[string][]*types.SearchResult)
	var resultMu sync.Mutex

	for _, t := range tasks {
		t := t
		parallelTasks = append(parallelTasks, ParallelTask{
			Name: "channel_" + t.channel,
			Run: func() *PluginError {
				res := t.run()
				resultMu.Lock()
				channelResults[t.channel] = res
				resultMu.Unlock()
				pipelineInfo(ctx, "SearchParallel", "channel_done", map[string]interface{}{
					"channel":      t.channel,
					"result_count": len(res),
				})
				return nil
			},
		})
	}

	errs := RunParallel(parallelTasks...)
	for name, err := range errs {
		logger.Warnf(ctx, "[SearchParallel] %s error: %v", name, err.Err)
	}

	// Store per-channel results for observability and downstream stages.
	chatManage.ChannelResults = channelResults

	// Build weighted lists for fusion.
	lists := make([]channelRankedList, 0, len(tasks))
	for _, t := range tasks {
		if res, ok := channelResults[t.channel]; ok && len(res) > 0 {
			lists = append(lists, channelRankedList{
				Channel: t.channel,
				Weight:  t.weight,
				Results: res,
			})
		}
	}

	// Weighted RRF fusion.
	fused := fuseChannelsWithWeightedRRF(ctx, lists, 60)

	// Apply document-level evidence boost when enabled (e.g. summary intent).
	if plan.UseDocBoost {
		fused = applyDocumentEvidenceBoost(fused)
	}

	chatManage.SearchResult = fused

	pipelineInfo(ctx, "SearchParallel", "complete", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"channel_count":  len(lists),
		"fused_results":  len(fused),
		"error_count":    len(errs),
		"mode":           "multi_channel",
	})

	if len(chatManage.SearchResult) == 0 {
		return ErrSearchNothing
	}

	return next()
}

// expandQueryTerms generates a lightweight sparse expansion of the query.
// It preserves the original terms and appends morphological variants and
// high-frequency co-occurring terms derived from simple rules. This is the
// cheap stand-in for SPLADE/BM42 until a dedicated sparse encoder is wired in.
func expandQueryTerms(query string) string {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return query
	}
	seen := make(map[string]struct{})
	var out []string
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
		// Simple rule-based expansion: append common suffix/prefix variants.
		// This is intentionally conservative — the goal is to slightly broaden
		// lexical recall without exploding the term set.
		variants := []string{
			t + "s",
			t + "es",
			strings.TrimSuffix(t, "s"),
			strings.TrimSuffix(t, "ing"),
			strings.TrimSuffix(t, "ed"),
		}
		for _, v := range variants {
			v = strings.TrimSpace(v)
			if v == "" || v == t {
				continue
			}
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				out = append(out, v)
			}
		}
	}
	return strings.Join(out, " ")
}

// applyDocumentEvidenceBoost boosts chunks that belong to documents with
// multiple high-ranking chunks, improving cross-chunk coherence for summary
// and reasoning intents.
func applyDocumentEvidenceBoost(results []*types.SearchResult) []*types.SearchResult {
	if len(results) <= 1 {
		return results
	}
	// Count how many chunks each knowledge document contributes.
	docCounts := make(map[string]int)
	for _, r := range results {
		if r.KnowledgeID != "" {
			docCounts[r.KnowledgeID]++
		}
	}
	// Boost score proportionally to document frequency (capped).
	for _, r := range results {
		if r.KnowledgeID == "" {
			continue
		}
		cnt := docCounts[r.KnowledgeID]
		if cnt > 1 {
			boost := 1.0 + 0.05*float64(cnt-1)
			if boost > 1.25 {
				boost = 1.25
			}
			r.Score *= boost
			r.Metadata = ensureMetadata(r.Metadata)
			r.Metadata["doc_evidence_boost"] = fmt.Sprintf("%.2f", boost)
		}
	}
	// Re-sort by boosted score.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results
}
