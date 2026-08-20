package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hibiken/asynq"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// memoryExtractionTurns bounds how many recent session messages are fed to the
// extraction LLM. The prompt only needs the latest turn plus a little context;
// more turns would mostly cost tokens without improving extraction quality.
const memoryExtractionTurns = 8

// memoryExtractionMaxContentChars truncates a single message before it enters
// the extraction prompt; long RAG answers carry no extra user signal.
const memoryExtractionMaxContentChars = 2000

// defaultMemoryExtractionPrompt is used when the configured template cannot be
// resolved (missing config key). Keep it minimal; the canonical template lives
// in config/prompt_templates/memory_extraction.yaml.
const defaultMemoryExtractionPrompt = `You maintain the long-term memory of an AI assistant.
Analyze the user-provided conversation excerpt and extract memories worth
persisting (categories: profile, fact, preference, todo, feedback, soul,
skill), plus a refreshed rolling session summary. soul = an explicit user
directive about the assistant's own behavior (how to address the user, tone,
format); skill = a behavioral rule distilled from explicit user feedback or
instruction, subject "assistant", confidence >= 0.7 only. When the user
criticizes or prescribes answer behavior, emit BOTH the raw feedback and the
distilled skill. Output JSON only:
{"memories":[{"category":"...","subject":"...","predicate":"...","object":"...","content":"...","confidence":0.0,"importance":0.0,"due_at":""}],"session_summary":"...","key_topics":["..."]}
Never fabricate; at most 5 memories; empty array when nothing is worth remembering.`

// memoryService implements interfaces.MemoryService.
type memoryService struct {
	cfg           *config.Config
	memoryRepo    interfaces.MemoryRepository
	messageRepo   interfaces.MessageRepository
	userRepo      interfaces.UserRepository
	modelService  interfaces.ModelService
	taskEnqueuer  interfaces.TaskEnqueuer
	precipitation interfaces.SessionPrecipitationService
}

// NewMemoryService creates the three-layer memory service.
func NewMemoryService(
	cfg *config.Config,
	memoryRepo interfaces.MemoryRepository,
	messageRepo interfaces.MessageRepository,
	userRepo interfaces.UserRepository,
	modelService interfaces.ModelService,
	taskEnqueuer interfaces.TaskEnqueuer,
	precipitation interfaces.SessionPrecipitationService,
) interfaces.MemoryService {
	return &memoryService{
		cfg:           cfg,
		memoryRepo:    memoryRepo,
		messageRepo:   messageRepo,
		userRepo:      userRepo,
		modelService:  modelService,
		taskEnqueuer:  taskEnqueuer,
		precipitation: precipitation,
	}
}

// ---------------------------------------------------------------------------
// Enqueue (producer side, called from the QA handler after a turn completes)
// ---------------------------------------------------------------------------

// EnqueueMemoryExtract dispatches the async extraction task. Best-effort: any
// failure is logged and swallowed so the QA path never blocks on memory.
func (s *memoryService) EnqueueMemoryExtract(ctx context.Context, payload *types.MemoryExtractPayload) {
	if payload == nil || payload.SessionID == "" || payload.TenantID == 0 {
		return
	}
	if !s.IsEnabled(ctx, payload.UserID) {
		return
	}
	langfuse.InjectTracing(ctx, payload)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf(ctx, "[Memory] marshal extract payload failed for session %s: %v", payload.SessionID, err)
		return
	}
	task := asynq.NewTask(types.TypeMemoryExtract, payloadBytes, memoryExtractTaskOptions()...)
	if s.taskEnqueuer == nil {
		logger.Warnf(ctx, "[Memory] task enqueuer unavailable, dropping memory extract for session %s", payload.SessionID)
		return
	}
	if _, err := s.taskEnqueuer.Enqueue(task); err != nil {
		logger.Warnf(ctx, "[Memory] enqueue extract task failed for session %s: %v", payload.SessionID, err)
		return
	}
	logger.Debugf(ctx, "[Memory] enqueued extract task session=%s user_msg=%s assistant_msg=%s",
		payload.SessionID, payload.UserMessageID, payload.AssistantMessageID)
}

// memoryExtractTaskOptions keeps extraction off the interactive path: the
// post-process queue already carries non-latency-critical background work.
// Retry is limited — a malformed turn must not loop forever.
func memoryExtractTaskOptions() []asynq.Option {
	return []asynq.Option{
		asynq.Queue(types.QueuePostProcess),
		asynq.MaxRetry(2),
		asynq.Timeout(5 * time.Minute),
	}
}

// ---------------------------------------------------------------------------
// ProcessMemoryExtract (worker side)
// ---------------------------------------------------------------------------

// ProcessMemoryExtract is the asynq handler for types.TypeMemoryExtract.
func (s *memoryService) ProcessMemoryExtract(ctx context.Context, task *asynq.Task) error {
	var payload types.MemoryExtractPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal memory extract payload: %w", err)
	}

	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.UserID != "" {
		ctx = context.WithValue(ctx, types.UserIDContextKey, payload.UserID)
	}

	if !s.IsEnabled(ctx, payload.UserID) {
		logger.Debugf(ctx, "[Memory] memory disabled for user %q, skipping extraction", payload.UserID)
		return nil
	}

	// 1. Load the recent conversation excerpt.
	messages, err := s.messageRepo.GetRecentMessagesBySession(ctx, payload.SessionID, memoryExtractionTurns)
	if err != nil {
		return fmt.Errorf("load session messages: %w", err)
	}
	if len(messages) == 0 {
		return nil
	}

	// 2. Load the previous L2 summary so the LLM can merge instead of restart.
	prevSummary := s.loadPreviousSummary(ctx, &payload)

	// 3. Resolve models. Extraction failing to resolve a chat model is not
	//    retryable-worthy: log and drop (returning nil acks the task).
	chatModel, err := s.resolveChatModel(ctx, payload.SummaryModelID)
	if err != nil {
		logger.Warnf(ctx, "[Memory] resolve chat model failed (session %s): %v", payload.SessionID, err)
		return nil
	}
	embedder := s.resolveEmbedder(ctx, payload.EmbeddingModelID) // nil-tolerant

	// 4. Run the extraction LLM call.
	result, err := s.runExtraction(ctx, chatModel, messages, prevSummary)
	if err != nil {
		// Transient model errors are worth one retry via asynq.
		return fmt.Errorf("memory extraction LLM call: %w", err)
	}
	if result == nil {
		return nil
	}

	// 5. Embed new/updated contents in one batch (facts + summary).
	factEmbeddings, summaryEmbedding := s.embedExtraction(ctx, embedder, result)

	// 6. Upsert L3 facts, then refresh the L2 rolling summary.
	upserted := s.upsertExtractedFacts(ctx, &payload, result.Memories, factEmbeddings)
	if strings.TrimSpace(result.SessionSummary) != "" {
		s.refreshSessionSummary(ctx, &payload, messages, result, summaryEmbedding)
	}

	// 7. Enforce the per-user cap so recall stays a cheap in-Go rescore.
	s.enforceFactCap(ctx, payload.TenantID, payload.UserID)

	// 8. 知识沉淀 (4.4): high-value sessions (favorited or sustained
	//    follow-ups) get distilled into the tenant's precipitation KB.
	//    Enqueue is best-effort and deduplicated by a uniqueness TTL.
	if s.precipitation != nil {
		s.precipitation.MaybeEnqueuePrecipitation(ctx, &types.SessionPrecipitatePayload{
			TenantID:       payload.TenantID,
			UserID:         payload.UserID,
			SessionID:      payload.SessionID,
			SummaryModelID: payload.SummaryModelID,
		})
	}

	logger.Infof(ctx, "[Memory] extraction done session=%s facts_upserted=%d summary=%t",
		payload.SessionID, upserted, strings.TrimSpace(result.SessionSummary) != "")
	return nil
}

// loadPreviousSummary returns the current L2 summary text for the session, or
// "" when none exists yet.
func (s *memoryService) loadPreviousSummary(ctx context.Context, payload *types.MemoryExtractPayload) string {
	summary, err := s.memoryRepo.GetSessionSummary(ctx, payload.TenantID, payload.SessionID)
	if err != nil || summary == nil {
		return ""
	}
	return summary.Summary
}

// resolveChatModel picks the extraction chat model: the payload's model first,
// otherwise the first available KnowledgeQA model (same fallback as session
// title generation).
func (s *memoryService) resolveChatModel(ctx context.Context, modelID string) (chat.Chat, error) {
	if modelID == "" {
		models, err := s.modelService.ListModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("list models: %w", err)
		}
		for _, m := range models {
			if m != nil && m.Type == types.ModelTypeKnowledgeQA {
				modelID = m.ID
				break
			}
		}
		if modelID == "" {
			return nil, fmt.Errorf("no KnowledgeQA model available for memory extraction")
		}
	}
	return s.modelService.GetChatModel(ctx, modelID)
}

// resolveEmbedder picks the embedding model; returns nil (without failing the
// task) when none can be resolved — recall then degrades to recency/importance.
func (s *memoryService) resolveEmbedder(ctx context.Context, modelID string) embedding.Embedder {
	if modelID == "" {
		models, err := s.modelService.ListModels(ctx)
		if err != nil {
			return nil
		}
		for _, m := range models {
			if m != nil && m.Type == types.ModelTypeEmbedding {
				modelID = m.ID
				break
			}
		}
		if modelID == "" {
			return nil
		}
	}
	emb, err := s.modelService.GetEmbeddingModel(ctx, modelID)
	if err != nil {
		logger.Warnf(ctx, "[Memory] resolve embedding model %s failed: %v", modelID, err)
		return nil
	}
	return emb
}

// runExtraction renders the prompt, calls the chat model and parses the JSON
// envelope. Returns (nil, nil) when the model returned nothing parseable —
// that turn is simply skipped, no retry.
func (s *memoryService) runExtraction(
	ctx context.Context,
	chatModel chat.Chat,
	messages []*types.Message,
	prevSummary string,
) (*types.MemoryExtractionResult, error) {
	systemPrompt := ""
	if s.cfg != nil && s.cfg.Conversation != nil {
		systemPrompt = strings.TrimSpace(s.cfg.Conversation.MemoryExtractionPrompt)
	}
	if systemPrompt == "" {
		systemPrompt = defaultMemoryExtractionPrompt
	}

	var b strings.Builder
	if prev := strings.TrimSpace(prevSummary); prev != "" {
		b.WriteString("Previous summary: ")
		b.WriteString(prev)
		b.WriteString("\n\n")
	}
	b.WriteString("Conversation:\n")
	for _, m := range messages {
		if m == nil || (m.Role != "user" && m.Role != "assistant") {
			continue
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		if len(content) > memoryExtractionMaxContentChars {
			content = content[:memoryExtractionMaxContentChars] + "…"
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}

	thinking := false
	response, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: b.String()},
	}, &chat.ChatOptions{Temperature: 0.1, Thinking: &thinking})
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nil
	}

	raw := extractJSONObject(agenttools.StripThinkBlocks(response.Content))
	if raw == "" {
		logger.Warnf(ctx, "[Memory] extraction returned no JSON object, skipping turn")
		return nil, nil
	}
	var result types.MemoryExtractionResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		logger.Warnf(ctx, "[Memory] extraction JSON parse failed: %v", err)
		return nil, nil
	}
	return &result, nil
}

// extractJSONObject finds the outermost JSON object in the model output,
// tolerating markdown fences and leading/trailing prose.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// embedExtraction batch-embeds every extracted fact content plus the session
// summary so each stored row carries a vector for semantic recall. Returns one
// embedding per memory item (nil for items with empty content) and the summary
// embedding (nil when no summary or embedding is unavailable).
func (s *memoryService) embedExtraction(
	ctx context.Context, embedder embedding.Embedder, result *types.MemoryExtractionResult,
) ([]types.VectorBlob, types.VectorBlob) {
	if embedder == nil || result == nil {
		return nil, nil
	}
	// Track which item index each text belongs to so the parallel result
	// slices line up even when some items have empty content.
	texts := make([]string, 0, len(result.Memories)+1)
	itemIndexes := make([]int, 0, len(result.Memories))
	for i, m := range result.Memories {
		if c := strings.TrimSpace(m.Content); c != "" {
			texts = append(texts, c)
			itemIndexes = append(itemIndexes, i)
		}
	}
	summaryText := strings.TrimSpace(result.SessionSummary)
	summaryIdx := -1
	if summaryText != "" {
		summaryIdx = len(texts)
		texts = append(texts, summaryText)
	}
	if len(texts) == 0 {
		return nil, nil
	}
	vectors, err := embedder.BatchEmbed(ctx, texts)
	if err != nil || len(vectors) != len(texts) {
		logger.Warnf(ctx, "[Memory] batch embed failed (%d texts): %v", len(texts), err)
		return nil, nil
	}
	factEmbeddings := make([]types.VectorBlob, len(result.Memories))
	for pos, itemIdx := range itemIndexes {
		factEmbeddings[itemIdx] = types.VectorBlob(vectors[pos])
	}
	var summaryEmbedding types.VectorBlob
	if summaryIdx >= 0 {
		summaryEmbedding = types.VectorBlob(vectors[summaryIdx])
	}
	return factEmbeddings, summaryEmbedding
}

// upsertExtractedFacts creates or refreshes L3 facts. Re-extracted duplicates
// (same triple hash) update confidence/importance/content instead of inserting
// a new row.
func (s *memoryService) upsertExtractedFacts(
	ctx context.Context,
	payload *types.MemoryExtractPayload,
	items []types.ExtractedMemoryItem,
	factEmbeddings []types.VectorBlob,
) int {
	upserted := 0
	for i := range items {
		item := &items[i]
		if !validMemoryCategory(item.Category) {
			continue
		}
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		// Skills are high-leverage: a hallucinated "assistant lesson" would
		// steer every future answer, so they require explicit evidence.
		if item.Category == types.MemoryCategorySkill && item.Confidence < 0.7 {
			continue
		}
		// Skills are always assistant-subject so they can never collide with
		// user-fact triple hashes.
		if item.Category == types.MemoryCategorySkill {
			item.Subject = "assistant"
		}
		fact := &types.MemoryFact{
			TenantID:   payload.TenantID,
			UserID:     payload.UserID,
			SessionID:  payload.SessionID,
			MessageID:  payload.UserMessageID,
			Category:   item.Category,
			Subject:    strings.TrimSpace(item.Subject),
			Predicate:  strings.TrimSpace(item.Predicate),
			Object:     strings.TrimSpace(item.Object),
			Content:    content,
			Confidence: clamp01(item.Confidence, 0.7),
			Importance: clamp01(item.Importance, 0.5),
			Status:     types.MemoryStatusActive,
			DueAt:      parseMemoryDueAt(item.DueAt),
		}
		if i < len(factEmbeddings) && factEmbeddings[i] != nil {
			fact.Embedding = factEmbeddings[i]
		}
		fact.TripleHash = types.ComputeTripleHash(fact.Category, fact.Subject, fact.Predicate, fact.Object)

		existing, err := s.memoryRepo.GetFactByTripleHash(ctx, fact.TenantID, fact.UserID, fact.TripleHash)
		if err != nil {
			logger.Warnf(ctx, "[Memory] lookup fact by hash failed: %v", err)
			continue
		}
		if existing != nil {
			// Refresh instead of duplicating: keep the stronger confidence /
			// importance signals and bump access so the fact stays hot.
			fact.ID = existing.ID
			if fact.Confidence < existing.Confidence {
				fact.Confidence = existing.Confidence
			}
			if fact.Importance < existing.Importance {
				fact.Importance = existing.Importance
			}
			if fact.DueAt == nil {
				fact.DueAt = existing.DueAt
			}
			if fact.Embedding == nil {
				fact.Embedding = existing.Embedding
			}
			if _, err := s.memoryRepo.UpdateFact(ctx, fact); err != nil {
				logger.Warnf(ctx, "[Memory] refresh fact %s failed: %v", existing.ID, err)
				continue
			}
			_ = s.memoryRepo.TouchFacts(ctx, fact.TenantID, fact.UserID, []string{existing.ID}, time.Now())
			upserted++
			continue
		}
		if err := s.memoryRepo.CreateFact(ctx, fact); err != nil {
			logger.Warnf(ctx, "[Memory] create fact failed: %v", err)
			continue
		}
		upserted++
	}
	return upserted
}

// refreshSessionSummary upserts the L2 rolling summary of the session.
func (s *memoryService) refreshSessionSummary(
	ctx context.Context,
	payload *types.MemoryExtractPayload,
	messages []*types.Message,
	result *types.MemoryExtractionResult,
	summaryEmbedding types.VectorBlob,
) {
	now := time.Now()
	summary := &types.MemorySessionSummary{
		TenantID:      payload.TenantID,
		UserID:        payload.UserID,
		SessionID:     payload.SessionID,
		Summary:       strings.TrimSpace(result.SessionSummary),
		KeyTopics:     types.StringArray(result.KeyTopics),
		MessageCount:  len(messages),
		LastMessageAt: &now,
		Embedding:     summaryEmbedding,
	}
	if err := s.memoryRepo.UpsertSessionSummary(ctx, summary); err != nil {
		logger.Warnf(ctx, "[Memory] upsert session summary failed (session %s): %v", payload.SessionID, err)
	}
}

// enforceFactCap evicts the lowest-importance facts beyond MaxFactsPerUser.
func (s *memoryService) enforceFactCap(ctx context.Context, tenantID uint64, userID string) {
	count, err := s.memoryRepo.CountActiveFacts(ctx, tenantID, userID)
	if err != nil || count <= types.MaxFactsPerUser {
		return
	}
	excess := int(count - types.MaxFactsPerUser)
	candidates, err := s.memoryRepo.ListLowestImportanceFacts(ctx, tenantID, userID, excess)
	if err != nil {
		return
	}
	for _, fact := range candidates {
		if _, err := s.memoryRepo.DeleteFact(ctx, tenantID, userID, fact.ID); err != nil {
			logger.Warnf(ctx, "[Memory] evict fact %s failed: %v", fact.ID, err)
		}
	}
	logger.Infof(ctx, "[Memory] evicted %d low-importance facts for user %s (cap %d)",
		len(candidates), userID, types.MaxFactsPerUser)
}

// ---------------------------------------------------------------------------
// Recall (L2 + L3, scored in Go)
// ---------------------------------------------------------------------------

// Recall scores the user's memories against the query embedding and returns
// the top-k. score = semantic x timeDecay x (1 + ln(1+accessCount)).
func (s *memoryService) Recall(ctx context.Context, params *types.MemoryRecallParams) ([]*types.RecalledMemory, error) {
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil, nil
	}
	userID := types.SessionOwnerIDFromContext(ctx)
	if userID == "" || !s.IsEnabled(ctx, userID) {
		return nil, nil
	}
	if params == nil {
		params = &types.MemoryRecallParams{}
	}
	now := params.Now
	if now.IsZero() {
		now = time.Now()
	}
	limit := params.Limit
	if limit <= 0 {
		limit = types.DefaultMemoryRecallLimit
	}
	factHalfLife := params.FactHalfLife
	if factHalfLife <= 0 {
		factHalfLife = types.DefaultFactHalfLife
	}
	summaryHalfLife := params.SummaryHalfLife
	if summaryHalfLife <= 0 {
		summaryHalfLife = types.DefaultSessionSummaryHalfLife
	}

	// Embed the query when the caller did not pre-compute a vector. Embedding
	// failure degrades recall to recency/importance scoring, never an error.
	queryEmbedding := params.QueryEmbedding
	if len(queryEmbedding) == 0 && strings.TrimSpace(params.Query) != "" {
		if emb := s.resolveEmbedder(ctx, ""); emb != nil {
			if vec, err := emb.Embed(ctx, params.Query); err == nil {
				queryEmbedding = vec
			} else {
				logger.Warnf(ctx, "[Memory] query embedding failed, recall degrades to non-semantic: %v", err)
			}
		}
	}

	// Candidate window: 10 half-lives back. Beyond that the decay floor makes
	// scores indistinguishable, so older rows are not worth loading.
	facts, err := s.memoryRepo.ListActiveFactsForRecall(
		ctx, tenantID, userID, params.Categories, now.Add(-10*factHalfLife), types.MemoryRecallCandidateLimit)
	if err != nil {
		return nil, err
	}
	summaries, err := s.memoryRepo.ListSessionSummariesForRecall(
		ctx, tenantID, userID, now.Add(-10*summaryHalfLife), types.MemoryRecallCandidateLimit)
	if err != nil {
		return nil, err
	}

	scored := make([]*types.RecalledMemory, 0, len(facts)+len(summaries))
	for _, fact := range facts {
		semantic := 0.0
		if len(queryEmbedding) > 0 && len(fact.Embedding) > 0 {
			semantic = types.Cosine(queryEmbedding, fact.Embedding)
			// Soul directives are persona-level: they must inject even when
			// topically unrelated to the current query, so their semantic
			// signal is boosted (PRD P0-2 FR2).
			if fact.Category == types.MemoryCategorySoul {
				semantic *= 1.5
				if semantic > 1 {
					semantic = 1
				}
			}
		}
		reference := fact.UpdatedAt
		if fact.LastAccessedAt != nil && fact.LastAccessedAt.After(reference) {
			reference = *fact.LastAccessedAt
		}
		score := types.MemoryRecallScore(semantic, reference, fact.AccessCount, factHalfLife, now)
		// Blend a small importance prior so never-accessed but critical facts
		// (deadlines, identity) still surface without a semantic match.
		score += 0.1 * fact.Importance
		scored = append(scored, &types.RecalledMemory{Kind: "fact", Fact: fact, Score: score})
	}
	for _, sum := range summaries {
		semantic := 0.0
		if len(queryEmbedding) > 0 && len(sum.Embedding) > 0 {
			semantic = types.Cosine(queryEmbedding, sum.Embedding)
		}
		score := types.MemoryRecallScore(semantic, sum.UpdatedAt, 0, summaryHalfLife, now)
		scored = append(scored, &types.RecalledMemory{Kind: "session_summary", Summary: sum, Score: score})
	}

	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	scored = selectRecalledWithSoulQuota(scored, limit)

	// Bump access stats fire-and-forget so frequently recalled memories rank
	// higher over time. Use WithoutCancel: the HTTP request may end first.
	if ids := recalledFactIDs(scored); len(ids) > 0 {
		bgCtx := context.WithoutCancel(ctx)
		go func() {
			if err := s.memoryRepo.TouchFacts(bgCtx, tenantID, userID, ids, now); err != nil {
				logger.Warnf(bgCtx, "[Memory] touch facts failed: %v", err)
			}
		}()
	}
	return scored, nil
}

func recalledFactIDs(memories []*types.RecalledMemory) []string {
	ids := make([]string, 0, len(memories))
	for _, m := range memories {
		if m.Kind == "fact" && m.Fact != nil {
			ids = append(ids, m.Fact.ID)
		}
	}
	return ids
}

// soulOverflowTolerance bounds how far the final recall set may exceed the
// regular limit when soul directives are appended (PRD P0-2 FR2: total ≤
// DefaultMemoryRecallLimit + 2).
const soulOverflowTolerance = 2

// soulQuotaMax is the defensive ceiling on soul directives in one recall
// set. Soul directives are few by nature; the cap guards against runaway
// persona memories, it is not an expected operating point.
const soulQuotaMax = 4

// selectRecalledWithSoulQuota picks the final recall set from the ranked
// candidates: soul directives have their own quota (they are persona-level
// and must not be pushed out by topical memories), everything else takes the
// top `limit` by score. Total is bounded at limit + soulOverflowTolerance.
func selectRecalledWithSoulQuota(ranked []*types.RecalledMemory, limit int) []*types.RecalledMemory {
	souls := 0
	others := 0
	totalCap := limit + soulOverflowTolerance
	selected := make([]*types.RecalledMemory, 0, totalCap)
	for _, m := range ranked {
		if others+souls >= totalCap {
			// Total budget exhausted (PRD FR2: total ≤ limit + 2).
			break
		}
		isSoul := m.Kind == "fact" && m.Fact != nil && m.Fact.Category == types.MemoryCategorySoul
		if isSoul {
			if souls >= soulQuotaMax {
				continue
			}
			souls++
		} else {
			if others >= limit {
				// Keep scanning: later soul directives can still fill the
				// soul quota.
				continue
			}
			others++
		}
		selected = append(selected, m)
	}
	return selected
}

// Per-module injection budgets (PRD P0-2 FR2). Soul directives are few and
// stable, so they inject in full; the user profile takes its most important
// rows; skills inject by importance x confidence.
const (
	// memoryInjectProfileMax bounds the 用户档案 block (identity/role rows).
	memoryInjectProfileMax = 4
	// memoryInjectSkillMax bounds the 助手经验 block.
	memoryInjectSkillMax = 3
)

// factLine renders one fact as a prompt bullet; todos carry their deadline.
func factLine(fact *types.MemoryFact) string {
	line := "- " + fact.Content
	if fact.Category == types.MemoryCategoryTodo && fact.DueAt != nil {
		line += fmt.Sprintf("（截止 %s）", fact.DueAt.Format("2006-01-02"))
	}
	return line
}

// FormatRecalledForPrompt renders recalled memories as a system-prompt block,
// grouped into the four memory modules with fixed section order and
// per-module budgets (soul directives → user profile → memory stream →
// assistant skills). Raw feedback is archived for audit but never injected —
// only its distilled skill is.
func (s *memoryService) FormatRecalledForPrompt(memories []*types.RecalledMemory) string {
	if len(memories) == 0 {
		return ""
	}

	var soul, profile, memoryFacts, skills, summaries []string
	var profileFacts, skillFacts []*types.MemoryFact
	for _, m := range memories {
		switch m.Kind {
		case "fact":
			if m.Fact == nil {
				continue
			}
			switch m.Fact.Category {
			case types.MemoryCategorySoul:
				soul = append(soul, factLine(m.Fact))
			case types.MemoryCategoryProfile, types.MemoryCategoryFact:
				profileFacts = append(profileFacts, m.Fact)
			case types.MemoryCategorySkill:
				skillFacts = append(skillFacts, m.Fact)
			case types.MemoryCategoryFeedback:
				// Never injected: the distilled skill carries the lesson.
			default:
				memoryFacts = append(memoryFacts, factLine(m.Fact))
			}
		case "session_summary":
			if m.Summary == nil {
				continue
			}
			summaries = append(summaries, "- "+m.Summary.Summary)
		}
	}

	// 用户档案: importance-ordered, bounded.
	sort.SliceStable(profileFacts, func(i, j int) bool {
		return profileFacts[i].Importance > profileFacts[j].Importance
	})
	if len(profileFacts) > memoryInjectProfileMax {
		profileFacts = profileFacts[:memoryInjectProfileMax]
	}
	for _, f := range profileFacts {
		profile = append(profile, factLine(f))
	}

	// 助手经验: importance × confidence, bounded (PRD FR2, M=3).
	sort.SliceStable(skillFacts, func(i, j int) bool {
		return skillFacts[i].Importance*skillFacts[i].Confidence > skillFacts[j].Importance*skillFacts[j].Confidence
	})
	if len(skillFacts) > memoryInjectSkillMax {
		skillFacts = skillFacts[:memoryInjectSkillMax]
	}
	for _, f := range skillFacts {
		skills = append(skills, factLine(f))
	}

	if len(soul)+len(profile)+len(memoryFacts)+len(skills)+len(summaries) == 0 {
		return ""
	}

	writeSection := func(b *strings.Builder, title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("\n")
		b.WriteString(title)
		b.WriteString("\n")
		for _, l := range lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}

	var b strings.Builder
	b.WriteString("## 关于用户的长期记忆\n")
	b.WriteString("以下来自历史对话的记忆可能与本问题相关，请在回答时自然地利用，不要逐字复述：\n")
	writeSection(&b, "### 助手风格指令（用户设定）", soul)
	writeSection(&b, "### 用户档案", profile)
	writeSection(&b, "### 相关长期记忆", memoryFacts)
	if len(summaries) > 0 {
		b.WriteString("\n相关历史对话摘要：\n")
		for _, l := range summaries {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	writeSection(&b, "### 助手经验", skills)
	return b.String()
}

// ---------------------------------------------------------------------------
// User-facing management surface (GDPR)
// ---------------------------------------------------------------------------

// ListFacts is the "AI 记住了我什么" listing, scoped to the ctx user.
func (s *memoryService) ListFacts(
	ctx context.Context, q *types.MemoryFactQuery,
) ([]*types.MemoryFact, int64, error) {
	tenantID, userID, ok := memoryScopeFromContext(ctx)
	if !ok {
		return nil, 0, fmt.Errorf("memory scope unavailable")
	}
	return s.memoryRepo.ListFacts(ctx, tenantID, userID, q)
}

// ---------------------------------------------------------------------------
// Four-module aggregation surface (P0-2)
// ---------------------------------------------------------------------------

// GetModuleOverview returns per-module active fact counts plus the L2
// summary count on the memory module. Two DB round trips total.
func (s *memoryService) GetModuleOverview(ctx context.Context) ([]*types.MemoryModuleOverview, error) {
	tenantID, userID, ok := memoryScopeFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("memory scope unavailable")
	}
	byCategory, err := s.memoryRepo.CountActiveFactsByCategory(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	summaryCount, err := s.memoryRepo.CountSessionSummaries(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}

	overview := make([]*types.MemoryModuleOverview, 0, 4)
	var memoryCount int64
	for _, module := range []string{
		types.MemoryModuleSoul, types.MemoryModuleUser, types.MemoryModuleMemory, types.MemoryModuleAgent,
	} {
		var count int64
		for category, n := range byCategory {
			if types.MemoryModuleOf(category) == module {
				count += n
			}
			if module == types.MemoryModuleMemory {
				// The memory stream module covers every fact.
				memoryCount += n
			}
		}
		row := &types.MemoryModuleOverview{Module: module, FactCount: count}
		if module == types.MemoryModuleMemory {
			row.FactCount = memoryCount
			row.SummaryCount = summaryCount
		}
		overview = append(overview, row)
	}
	return overview, nil
}

// resolveGlobalPersona loads the default system-prompt template as the
// read-only global persona; empty when no template is configured (the Soul
// card degrades to the adjustments list only).
func (s *memoryService) resolveGlobalPersona() types.SoulPersona {
	if s.cfg == nil || s.cfg.PromptTemplates == nil {
		return types.SoulPersona{}
	}
	templates := s.cfg.PromptTemplates.SystemPrompt
	var chosen *config.PromptTemplate
	for i := range templates {
		if templates[i].Default {
			chosen = &templates[i]
			break
		}
	}
	if chosen == nil && len(templates) > 0 {
		chosen = &templates[0]
	}
	if chosen == nil {
		return types.SoulPersona{}
	}
	return types.SoulPersona{
		Name:        chosen.Name,
		Description: chosen.Description,
		Content:     chosen.Content,
	}
}

// GetSoulCard returns the global persona plus the user's style directives.
func (s *memoryService) GetSoulCard(ctx context.Context) (*types.SoulCard, error) {
	adjustments, _, err := s.ListFacts(ctx, &types.MemoryFactQuery{
		Category: types.MemoryCategorySoul,
		Page:     1,
		PageSize: 100,
	})
	if err != nil {
		return nil, err
	}
	if adjustments == nil {
		adjustments = []*types.MemoryFact{}
	}
	return &types.SoulCard{
		GlobalPersona: s.resolveGlobalPersona(),
		Adjustments:   adjustments,
	}, nil
}

// profileSectionOrder is the fixed section order of the User profile card.
var profileSectionOrder = []string{
	types.MemoryProfileSectionIdentity,
	types.MemoryProfileSectionRole,
	types.MemoryProfileSectionPreference,
	types.MemoryProfileSectionFact,
}

// GetProfileCard groups the user's profile/fact/preference memories into the
// four profile sections (identity, role, preference, fact) and computes the
// weighted completeness (identity/role weigh double).
func (s *memoryService) GetProfileCard(ctx context.Context) (*types.ProfileCard, error) {
	var facts []*types.MemoryFact
	for _, category := range []string{
		types.MemoryCategoryProfile, types.MemoryCategoryFact, types.MemoryCategoryPreference,
	} {
		items, _, err := s.ListFacts(ctx, &types.MemoryFactQuery{
			Category: category,
			Page:     1,
			PageSize: 500,
		})
		if err != nil {
			return nil, err
		}
		facts = append(facts, items...)
	}

	sections := make([]*types.MemoryProfileSection, 0, len(profileSectionOrder))
	byKey := make(map[string]*types.MemoryProfileSection, len(profileSectionOrder))
	for _, key := range profileSectionOrder {
		section := &types.MemoryProfileSection{Key: key, Items: []*types.MemoryFact{}}
		byKey[key] = section
		sections = append(sections, section)
	}
	for _, fact := range facts {
		key := types.MemoryProfileSectionOf(fact)
		byKey[key].Items = append(byKey[key].Items, fact)
	}

	// Weighted completeness: identity/role weigh double (stable attributes
	// define who the user is), preference/fact weigh 1.
	completeness := 0.0
	const totalWeight = 6.0
	for _, section := range sections {
		if len(section.Items) == 0 {
			continue
		}
		switch section.Key {
		case types.MemoryProfileSectionIdentity, types.MemoryProfileSectionRole:
			completeness += 2
		default:
			completeness += 1
		}
	}
	return &types.ProfileCard{Sections: sections, Completeness: completeness / totalWeight}, nil
}

// GetAgentTipsCard returns the distilled skills plus the raw feedback wall.
// Feedback items are linked to the skill they were upgraded into via their
// shared extraction turn (same session + message ID).
func (s *memoryService) GetAgentTipsCard(ctx context.Context, page, pageSize int) (*types.AgentTipsCard, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}

	skills, _, err := s.ListFacts(ctx, &types.MemoryFactQuery{
		Category: types.MemoryCategorySkill,
		Page:     1,
		PageSize: 200,
	})
	if err != nil {
		return nil, err
	}
	feedback, feedbackTotal, err := s.ListFacts(ctx, &types.MemoryFactQuery{
		Category: types.MemoryCategoryFeedback,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		return nil, err
	}

	// Upgrade linkage: a feedback and the skill distilled from it share the
	// same extraction turn. When several skills share a turn, the strongest
	// (highest importance) wins.
	skillByTurn := make(map[string]*types.MemoryFact, len(skills))
	for _, skill := range skills {
		key := skill.SessionID + "|" + skill.MessageID
		if existing, ok := skillByTurn[key]; ok && existing.Importance >= skill.Importance {
			continue
		}
		skillByTurn[key] = skill
	}

	items := make([]*types.AgentFeedbackItem, 0, len(feedback))
	for _, fb := range feedback {
		item := &types.AgentFeedbackItem{MemoryFact: fb}
		if skill, ok := skillByTurn[fb.SessionID+"|"+fb.MessageID]; ok && skill != nil && skill.ID != fb.ID {
			item.UpgradedTo = skill.ID
		}
		items = append(items, item)
	}

	if skills == nil {
		skills = []*types.MemoryFact{}
	}
	if items == nil {
		items = []*types.AgentFeedbackItem{}
	}
	return &types.AgentTipsCard{
		Skills:        skills,
		Feedback:      items,
		FeedbackTotal: feedbackTotal,
	}, nil
}

// UpdateFact applies a user edit to one fact owned by the ctx user. Empty
// fields fall back to the stored values; a content/object edit re-derives the
// dedup hash and re-embeds so semantic recall stays accurate.
func (s *memoryService) UpdateFact(ctx context.Context, fact *types.MemoryFact) error {
	tenantID, userID, ok := memoryScopeFromContext(ctx)
	if !ok {
		return fmt.Errorf("memory scope unavailable")
	}
	if fact == nil || fact.ID == "" {
		return fmt.Errorf("fact id is required")
	}
	existing, err := s.memoryRepo.GetFactByID(ctx, tenantID, userID, fact.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("memory fact not found")
	}

	fact.TenantID = tenantID
	fact.UserID = userID
	fact.Category = existing.Category // category is immutable via the edit API
	if fact.Subject == "" {
		fact.Subject = existing.Subject
	}
	if fact.Predicate == "" {
		fact.Predicate = existing.Predicate
	}
	if fact.Object == "" {
		fact.Object = existing.Object
	}
	if fact.Content == "" {
		fact.Content = existing.Content
	}
	if fact.Status == "" {
		fact.Status = existing.Status
	}
	if fact.Confidence <= 0 {
		fact.Confidence = existing.Confidence
	}
	if fact.Importance <= 0 {
		fact.Importance = existing.Importance
	}
	fact.TripleHash = types.ComputeTripleHash(fact.Category, fact.Subject, fact.Predicate, fact.Object)

	// Re-embed when the human-readable content changed so recall semantics
	// track the edit. Embedder resolution failure never blocks the edit.
	if fact.Content != existing.Content {
		if emb := s.resolveEmbedder(ctx, ""); emb != nil {
			if vec, err := emb.Embed(ctx, fact.Content); err == nil {
				fact.Embedding = types.VectorBlob(vec)
			}
		}
	}

	rows, err := s.memoryRepo.UpdateFact(ctx, fact)
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("memory fact not found")
	}
	return nil
}

// DeleteFact removes one fact owned by the ctx user.
func (s *memoryService) DeleteFact(ctx context.Context, id string) error {
	tenantID, userID, ok := memoryScopeFromContext(ctx)
	if !ok {
		return fmt.Errorf("memory scope unavailable")
	}
	rows, err := s.memoryRepo.DeleteFact(ctx, tenantID, userID, id)
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("memory fact not found")
	}
	return nil
}

// DeleteAllForUser erases every memory row of the ctx user (GDPR erase).
func (s *memoryService) DeleteAllForUser(ctx context.Context) (int64, error) {
	tenantID, userID, ok := memoryScopeFromContext(ctx)
	if !ok {
		return 0, fmt.Errorf("memory scope unavailable")
	}
	facts, err := s.memoryRepo.DeleteAllFacts(ctx, tenantID, userID)
	if err != nil {
		return 0, err
	}
	summaries, err := s.memoryRepo.DeleteAllSessionSummaries(ctx, tenantID, userID)
	if err != nil {
		return facts, err
	}
	return facts + summaries, nil
}

// DeleteSessionMemory drops the L2 summary bound to a session (session delete
// hook). L3 facts intentionally survive — that is the point of long-term memory.
func (s *memoryService) DeleteSessionMemory(ctx context.Context, tenantID uint64, sessionID string) {
	if _, err := s.memoryRepo.DeleteSessionSummaryBySession(ctx, tenantID, sessionID); err != nil {
		logger.Warnf(ctx, "[Memory] delete session summary failed (session %s): %v", sessionID, err)
	}
}

// DeleteAllSummariesForUser drops every L2 summary of the ctx user (workspace
// "delete all sessions" hook).
func (s *memoryService) DeleteAllSummariesForUser(ctx context.Context) (int64, error) {
	tenantID, userID, ok := memoryScopeFromContext(ctx)
	if !ok {
		return 0, fmt.Errorf("memory scope unavailable")
	}
	return s.memoryRepo.DeleteAllSessionSummaries(ctx, tenantID, userID)
}

// ---------------------------------------------------------------------------
// Feature switch
// ---------------------------------------------------------------------------

// IsEnabled reports whether memory is enabled for the user. Default on; an
// explicit per-user preference of false disables extraction and recall.
func (s *memoryService) IsEnabled(ctx context.Context, userID string) bool {
	if userID == "" {
		return false
	}
	user, err := s.userRepo.GetUserByID(ctx, userID)
	if err != nil || user == nil {
		// Principal-derived IDs (API external users, embed visitors,
		// synthetic tenant users) have no users row; treat them as enabled so
		// their sessions still benefit from memory. The (tenant, user) repo
		// scoping keeps their memories isolated per principal ID.
		return true
	}
	if user.Preferences.MemoryEnabled == nil {
		return true
	}
	return *user.Preferences.MemoryEnabled
}

// memoryScopeFromContext resolves the (tenant, user) memory scope of the
// current caller.
func memoryScopeFromContext(ctx context.Context) (uint64, string, bool) {
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return 0, "", false
	}
	userID := types.SessionOwnerIDFromContext(ctx)
	if userID == "" {
		return 0, "", false
	}
	return tenantID, userID, true
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func validMemoryCategory(category string) bool {
	switch category {
	case types.MemoryCategoryProfile,
		types.MemoryCategoryFact,
		types.MemoryCategoryPreference,
		types.MemoryCategoryTodo,
		types.MemoryCategoryFeedback,
		types.MemoryCategorySoul,
		types.MemoryCategorySkill:
		return true
	}
	return false
}

func clamp01(v, fallback float64) float64 {
	if v <= 0 {
		return fallback
	}
	if v > 1 {
		return 1
	}
	return v
}

// parseMemoryDueAt parses an ISO date or datetime; invalid input is dropped
// (a todo without a deadline is still a valid todo).
func parseMemoryDueAt(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return &t
		}
	}
	return nil
}
