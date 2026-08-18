package service

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/hibiken/asynq"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// precipitateMaxMessages bounds how many session messages are fed to the
// distillation LLM. High-value sessions can grow long; beyond this window the
// marginal curation value no longer justifies the tokens.
const precipitateMaxMessages = 60

// precipitateMessageChars truncates one message inside the distillation
// prompt; curated documents need conclusions, not verbatim RAG dumps.
const precipitateMessageChars = 1500

// precipitateUniqueTTL deduplicates enqueue storms: every turn of a hot
// session would otherwise re-enqueue. Within the TTL the first task wins;
// after it expires a fresh run refreshes the document with the latest turns.
const precipitateUniqueTTL = 30 * time.Minute

// defaultSessionPrecipitationPrompt / defaultSessionWikiPrompt are used when
// the configured template cannot be resolved. Canonical templates live in
// config/prompt_templates/session_precipitation.yaml.
const defaultSessionPrecipitationPrompt = `You are the knowledge curator of an AI assistant. Distill the provided
conversation into a durable Markdown knowledge document with sections:
# <title>, ## 背景与目标, ## 关键结论, ## 待办与后续 (omit when empty),
## 涉及实体. Be faithful, self-contained, under 800 words, in the
conversation's language. Output Markdown only.`

const defaultSessionWikiPrompt = `You are the wiki editor of an AI assistant. Distill the provided conversation
into ONE standalone wiki article. Output JSON only:
{"title":"...","summary":"...","content":"..."}
content is GitHub-flavored Markdown organized in ## sections (not a chat log);
link first mentions of wiki-worthy terms with [[slug]] syntax (at most 10).
Be faithful, in the conversation's language, under 1200 words.`

// sessionPrecipitationService implements interfaces.SessionPrecipitationService.
type sessionPrecipitationService struct {
	cfg              *config.Config
	sessionRepo      interfaces.SessionRepository
	messageRepo      interfaces.MessageRepository
	favoriteRepo     interfaces.UserResourceFavoriteRepository
	knowledgeRepo    interfaces.KnowledgeRepository
	knowledgeService interfaces.KnowledgeService
	kbService        interfaces.KnowledgeBaseService
	wikiPageService  interfaces.WikiPageService
	modelService     interfaces.ModelService
	taskEnqueuer     interfaces.TaskEnqueuer
}

// NewSessionPrecipitationService creates the knowledge-precipitation service.
func NewSessionPrecipitationService(
	cfg *config.Config,
	sessionRepo interfaces.SessionRepository,
	messageRepo interfaces.MessageRepository,
	favoriteRepo interfaces.UserResourceFavoriteRepository,
	knowledgeRepo interfaces.KnowledgeRepository,
	knowledgeService interfaces.KnowledgeService,
	kbService interfaces.KnowledgeBaseService,
	wikiPageService interfaces.WikiPageService,
	modelService interfaces.ModelService,
	taskEnqueuer interfaces.TaskEnqueuer,
) interfaces.SessionPrecipitationService {
	return &sessionPrecipitationService{
		cfg:              cfg,
		sessionRepo:      sessionRepo,
		messageRepo:      messageRepo,
		favoriteRepo:     favoriteRepo,
		knowledgeRepo:    knowledgeRepo,
		knowledgeService: knowledgeService,
		kbService:        kbService,
		wikiPageService:  wikiPageService,
		modelService:     modelService,
		taskEnqueuer:     taskEnqueuer,
	}
}

// ---------------------------------------------------------------------------
// High-value detection + enqueue (called from the memory-extract worker)
// ---------------------------------------------------------------------------

// MaybeEnqueuePrecipitation enqueues the precipitation task when the session
// qualifies as high-value: the user favorited it, or the conversation shows
// sustained follow-up depth. Best-effort — all failures are logged and
// swallowed so the memory pipeline never breaks on precipitation.
func (s *sessionPrecipitationService) MaybeEnqueuePrecipitation(ctx context.Context, payload *types.SessionPrecipitatePayload) {
	if payload == nil || payload.SessionID == "" || payload.TenantID == 0 {
		return
	}
	trigger := s.highValueTrigger(ctx, payload)
	if trigger == "" {
		return
	}
	payload.Trigger = trigger
	langfuse.InjectTracing(ctx, payload)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf(ctx, "[Precipitate] marshal payload failed for session %s: %v", payload.SessionID, err)
		return
	}
	if s.taskEnqueuer == nil {
		return
	}
	task := asynq.NewTask(types.TypeSessionPrecipitate, payloadBytes,
		asynq.Queue(types.QueuePostProcess),
		asynq.MaxRetry(2),
		asynq.Timeout(5*time.Minute),
		// Coalesce the per-turn enqueue storm of a hot session into at most
		// one pending precipitation run per TTL window.
		asynq.Unique(precipitateUniqueTTL),
	)
	if _, err := s.taskEnqueuer.Enqueue(task); err != nil {
		// asynq.ErrDuplicateTask means a run is already pending — the desired
		// dedup outcome, not a failure.
		if strings.Contains(err.Error(), "duplicate") {
			return
		}
		logger.Warnf(ctx, "[Precipitate] enqueue failed for session %s: %v", payload.SessionID, err)
		return
	}
	logger.Debugf(ctx, "[Precipitate] enqueued session=%s trigger=%s", payload.SessionID, trigger)
}

// highValueTrigger returns "favorite" / "followups" when the session meets a
// high-value criterion, "" otherwise.
func (s *sessionPrecipitationService) highValueTrigger(ctx context.Context, payload *types.SessionPrecipitatePayload) string {
	if s.favoriteRepo != nil && payload.UserID != "" {
		fav, err := s.favoriteRepo.IsFavorite(
			ctx, payload.UserID, payload.TenantID, types.ResourceTypeSession, payload.SessionID)
		if err == nil && fav {
			return "favorite"
		}
	}
	// Follow-up depth: fetch up to the threshold; reaching it means the
	// conversation sustained ~threshold/2 QA rounds of follow-ups.
	messages, err := s.messageRepo.GetMessagesBySession(
		ctx, payload.SessionID, 1, types.DefaultPrecipitateMessageThreshold)
	if err != nil {
		logger.Warnf(ctx, "[Precipitate] count messages failed for session %s: %v", payload.SessionID, err)
		return ""
	}
	if len(messages) >= types.DefaultPrecipitateMessageThreshold {
		return "followups"
	}
	return ""
}

// ---------------------------------------------------------------------------
// ProcessSessionPrecipitate (worker side)
// ---------------------------------------------------------------------------

// ProcessSessionPrecipitate is the asynq handler for types.TypeSessionPrecipitate.
func (s *sessionPrecipitationService) ProcessSessionPrecipitate(ctx context.Context, task *asynq.Task) error {
	var payload types.SessionPrecipitatePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal session precipitate payload: %w", err)
	}

	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.UserID != "" {
		ctx = context.WithValue(ctx, types.UserIDContextKey, payload.UserID)
	}

	session, messages, err := s.loadSessionConversation(ctx, payload.TenantID, payload.UserID, payload.SessionID)
	if err != nil {
		if stderrors.Is(err, apperrors.ErrSessionNotFound) {
			// Session was deleted between enqueue and execution.
			return nil
		}
		return fmt.Errorf("load session conversation: %w", err)
	}
	if session == nil || len(messages) == 0 {
		return nil
	}

	chatModel, err := s.resolveChatModel(ctx, payload.SummaryModelID)
	if err != nil {
		logger.Warnf(ctx, "[Precipitate] resolve chat model failed (session %s): %v", payload.SessionID, err)
		return nil // model misconfiguration is not retryable
	}

	document, err := s.distillDocument(ctx, chatModel, session, messages)
	if err != nil {
		return fmt.Errorf("distill session document: %w", err)
	}
	if strings.TrimSpace(document) == "" {
		return nil
	}

	kb, err := s.ensurePrecipitationKB(ctx, payload.TenantID)
	if err != nil {
		// KB creation depends on an embedding model being configured; treat
		// resolution failure as permanent for this run.
		logger.Warnf(ctx, "[Precipitate] ensure precipitation KB failed: %v", err)
		return nil
	}

	if err := s.upsertSessionDocument(ctx, kb, session, document); err != nil {
		return fmt.Errorf("upsert session document: %w", err)
	}
	logger.Infof(ctx, "[Precipitate] session=%s trigger=%s kb=%s done",
		payload.SessionID, payload.Trigger, kb.ID)
	return nil
}

// loadSessionConversation loads the session plus a bounded message window.
// Returns (nil, nil, nil) when the session is gone (deleted between enqueue
// and execution — nothing to precipitate).
func (s *sessionPrecipitationService) loadSessionConversation(
	ctx context.Context, tenantID uint64, userID, sessionID string,
) (*types.Session, []*types.Message, error) {
	session, err := s.sessionRepo.Get(ctx, tenantID, userID, sessionID)
	if err != nil || session == nil {
		return nil, nil, err
	}
	messages, err := s.messageRepo.GetMessagesBySession(ctx, sessionID, 1, precipitateMaxMessages)
	if err != nil {
		return nil, nil, err
	}
	return session, messages, nil
}

// distillDocument runs the curation LLM call and returns the Markdown body.
func (s *sessionPrecipitationService) distillDocument(
	ctx context.Context, chatModel chat.Chat, session *types.Session, messages []*types.Message,
) (string, error) {
	systemPrompt := ""
	if s.cfg != nil && s.cfg.Conversation != nil {
		systemPrompt = strings.TrimSpace(s.cfg.Conversation.SessionPrecipitationPrompt)
	}
	if systemPrompt == "" {
		systemPrompt = defaultSessionPrecipitationPrompt
	}
	thinking := false
	response, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: renderConversation(session, messages)},
	}, &chat.ChatOptions{Temperature: 0.2, Thinking: &thinking})
	if err != nil {
		return "", err
	}
	if response == nil {
		return "", nil
	}
	return strings.TrimSpace(agenttools.StripThinkBlocks(response.Content)), nil
}

// ensurePrecipitationKB finds or creates the tenant's hidden session-insights
// knowledge base (same auto-managed pattern as __chat_history__).
func (s *sessionPrecipitationService) ensurePrecipitationKB(ctx context.Context, tenantID uint64) (*types.KnowledgeBase, error) {
	kbs, err := s.kbService.ListKnowledgeBasesByTenantID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for _, kb := range kbs {
		if kb != nil && kb.Name == types.SessionPrecipitationKBName && kb.DeletedAt.Time.IsZero() {
			return kb, nil
		}
	}

	// First precipitation in this tenant: create the hidden KB. It needs an
	// embedding model so precipitated documents become retrievable.
	embeddingModelID := s.resolveEmbeddingModelID(ctx)
	if embeddingModelID == "" {
		return nil, fmt.Errorf("no embedding model available for precipitation KB")
	}
	kb := &types.KnowledgeBase{
		Name:             types.SessionPrecipitationKBName,
		Type:             types.KnowledgeBaseTypeDocument,
		IsTemporary:      true, // hidden from user-facing KB listings
		Description:      "Auto-managed knowledge base for high-value session precipitation (知识沉淀)",
		EmbeddingModelID: embeddingModelID,
	}
	return s.kbService.CreateKnowledgeBase(ctx, kb)
}

// upsertSessionDocument creates or refreshes the precipitated document of a
// session inside the precipitation KB. Dedup keys on the provenance marker
// (Source = "session:<id>"), with the session-marked title as a fallback for
// rows whose Source was clobbered by the manual-knowledge update flow.
func (s *sessionPrecipitationService) upsertSessionDocument(
	ctx context.Context, kb *types.KnowledgeBase, session *types.Session, document string,
) error {
	title := precipitatedTitle(session)
	payload := &types.ManualKnowledgePayload{
		Title:   title,
		Content: document,
		Status:  types.ManualKnowledgeStatusPublish,
		Channel: types.SessionPrecipitationChannel,
	}

	existing, err := s.findSessionDocument(ctx, kb.ID, session.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		if _, err := s.knowledgeService.UpdateManualKnowledge(ctx, existing.ID, payload); err != nil {
			return err
		}
		// UpdateManualKnowledge resets Source to "manual"; restore the
		// session provenance marker so the next run still dedups.
		s.restoreSessionSource(ctx, existing.TenantID, existing.ID, session.ID)
		return nil
	}

	// Create as draft first (no processing task enqueued yet), stamp the
	// provenance marker, then flip to publish — this ordering guarantees the
	// async indexing task reloads the row with Source already in place.
	knowledge, err := s.knowledgeService.CreateKnowledgeFromManual(ctx, kb.ID, &types.ManualKnowledgePayload{
		Title:   title,
		Content: document,
		Status:  types.ManualKnowledgeStatusDraft,
		Channel: types.SessionPrecipitationChannel,
	}, types.SessionPrecipitationChannel)
	if err != nil {
		return err
	}
	knowledge.Source = types.SessionKnowledgeSource(session.ID)
	if err := s.knowledgeRepo.UpdateKnowledge(ctx, knowledge); err != nil {
		logger.Warnf(ctx, "[Precipitate] stamp session source failed for knowledge %s: %v", knowledge.ID, err)
	}
	if _, err := s.knowledgeService.UpdateManualKnowledge(ctx, knowledge.ID, payload); err != nil {
		return err
	}
	return nil
}

// findSessionDocument locates the precipitated document of a session inside
// the precipitation KB by provenance marker, falling back to the title.
func (s *sessionPrecipitationService) findSessionDocument(
	ctx context.Context, kbID, sessionID string,
) (*types.Knowledge, error) {
	list, err := s.knowledgeService.ListKnowledgeByKnowledgeBaseID(ctx, kbID)
	if err != nil {
		return nil, err
	}
	source := types.SessionKnowledgeSource(sessionID)
	var titleMatch *types.Knowledge
	for _, k := range list {
		if k == nil {
			continue
		}
		if k.Source == source {
			return k, nil
		}
		if titleMatch == nil && strings.HasSuffix(k.Title, "· "+shortSessionID(sessionID)) {
			titleMatch = k
		}
	}
	return titleMatch, nil
}

// restoreSessionSource re-stamps the session provenance marker after the
// manual-update flow reset Source to "manual".
func (s *sessionPrecipitationService) restoreSessionSource(ctx context.Context, tenantID uint64, knowledgeID, sessionID string) {
	knowledge, err := s.knowledgeRepo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
	if err != nil || knowledge == nil {
		return
	}
	knowledge.Source = types.SessionKnowledgeSource(sessionID)
	if err := s.knowledgeRepo.UpdateKnowledge(ctx, knowledge); err != nil {
		logger.Warnf(ctx, "[Precipitate] restore session source failed for knowledge %s: %v", knowledgeID, err)
	}
}

// ---------------------------------------------------------------------------
// One-click session → Wiki (4.4 Wiki 联动)
// ---------------------------------------------------------------------------

// CreateWikiFromSession distills the session into a wiki page in the target
// wiki knowledge base. Idempotent per session: the deterministic
// session/<id> slug is refreshed on repeated calls. The generated page links
// back to its triggering conversation via source_refs and page_metadata,
// closing the 对话→知识→对话 loop.
func (s *sessionPrecipitationService) CreateWikiFromSession(
	ctx context.Context, sessionID string, req *types.SessionWikiRequest,
) (*types.WikiPage, error) {
	if req == nil || strings.TrimSpace(req.KnowledgeBaseID) == "" {
		return nil, fmt.Errorf("knowledge_base_id is required")
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil, fmt.Errorf("tenant scope unavailable")
	}

	// The target must be a wiki knowledge base of this tenant.
	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, req.KnowledgeBaseID)
	if err != nil {
		return nil, err
	}
	if kb == nil || kb.Type != types.KnowledgeBaseTypeWiki {
		return nil, fmt.Errorf("knowledge base %s is not a wiki knowledge base", req.KnowledgeBaseID)
	}

	// Session lookup honors the caller's per-user scope; the not-found
	// sentinel propagates so the handler maps it to 404.
	session, messages, err := s.loadSessionConversation(
		ctx, tenantID, types.SessionOwnerIDFromContext(ctx), sessionID)
	if err != nil {
		return nil, err
	}
	if session == nil || len(messages) == 0 {
		return nil, fmt.Errorf("session has no messages to distill")
	}

	chatModel, err := s.resolveChatModel(ctx, "")
	if err != nil {
		return nil, err
	}
	article, err := s.distillWikiArticle(ctx, chatModel, session, messages)
	if err != nil {
		return nil, err
	}
	if article == nil {
		return nil, fmt.Errorf("wiki distillation returned no article")
	}
	if title := strings.TrimSpace(req.Title); title != "" {
		article.Title = title
	}
	if article.Title == "" {
		article.Title = session.Title
	}

	slug := types.SessionWikiSlug(sessionID)
	sourceRef := types.SessionSourceRef(session.ID, session.Title)
	// PageMetadata marks the page's conversation provenance so the wiki UI
	// can deep-link back to the triggering session.
	metadata, _ := json.Marshal(map[string]string{
		"source":            "session",
		"source_session_id": session.ID,
	})

	existing, err := s.wikiPageService.GetPageBySlug(ctx, kb.ID, slug)
	if err != nil && !stderrors.Is(err, repository.ErrWikiPageNotFound) {
		return nil, err
	}
	if existing != nil {
		existing.Title = article.Title
		existing.Summary = article.Summary
		existing.Content = article.Content
		existing.SourceRefs = mergeSourceRefs(existing.SourceRefs, sourceRef)
		existing.PageMetadata = types.JSON(metadata)
		return s.wikiPageService.UpdatePage(ctx, existing)
	}

	page := &types.WikiPage{
		TenantID:        tenantID,
		KnowledgeBaseID: kb.ID,
		Slug:            slug,
		Title:           article.Title,
		Summary:         article.Summary,
		Content:         article.Content,
		// Session-distilled articles are cross-cutting syntheses of a
		// conversation, matching the synthesis page type's semantics.
		PageType:     types.WikiPageTypeSynthesis,
		SourceRefs:   types.StringArray{sourceRef},
		PageMetadata: types.JSON(metadata),
	}
	created, err := s.wikiPageService.CreatePage(ctx, page)
	if err != nil {
		return nil, err
	}

	// Keep the wiki navigable: surface the new page in the KB index and let
	// related pages discover it. Best-effort — never fail the request.
	s.wikiPageService.InjectCrossLinks(ctx, kb.ID, []string{slug})
	if err := s.wikiPageService.RebuildIndexPage(ctx, kb.ID); err != nil {
		logger.Warnf(ctx, "[Precipitate] rebuild wiki index failed (kb %s): %v", kb.ID, err)
	}
	return created, nil
}

// distillWikiArticle runs the session→wiki LLM call and parses the JSON
// envelope. Returns (nil, nil) when the model produced nothing parseable.
func (s *sessionPrecipitationService) distillWikiArticle(
	ctx context.Context, chatModel chat.Chat, session *types.Session, messages []*types.Message,
) (*types.SessionWikiArticle, error) {
	systemPrompt := ""
	if s.cfg != nil && s.cfg.Conversation != nil {
		systemPrompt = strings.TrimSpace(s.cfg.Conversation.SessionWikiPrompt)
	}
	if systemPrompt == "" {
		systemPrompt = defaultSessionWikiPrompt
	}
	thinking := false
	response, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: renderConversation(session, messages)},
	}, &chat.ChatOptions{Temperature: 0.2, Thinking: &thinking})
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nil
	}
	raw := extractJSONObject(agenttools.StripThinkBlocks(response.Content))
	if raw == "" {
		return nil, nil
	}
	var article types.SessionWikiArticle
	if err := json.Unmarshal([]byte(raw), &article); err != nil {
		logger.Warnf(ctx, "[Precipitate] wiki article JSON parse failed: %v", err)
		return nil, nil
	}
	article.Title = strings.TrimSpace(article.Title)
	article.Summary = strings.TrimSpace(article.Summary)
	article.Content = strings.TrimSpace(article.Content)
	if article.Content == "" {
		return nil, nil
	}
	return &article, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// resolveChatModel picks the distillation chat model: the explicit ID first,
// otherwise the first available KnowledgeQA model (same fallback as memory
// extraction and session title generation).
func (s *sessionPrecipitationService) resolveChatModel(ctx context.Context, modelID string) (chat.Chat, error) {
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
			return nil, fmt.Errorf("no KnowledgeQA model available for session distillation")
		}
	}
	return s.modelService.GetChatModel(ctx, modelID)
}

// resolveEmbeddingModelID returns the first available embedding model ID, or
// "" when none is configured (precipitation KB cannot be created then).
func (s *sessionPrecipitationService) resolveEmbeddingModelID(ctx context.Context) string {
	models, err := s.modelService.ListModels(ctx)
	if err != nil {
		return ""
	}
	for _, m := range models {
		if m != nil && m.Type == types.ModelTypeEmbedding {
			return m.ID
		}
	}
	return ""
}

// renderConversation renders the session into the distillation prompt's user
// message.
func renderConversation(session *types.Session, messages []*types.Message) string {
	var b strings.Builder
	title := strings.TrimSpace(session.Title)
	if title == "" {
		title = "未命名会话"
	}
	b.WriteString("Session title: ")
	b.WriteString(title)
	b.WriteString("\n\nConversation:\n")
	for _, m := range messages {
		if m == nil || (m.Role != "user" && m.Role != "assistant") {
			continue
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		if len(content) > precipitateMessageChars {
			content = content[:precipitateMessageChars] + "…"
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	return b.String()
}

// precipitatedTitle builds the knowledge document title carrying the session
// provenance marker: "会话沉淀 · <session title> · <id8>". The id suffix makes
// the title a stable dedup fallback even when Source was clobbered.
func precipitatedTitle(session *types.Session) string {
	title := strings.TrimSpace(session.Title)
	if title == "" {
		title = "未命名会话"
	}
	return fmt.Sprintf("会话沉淀 · %s · %s", title, shortSessionID(session.ID))
}

func shortSessionID(sessionID string) string {
	if len(sessionID) <= 8 {
		return sessionID
	}
	return sessionID[:8]
}

// mergeSourceRefs appends the session source ref unless already present.
func mergeSourceRefs(refs types.StringArray, ref string) types.StringArray {
	for _, r := range refs {
		if r == ref {
			return refs
		}
	}
	return append(refs, ref)
}
