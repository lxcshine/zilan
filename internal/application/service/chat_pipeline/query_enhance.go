package chatpipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginQueryEnhance implements section 2.2 of the retrieval optimization
// design: LLM-driven query enhancement between routing and search.
//
// Three techniques, individually gated by the RetrievalPlan resolved in
// ROUTE_RETRIEVAL (which in turn follows the per-intent profile):
//
//   - HyDE (UseHyDE): generate one hypothetical answer passage for the query.
//     Its embedding is closer to real answer chunks than the short query
//     embedding, lifting semantic recall on summary/reasoning/exploratory
//     intents. Consumed by the search stage as ChannelHyde (vector-only).
//   - Multi-query expansion (UseMultiQuery): generate 3 semantic variants of
//     the query (different vocabulary / angle). Searched in parallel and
//     fused as ChannelMultiQuery. Also used to materialize comparison
//     decomposition sub-queries (one per A/B entity).
//   - Step-back prompting (UseStepBack): generate one abstracted "bigger
//     picture" question that retrieves macro concepts; fused as
//     ChannelStepBack.
//
// Cost discipline: at most three small LLM calls per request, run
// concurrently, only when the intent profile asks for them, and every call
// has a hard token cap with Thinking disabled. Any failure degrades silently
// to the base retrieval path — enhancement must never break a chat turn.
type PluginQueryEnhance struct {
	modelService interfaces.ModelService
	cfgProvider  retrievalConfigProvider
}

// NewPluginQueryEnhance creates and registers the query-enhancement plugin.
func NewPluginQueryEnhance(eventManager *EventManager,
	modelService interfaces.ModelService,
) *PluginQueryEnhance {
	res := &PluginQueryEnhance{
		modelService: modelService,
		cfgProvider: func(ctx context.Context) *types.RetrievalConfig {
			tenant, ok := types.TenantInfoFromContext(ctx)
			if !ok || tenant == nil {
				return nil
			}
			return tenant.RetrievalConfig
		},
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles.
func (p *PluginQueryEnhance) ActivationEvents() []types.EventType {
	return []types.EventType{types.QUERY_ENHANCE}
}

// Enhancement output limits. HyDE passages must stay short (they are
// embedded whole); variants stay one line each.
const (
	hydeMaxTokens       = 320
	multiQueryMaxTokens = 220
	stepBackMaxTokens   = 120
	maxEnhancedQueries  = 4
	maxHydeRunes        = 1200
)

// OnEvent runs the enabled enhancement techniques for the current plan.
func (p *PluginQueryEnhance) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	plan := chatManage.Plan
	if plan == nil || !chatManage.NeedsRetrieval() {
		return next()
	}
	if cfg := p.cfgProvider(ctx); !cfg.IsQueryEnhancementEnabled() {
		return next()
	}

	query := strings.TrimSpace(chatManage.RewriteQuery)
	if query == "" {
		query = strings.TrimSpace(chatManage.Query)
	}
	if query == "" {
		return next()
	}

	wantHyde := plan.UseHyDE
	wantMulti := plan.UseMultiQuery
	wantStepBack := plan.UseStepBack
	wantDecompose := plan.UseDecompose && len(plan.CompareEntities) >= 2
	if !wantHyde && !wantMulti && !wantStepBack && !wantDecompose {
		return next()
	}

	chatModel := p.resolveModel(ctx, chatManage)
	if chatModel == nil {
		// No model available — degrade to base retrieval rather than failing.
		return next()
	}

	tasks := make([]ParallelTask, 0, 4)

	if wantHyde {
		tasks = append(tasks, ParallelTask{
			Name: "hyde",
			Run: func() *PluginError {
				doc := p.generateHyDE(ctx, chatModel, query, chatManage.Language)
				if doc != "" {
					chatManage.HydeDocument = doc
				}
				return nil
			},
		})
	}

	if wantMulti {
		tasks = append(tasks, ParallelTask{
			Name: "multi_query",
			Run: func() *PluginError {
				variants := p.generateMultiQueries(ctx, chatModel, query, chatManage.Language)
				chatManage.EnhancedQueries = mergeQueryVariants(chatManage.EnhancedQueries, variants, query)
				return nil
			},
		})
	}

	if wantStepBack {
		tasks = append(tasks, ParallelTask{
			Name: "step_back",
			Run: func() *PluginError {
				sb := p.generateStepBack(ctx, chatModel, query, chatManage.Language)
				if sb != "" && !strings.EqualFold(sb, query) {
					chatManage.StepBackQuery = sb
				}
				return nil
			},
		})
	}

	if wantDecompose {
		tasks = append(tasks, ParallelTask{
			Name: "comparison_decompose",
			Run: func() *PluginError {
				subs := buildComparisonSubQueries(query, plan.CompareEntities)
				chatManage.EnhancedQueries = mergeQueryVariants(chatManage.EnhancedQueries, subs, query)
				return nil
			},
		})
	}

	RunParallel(tasks...)

	if len(chatManage.EnhancedQueries) > maxEnhancedQueries {
		chatManage.EnhancedQueries = chatManage.EnhancedQueries[:maxEnhancedQueries]
	}

	pipelineInfo(ctx, "QueryEnhance", "output", map[string]interface{}{
		"session_id":       chatManage.SessionID,
		"has_hyde":         chatManage.HydeDocument != "",
		"enhanced_queries": len(chatManage.EnhancedQueries),
		"step_back":        chatManage.StepBackQuery != "",
		"intent":           plan.Intent,
	})
	return next()
}

// resolveModel picks the cheap query-understanding model when configured,
// falling back to the chat model. Returns nil when neither resolves.
func (p *PluginQueryEnhance) resolveModel(ctx context.Context, chatManage *types.ChatManage) chat.Chat {
	modelID := chatManage.QueryUnderstandModelID
	if modelID == "" {
		modelID = chatManage.ChatModelID
	}
	m, err := p.modelService.GetChatModel(ctx, modelID)
	if err != nil {
		pipelineWarn(ctx, "QueryEnhance", "get_model", map[string]interface{}{
			"model_id": modelID,
			"error":    err.Error(),
		})
		return nil
	}
	return m
}

// callEnhancementModel issues one small non-streaming completion. Thinking is
// force-disabled — these are mechanical transformations, not reasoning tasks.
func (p *PluginQueryEnhance) callEnhancementModel(
	ctx context.Context, chatModel chat.Chat, purpose, systemPrompt, userPrompt string, maxTokens int, temperature float64,
) (string, error) {
	thinking := false
	modelCtx := types.WithLLMCallMetadata(ctx, purpose, "")
	resp, err := chatModel.Chat(modelCtx, []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, &chat.ChatOptions{
		Temperature:         temperature,
		MaxCompletionTokens: maxTokens,
		Thinking:            &thinking,
	})
	if err != nil {
		pipelineWarn(ctx, "QueryEnhance", purpose, map[string]interface{}{
			"error": err.Error(),
		})
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// generateHyDE writes one short hypothetical answer passage. The passage is
// deliberately assertive (no hedging) — it is an embedding probe, not a
// user-facing answer, so factual accuracy matters less than topical density.
func (p *PluginQueryEnhance) generateHyDE(ctx context.Context, chatModel chat.Chat, query, language string) string {
	system := `You are a retrieval probe writer. Given a user question, write ONE short hypothetical passage that could plausibly appear in a knowledge base and would answer the question.

Rules:
- Write 80-150 words as a factual knowledge-base excerpt (statement style, NOT a question).
- Pack the passage with the domain terms, entities and concepts the real answer would contain.
- Never hedge ("可能", "也许", "I think") and never refuse; invent plausible specifics when needed — the passage only guides retrieval.
- Write in the same language as the question.
- Output ONLY the passage text, no prefixes, no quotes.`
	raw, err := p.callEnhancementModel(ctx, chatModel, "hyde", system, query, hydeMaxTokens, 0.3)
	if err != nil {
		return ""
	}
	// Guard: cap length and drop degenerate echoes of the query.
	runes := []rune(raw)
	if len(runes) > maxHydeRunes {
		raw = string(runes[:maxHydeRunes])
	}
	if len(runes) < 10 || strings.EqualFold(strings.TrimSpace(raw), query) {
		return ""
	}
	return raw
}

// generateMultiQueries produces up to 3 semantic variants of the query.
// Parsing is tolerant: JSON array first, then numbered/bulleted lines.
func (p *PluginQueryEnhance) generateMultiQueries(ctx context.Context, chatModel chat.Chat, query, language string) []string {
	system := `You are a search query expansion engine. Given a user question, generate 3 alternative phrasings that would retrieve the same information from a knowledge base.

Rules:
- Each variant must preserve the full meaning and every key entity of the original.
- Vary the vocabulary and angle (synonyms, hypernyms, different sentence structures), do not just reorder words.
- One variant per line. No numbering, no bullets, no quotes, no explanations.
- Use the same language as the question.`
	raw, err := p.callEnhancementModel(ctx, chatModel, "multi_query", system, query, multiQueryMaxTokens, 0.7)
	if err != nil {
		return nil
	}
	return parseQueryVariants(raw)
}

// generateStepBack produces the abstracted macro-level question.
func (p *PluginQueryEnhance) generateStepBack(ctx context.Context, chatModel chat.Chat, query, language string) string {
	system := `You are a query abstraction engine. Given a specific user question, produce ONE broader "step-back" question that asks about the underlying concept, background or category the specific question belongs to.

Example: "为什么XX项目二期延期了" -> "XX项目的整体进展和常见延期原因是什么"
Example: "What is the battery life of model X200?" -> "What are the specifications of model X200?"

Rules:
- The step-back question must remain useful for retrieving context that helps answer the original.
- Output ONLY the step-back question, nothing else.
- Use the same language as the original question.`
	raw, err := p.callEnhancementModel(ctx, chatModel, "step_back", system, query, stepBackMaxTokens, 0.5)
	if err != nil {
		return ""
	}
	// Single line expected; keep the first non-empty line.
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.Trim(line, `"'“”`))
		if line != "" {
			return line
		}
	}
	return ""
}

// parseQueryVariants extracts query variants from model output, tolerating
// both JSON arrays and plain line-based formats.
func parseQueryVariants(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// JSON array fast path (some models wrap output in fences).
	candidate := strings.TrimPrefix(strings.TrimSuffix(raw, "```"), "```json")
	candidate = strings.Trim(candidate, "` \n")
	var arr []string
	if json.Unmarshal([]byte(candidate), &arr) == nil && len(arr) > 0 {
		return cleanVariantLines(arr)
	}

	return cleanVariantLines(strings.Split(raw, "\n"))
}

// cleanVariantLines normalizes line-based variants: strips numbering/bullets,
// quotes and empty lines.
func cleanVariantLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Strip leading numbering/bullets: "1.", "1)", "- ", "* ", "、".
		line = strings.TrimLeft(line, "-*•、 \t")
		if idx := strings.Index(line, ". "); idx > 0 && idx <= 3 {
			line = line[idx+2:]
		} else if idx := strings.Index(line, ") "); idx > 0 && idx <= 3 {
			line = line[idx+2:]
		}
		line = strings.Trim(strings.TrimSpace(line), `"'“”‘’`)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// mergeQueryVariants appends new variants, deduplicated (case-insensitive)
// against existing entries and the base query, capped at maxEnhancedQueries.
func mergeQueryVariants(existing, variants []string, baseQuery string) []string {
	seen := make(map[string]struct{}, len(existing)+1)
	for _, e := range existing {
		seen[strings.ToLower(e)] = struct{}{}
	}
	seen[strings.ToLower(strings.TrimSpace(baseQuery))] = struct{}{}

	out := append([]string(nil), existing...)
	for _, v := range variants {
		v = strings.TrimSpace(v)
		if len([]rune(v)) < 3 || len([]rune(v)) > 200 {
			continue
		}
		key := strings.ToLower(v)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
		if len(out) >= maxEnhancedQueries {
			break
		}
	}
	return out
}

// buildComparisonSubQueries materializes per-entity sub-queries for "A vs B"
// comparisons without an extra LLM call: the aspect words of the original
// query (everything that is not an entity or comparison scaffolding) are
// appended to each entity, e.g. "对比MySQL和PG的写入性能" yields
// "MySQL 写入性能" / "PG 写入性能".
func buildComparisonSubQueries(query string, entities []string) []string {
	aspects := query
	for _, e := range entities {
		aspects = strings.ReplaceAll(aspects, e, " ")
	}
	aspects = comparisonSignals.ReplaceAllString(aspects, " ")
	aspects = questionWords.ReplaceAllString(aspects, " ")
	aspects = strings.Join(strings.Fields(aspects), " ")
	aspects = strings.Trim(aspects, " 。？?！!，,；;：:")
	if len([]rune(aspects)) > 30 {
		aspects = string([]rune(aspects)[:30])
	}

	subs := make([]string, 0, len(entities))
	for _, e := range entities {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if aspects == "" {
			subs = append(subs, e)
		} else {
			subs = append(subs, fmt.Sprintf("%s %s", e, aspects))
		}
	}
	return subs
}
