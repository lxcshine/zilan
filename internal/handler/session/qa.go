package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	chatpipeline "github.com/Tencent/WeKnora/internal/application/service/chat_pipeline"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// qaRequestContext holds all the common data needed for QA requests
type qaRequestContext struct {
	ctx                   context.Context
	c                     *gin.Context
	sessionID             string
	requestID             string
	receivedAt            time.Time // Wall-clock time the handler started processing the request
	query                 string
	session               *types.Session
	customAgent           *types.CustomAgent
	assistantMessage      *types.Message
	knowledgeBaseIDs      []string
	knowledgeIDs          []string
	tagScopes             []types.TagScope
	tagIDs                []string
	mcpServiceIDs         []string
	skillNames            []string
	summaryModelID        string
	webSearchEnabled      bool
	mentionedItems        types.MentionedItems
	effectiveTenantID     uint64                   // when using shared agent, tenant ID for model/KB/MCP resolution; 0 = use context tenant
	images                []ImageAttachment        // Uploaded images with analysis text
	userMessageID         string                   // Created user message ID (populated after createUserMessage)
	channel               string                   // Source channel: "web", "api", "im", etc.
	attachments           types.MessageAttachments // Processed base64 file attachments (legacy inline uploads)
	attachmentIDs         []string                 // Pre-uploaded session-scoped document IDs, resolved after SSE starts
	attachmentMetas       types.MessageAttachments // Metadata-only view of attachmentIDs for the persisted user message
	suggestionAttribution *types.SuggestionAttribution

	// Snapshot of the request fields needed to persist the input-bar state
	// for session restoration. Kept verbatim from the request so we record
	// what the user had selected on the UI (not server-side resolutions).
	reqAgentEnabled bool
	reqAgentID      string
}

// buildQARequest converts the qaRequestContext into a types.QARequest for service invocation.
func (rc *qaRequestContext) buildQARequest() *types.QARequest {
	imageURLs, imageDescription := extractImageURLsAndOCRText(rc.images)
	return &types.QARequest{
		Session:            rc.session,
		Query:              rc.query,
		AssistantMessageID: rc.assistantMessage.ID,
		SummaryModelID:     rc.summaryModelID,
		CustomAgent:        rc.customAgent,
		KnowledgeBaseIDs:   rc.knowledgeBaseIDs,
		KnowledgeIDs:       rc.knowledgeIDs,
		TagScopes:          rc.tagScopes,
		MCPServiceIDs:      rc.mcpServiceIDs,
		SkillNames:         rc.skillNames,
		ImageURLs:          imageURLs,
		ImageDescription:   imageDescription,
		UserMessageID:      rc.userMessageID,
		WebSearchEnabled:   rc.webSearchEnabled,
		Attachments:        rc.attachments,
	}
}

// parseQARequest parses and validates a QA request, returns the request context
func (h *Handler) parseQARequest(c *gin.Context, logPrefix string) (*qaRequestContext, *CreateKnowledgeQARequest, error) {
	receivedAt := time.Now()
	ctx := logger.CloneContext(c.Request.Context())
	requestID := secutils.SanitizeForLog(c.GetString(types.RequestIDContextKey.String()))
	logger.Infof(ctx, "[%s] TTFB:start request_id=%s received_at=%d",
		logPrefix, requestID, receivedAt.UnixMilli())

	// Get session ID from URL parameter
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	if sessionID == "" {
		logger.Error(ctx, "Session ID is empty")
		return nil, nil, errors.NewBadRequestError(errors.ErrInvalidSessionID.Error())
	}

	// Parse request body
	var request CreateKnowledgeQARequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		return nil, nil, errors.NewBadRequestError(err.Error())
	}

	// Validate query content
	if request.Query == "" {
		logger.Error(ctx, "Query content is empty")
		return nil, nil, errors.NewBadRequestError("Query content cannot be empty")
	}
	if h.suggestionService != nil && request.SuggestionAttribution != nil {
		if err := h.suggestionService.ValidateAttribution(ctx, sessionID, request.Query, request.SuggestionAttribution); err != nil {
			return nil, nil, errors.NewBadRequestError("invalid suggestion attribution")
		}
	}

	// SSRF protection: strip client-supplied URL/Caption fields from image attachments.
	// The URL field must only be populated server-side by saveImageAttachments; an
	// attacker could inject internal network URLs to trigger SSRF via the LLM provider.
	for i := range request.Images {
		request.Images[i].URL = ""
		request.Images[i].Caption = ""
	}

	// Log request details
	if requestJSON, err := json.Marshal(request); err == nil {
		logger.Infof(ctx, "[%s] Request: session_id=%s, request=%s",
			logPrefix, sessionID, secutils.SanitizeForLog(secutils.CompactImageDataURLForLog(string(requestJSON))))
	}

	// Get session. QA writes new messages into the session, so use the strict
	// owner scope: a tenant admin may read an API-key session but must not be
	// able to post messages to it (which would otherwise fail later at message
	// creation with a 500 instead of a clean not-found).
	session, err := h.sessionService.GetOwnedSession(ctx, sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session, session ID: %s, error: %v", sessionID, err)
		return nil, nil, errors.NewNotFoundError("Session not found")
	}

	// Get custom agent if agent_id is provided. Backend resolves shared agent from share relation (no client-provided tenant).
	customAgent, effectiveTenantID := h.resolveAgent(ctx, c, request.AgentID)

	// Merge @mentioned items into knowledge_base_ids and knowledge_ids
	kbIDs, knowledgeIDs := mergeKnowledgeTargets(request.KnowledgeBaseIDs, request.KnowledgeIds, request.MentionedItems)
	if err := types.AuthorizeTenantAPIKeyKnowledgeTargets(ctx, kbIDs, knowledgeIDs); err != nil {
		return nil, nil, err
	}

	// The built-in wiki fixer is invoked from a KB page, not from a tenant's
	// regular agent picker. When the KB is shared, run it in the source tenant
	// only if the caller has edit permission, so KB-scoped models/tools resolve
	// without granting viewers write capability.
	if customAgent != nil && customAgent.ID == types.BuiltinWikiFixerID {
		if scopedAgent, scopedTenantID := h.resolveWikiFixerTenantScope(
			ctx,
			customAgent,
			c.GetUint64(types.TenantIDContextKey.String()),
			types.TenantRoleFromContext(ctx),
			kbIDs,
		); scopedTenantID != 0 {
			customAgent = scopedAgent
			effectiveTenantID = scopedTenantID
		}
	}

	// Log merge results for debugging
	logger.Infof(ctx, "[%s] @mention merge: request.KnowledgeBaseIDs=%v, request.MentionedItems=%d, merged kbIDs=%v, merged knowledgeIDs=%v",
		logPrefix, request.KnowledgeBaseIDs, len(request.MentionedItems), kbIDs, knowledgeIDs)

	// Process inline base64 images: decode and save to storage.
	// VLM analysis for RAG paths is deferred to the pipeline rewrite step.
	// For pure chat paths with non-vision models, VLM analysis runs here as fallback.
	if len(request.Images) > 0 {
		if customAgent == nil || !customAgent.Config.ImageUploadEnabled {
			logger.Warnf(ctx, "[%s] Image upload is not enabled for this agent, rejecting %d images", logPrefix, len(request.Images))
			return nil, nil, errors.NewBadRequestError("Image upload is not enabled for this agent")
		}
		tenantID := c.GetUint64(types.TenantIDContextKey.String())
		agentStorageProvider := customAgent.Config.ImageStorageProvider
		if err := h.saveImageAttachments(ctx, request.Images, tenantID, agentStorageProvider); err != nil {
			logger.Errorf(ctx, "[%s] Failed to save images: %v", logPrefix, err)
			return nil, nil, errors.NewBadRequestError(fmt.Sprintf("Image save failed: %v", err))
		}

		// VLM analysis is always deferred to after SSE stream is up:
		// - Agent mode: runs in async execution flow with tool_call/tool_result events
		// - Normal RAG mode: runs in the pipeline rewrite step with progress events
		// - Normal pure-chat mode: runs in the async goroutine with progress events
	}

	// 附件（PRD FR-22 ①）：上传只做"持久化源文件 + 建 TemporaryDocument + 入 asynq 队列"，
	// 解析交给后台 worker，不阻塞对话开始。
	//
	// 失败语义因此发生变化：以前任一附件解析失败会让整轮返回 400；现在解析发生在
	// 请求返回之后，失败只落到该附件自己的 Status=Failed，由流式事件告知用户，
	// 后续 resolveTemporaryAttachments / describeTemporaryAttachmentsForAgent 会跳过它。
	// 仍然返回 400 的只有确定性的请求级错误：体积超限、base64 解码失败、
	// 以及连源文件都没能存下来（文件名非法 / 类型不支持 / 存储不可用）。
	var attachmentIDs []string
	var attachmentMetas types.MessageAttachments
	if len(request.AttachmentUploads) > 0 {
		logger.Infof(ctx, "[%s] persisting %d attachment(s) for background parsing",
			logPrefix, len(request.AttachmentUploads))
		// 与会话同租户：后面的 hasPendingAttachments / ResolveForPrompt 都按
		// session.TenantID 查文档，创建时就必须落在同一个租户下。
		var persistErr error
		attachmentIDs, attachmentMetas, persistErr = h.persistAttachmentUploads(
			ctx, session.TenantID, sessionID, request.AttachmentUploads, customAgent,
		)
		if persistErr != nil {
			logger.Errorf(ctx, "[%s] attachment upload failed: %v", logPrefix, persistErr)
			return nil, nil, errors.NewBadRequestError(persistErr.Error())
		}
		logger.Infof(ctx, "[%s] %d attachment(s) queued for background parsing", logPrefix, len(attachmentIDs))
	}

	// Pre-uploaded documents may still be parsing. Only fetch their metadata
	// here (fast, available even while processing) to validate agent file-type
	// limits and to record the attachments on the persisted user message. The
	// heavy content selection happens after the SSE stream is up, so the send
	// is not blocked while parsing finishes (see resolveTemporaryAttachments).
	if len(request.AttachmentIDs) > 0 {
		normalizedIDs, normErr := normalizeTemporaryAttachmentIDs(request.AttachmentIDs)
		if normErr != nil {
			return nil, nil, errors.NewBadRequestError(normErr.Error())
		}
		tenantID := session.TenantID
		for _, id := range normalizedIDs {
			doc, getErr := h.temporaryDocuments.Get(ctx, tenantID, sessionID, id)
			if getErr != nil || doc == nil {
				return nil, nil, errors.NewBadRequestError(
					fmt.Sprintf("attachment %s was not found in this session", secutils.SanitizeForLog(id)))
			}
			if customAgent != nil && len(customAgent.Config.SupportedFileTypes) > 0 {
				ext := strings.TrimPrefix(strings.ToLower(doc.FileType), ".")
				if !containsFileType(customAgent.Config.SupportedFileTypes, ext) {
					return nil, nil, errors.NewBadRequestError(
						fmt.Sprintf("file type %s is not supported by this agent", ext))
				}
			}
			attachmentIDs = append(attachmentIDs, doc.ID)
			attachmentMetas = append(attachmentMetas, types.MessageAttachment{
				ID: doc.ID, URL: doc.ResourceRef, FileName: doc.FileName,
				FileType: doc.FileType, FileSize: doc.FileSize,
			})
		}
	}

	// 两条上传路径合并后统一去重并限量，避免绕过 MaxTemporaryAttachmentsPerMessage。
	if len(attachmentIDs) > 0 {
		mergedIDs, mergeErr := normalizeTemporaryAttachmentIDs(attachmentIDs)
		if mergeErr != nil {
			return nil, nil, errors.NewBadRequestError(mergeErr.Error())
		}
		keep := make(map[string]struct{}, len(mergedIDs))
		for _, id := range mergedIDs {
			keep[id] = struct{}{}
		}
		mergedMetas := make(types.MessageAttachments, 0, len(mergedIDs))
		seen := make(map[string]struct{}, len(mergedIDs))
		for _, meta := range attachmentMetas {
			if _, ok := keep[meta.ID]; !ok {
				continue
			}
			if _, dup := seen[meta.ID]; dup {
				continue
			}
			seen[meta.ID] = struct{}{}
			mergedMetas = append(mergedMetas, meta)
		}
		attachmentIDs, attachmentMetas = mergedIDs, mergedMetas
	}

	mentionScopes := tagScopesFromMentionedItems(request.MentionedItems)
	requestTagIDs := dedupRequestStrings(request.TagIDs)
	if err := validateUnscopedTagIDs(orphanTagIDsForScope(requestTagIDs, mentionScopes), secutils.SanitizeForLogArray(kbIDs)); err != nil {
		return nil, nil, errors.NewBadRequestError(err.Error())
	}
	tagScopes := mergeTagScopesFromRequestIDs(mentionScopes, requestTagIDs, secutils.SanitizeForLogArray(kbIDs))
	tagIDs := dedupRequestStrings(append(request.TagIDs, mentionedIDsByType(request.MentionedItems, "tag")...))
	mcpServiceIDs := dedupRequestStrings(append(request.MCPServiceIDs, mentionedIDsByType(request.MentionedItems, "mcp")...))
	skillNames := dedupRequestStrings(append(request.SkillNames, mentionedIDsByType(request.MentionedItems, "skill")...))
	executionContext, agentID, agentTenantID, modelID := buildMessageExecutionContext(
		ctx,
		customAgent,
		effectiveTenantID,
		request.SummaryModelID,
		secutils.SanitizeForLogArray(kbIDs),
		secutils.SanitizeForLogArray(knowledgeIDs),
		secutils.SanitizeForLogArray(tagIDs),
		tagScopes,
		secutils.SanitizeForLogArray(mcpServiceIDs),
		secutils.SanitizeForLogArray(skillNames),
		request.WebSearchEnabled,
	)

	// Build request context
	reqCtx := &qaRequestContext{
		ctx:         ctx,
		c:           c,
		sessionID:   sessionID,
		requestID:   requestID,
		receivedAt:  receivedAt,
		query:       request.Query,
		session:     session,
		customAgent: customAgent,
		assistantMessage: &types.Message{
			SessionID:        sessionID,
			Role:             "assistant",
			RequestID:        c.GetString(types.RequestIDContextKey.String()),
			IsCompleted:      false,
			Channel:          request.Channel,
			AgentID:          agentID,
			AgentTenantID:    agentTenantID,
			ModelID:          modelID,
			ExecutionContext: executionContext,
		},
		knowledgeBaseIDs:      secutils.SanitizeForLogArray(kbIDs),
		knowledgeIDs:          secutils.SanitizeForLogArray(knowledgeIDs),
		tagScopes:             tagScopes,
		tagIDs:                secutils.SanitizeForLogArray(tagIDs),
		mcpServiceIDs:         secutils.SanitizeForLogArray(mcpServiceIDs),
		skillNames:            secutils.SanitizeForLogArray(skillNames),
		summaryModelID:        secutils.SanitizeForLog(request.SummaryModelID),
		webSearchEnabled:      request.WebSearchEnabled,
		mentionedItems:        convertMentionedItems(request.MentionedItems),
		effectiveTenantID:     effectiveTenantID,
		images:                request.Images,
		channel:               request.Channel,
		attachmentIDs:         attachmentIDs,
		attachmentMetas:       attachmentMetas,
		suggestionAttribution: request.SuggestionAttribution,
		reqAgentEnabled:       request.AgentEnabled,
		reqAgentID:            request.AgentID,
	}

	return reqCtx, &request, nil
}

func buildMessageExecutionContext(
	ctx context.Context,
	agent *types.CustomAgent,
	effectiveTenantID uint64,
	modelOverride string,
	knowledgeBaseIDs []string,
	knowledgeIDs []string,
	tagIDs []string,
	tagScopes []types.TagScope,
	mcpServiceIDs []string,
	skillNames []string,
	webSearchEnabled bool,
) (types.MessageExecutionContext, string, uint64, string) {
	locale, ok := types.LanguageFromContext(ctx)
	if !ok {
		locale = types.DefaultLanguage()
	}

	snapshot := types.MessageExecutionContext{
		KnowledgeBaseIDs: knowledgeBaseIDs,
		KnowledgeIDs:     knowledgeIDs,
		TagIDs:           tagIDs,
		TagScopes:        cloneTagScopes(tagScopes),
		MCPServiceIDs:    mcpServiceIDs,
		SkillNames:       skillNames,
		WebSearchEnabled: webSearchEnabled,
		Locale:           locale,
	}
	if agent == nil {
		return snapshot, "", effectiveTenantID, modelOverride
	}

	modelID := modelOverride
	if modelID == "" {
		modelID = agent.Config.ModelID
	}
	agentTenantID := effectiveTenantID
	if agentTenantID == 0 {
		agentTenantID = agent.TenantID
	}

	// Marshal/unmarshal gives the snapshot independent backing slices, so a
	// later agent edit cannot mutate an in-flight message context.
	if agent.Config.QuestionSuggestions != nil {
		if encoded, err := json.Marshal(agent.Config.QuestionSuggestions); err == nil {
			var suggestions types.QuestionSuggestionConfig
			if json.Unmarshal(encoded, &suggestions) == nil {
				snapshot.QuestionSuggestions = &suggestions
			}
		}
	}
	hashInput := struct {
		QuestionSuggestions *types.QuestionSuggestionConfig `json:"question_suggestions,omitempty"`
		KnowledgeBaseIDs    []string                        `json:"knowledge_base_ids,omitempty"`
		KnowledgeIDs        []string                        `json:"knowledge_ids,omitempty"`
		TagIDs              []string                        `json:"tag_ids,omitempty"`
		TagScopes           []types.TagScope                `json:"tag_scopes,omitempty"`
		ModelID             string                          `json:"model_id,omitempty"`
	}{
		QuestionSuggestions: snapshot.QuestionSuggestions,
		KnowledgeBaseIDs:    knowledgeBaseIDs,
		KnowledgeIDs:        knowledgeIDs,
		TagIDs:              tagIDs,
		TagScopes:           snapshot.TagScopes,
		ModelID:             modelID,
	}
	if encoded, err := json.Marshal(hashInput); err == nil {
		hash := sha256.Sum256(encoded)
		snapshot.AgentConfigHash = fmt.Sprintf("%x", hash[:])
	}

	return snapshot, agent.ID, agentTenantID, modelID
}

func cloneTagScopes(scopes []types.TagScope) []types.TagScope {
	if len(scopes) == 0 {
		return nil
	}
	cloned := make([]types.TagScope, 0, len(scopes))
	for _, scope := range scopes {
		if scope.KnowledgeBaseID == "" || len(scope.TagIDs) == 0 {
			continue
		}
		cloned = append(cloned, types.TagScope{
			KnowledgeBaseID: scope.KnowledgeBaseID,
			TagIDs:          append([]string(nil), scope.TagIDs...),
		})
	}
	return cloned
}

// resolveAgent resolves the custom agent by ID, trying shared agent first, then own agent.
// Returns (nil, 0) if agentID is empty or not found.
func (h *Handler) resolveAgent(ctx context.Context, c *gin.Context, agentID string) (*types.CustomAgent, uint64) {
	if agentID == "" {
		return nil, 0
	}

	logger.Infof(ctx, "Resolving agent, agent ID: %s", secutils.SanitizeForLog(agentID))

	// Try shared agent first
	var customAgent *types.CustomAgent
	var effectiveTenantID uint64
	userIDVal, _ := c.Get(types.UserIDContextKey.String())
	currentTenantID := c.GetUint64(types.TenantIDContextKey.String())
	if h.agentShareService != nil && userIDVal != nil && currentTenantID != 0 {
		callerTenantRole := types.TenantRoleFromContext(ctx)
		agent, err := h.agentShareService.GetSharedAgentForTenant(ctx, currentTenantID, callerTenantRole, agentID)
		if err == nil && agent != nil {
			effectiveTenantID = agent.TenantID
			customAgent = agent
			logger.Infof(ctx, "Using shared agent: ID=%s, Name=%s, effectiveTenantID=%d (retrieval scope)",
				customAgent.ID, customAgent.Name, effectiveTenantID)
		}
	}

	// Fall back to own agent
	if customAgent == nil {
		agent, err := h.customAgentService.GetAgentByID(ctx, agentID)
		if err == nil {
			customAgent = agent
			logger.Infof(ctx, "Using own agent: ID=%s, Name=%s, AgentMode=%s",
				customAgent.ID, customAgent.Name, customAgent.Config.AgentMode)
		} else {
			logger.Warnf(ctx, "Failed to get custom agent, agent ID: %s, error: %v, using default config",
				secutils.SanitizeForLog(agentID), err)
		}
	} else {
		logger.Infof(ctx, "Using custom agent: ID=%s, Name=%s, IsBuiltin=%v, AgentMode=%s, effectiveTenantID=%d",
			customAgent.ID, customAgent.Name, customAgent.IsBuiltin, customAgent.Config.AgentMode, effectiveTenantID)
	}

	return customAgent, effectiveTenantID
}

// mergeKnowledgeTargets merges request KB/knowledge IDs with @mentioned items into deduplicated slices.
func mergeKnowledgeTargets(requestKBIDs []string, requestKnowledgeIDs []string, mentionedItems []MentionedItemRequest) (kbIDs []string, knowledgeIDs []string) {
	kbIDSet := make(map[string]bool)
	kbIDs = make([]string, 0, len(requestKBIDs)+len(mentionedItems))
	for _, id := range requestKBIDs {
		if id != "" && !kbIDSet[id] {
			kbIDs = append(kbIDs, id)
			kbIDSet[id] = true
		}
	}

	knowledgeIDSet := make(map[string]bool)
	knowledgeIDs = make([]string, 0, len(requestKnowledgeIDs)+len(mentionedItems))
	for _, id := range requestKnowledgeIDs {
		if id != "" && !knowledgeIDSet[id] {
			knowledgeIDs = append(knowledgeIDs, id)
			knowledgeIDSet[id] = true
		}
	}

	for _, item := range mentionedItems {
		if item.ID == "" {
			continue
		}
		switch item.Type {
		case "kb":
			if !kbIDSet[item.ID] {
				kbIDs = append(kbIDs, item.ID)
				kbIDSet[item.ID] = true
			}
		case "file":
			if !knowledgeIDSet[item.ID] {
				knowledgeIDs = append(knowledgeIDs, item.ID)
				knowledgeIDSet[item.ID] = true
			}
		}
	}
	return kbIDs, knowledgeIDs
}

// sseStreamContext holds the context for SSE streaming
type sseStreamContext struct {
	eventBus         *event.EventBus
	asyncCtx         context.Context
	cancel           context.CancelFunc
	assistantMessage *types.Message
}

// setupSSEStream sets up the SSE streaming context
func (h *Handler) setupSSEStream(reqCtx *qaRequestContext, generateTitle bool) *sseStreamContext {
	// Set SSE headers
	setSSEHeaders(reqCtx.c)

	// Write initial agent_query event
	h.writeAgentQueryEvent(reqCtx.ctx, reqCtx.sessionID, reqCtx.assistantMessage.ID)

	// Base context for async work: when using shared agent, use source tenant for model/KB/MCP resolution
	baseCtx := reqCtx.ctx
	if reqCtx.effectiveTenantID != 0 && h.tenantService != nil {
		if tenant, err := h.tenantService.GetTenantByID(reqCtx.ctx, reqCtx.effectiveTenantID); err == nil && tenant != nil {
			baseCtx = context.WithValue(context.WithValue(reqCtx.ctx, types.TenantIDContextKey, reqCtx.effectiveTenantID), types.TenantInfoContextKey, tenant)
			logger.Infof(reqCtx.ctx, "Using effective tenant %d for shared agent (model/KB/MCP)", reqCtx.effectiveTenantID)
		}
	}

	// Create EventBus and cancellable context
	eventBus := event.NewEventBus()
	asyncCtx, cancel := context.WithCancel(logger.CloneContext(baseCtx))

	streamCtx := &sseStreamContext{
		eventBus:         eventBus,
		asyncCtx:         asyncCtx,
		cancel:           cancel,
		assistantMessage: reqCtx.assistantMessage,
	}

	// Setup stop event handler
	h.setupStopEventHandler(eventBus, reqCtx.sessionID, reqCtx.session.TenantID, reqCtx.assistantMessage, cancel)

	// Watch for stop events independently of the client SSE connection so a
	// user-requested stop reliably cancels generation even when the client
	// has already disconnected (e.g. API-Key callers that close the stream
	// before POSTing /stop). The watcher self-terminates on a terminal stream
	// event, so its lifetime is decoupled from when the QA service call
	// returns (KnowledgeQA returns immediately while streaming continues in a
	// background goroutine, whereas AgentQA blocks until done). Use a
	// connection-independent context derived from baseCtx so it survives the
	// client disconnect.
	h.startStopWatcher(logger.CloneContext(baseCtx), reqCtx.sessionID, reqCtx.assistantMessage.ID, eventBus)

	// Setup stream handler
	h.setupStreamHandler(asyncCtx, reqCtx.sessionID, reqCtx.assistantMessage.ID,
		reqCtx.requestID, reqCtx.receivedAt, reqCtx.assistantMessage, eventBus)

	// Generate title if needed
	if generateTitle && reqCtx.session.Title == "" {
		// Use the same model as the conversation for title generation
		modelID := ""
		if reqCtx.customAgent != nil && reqCtx.customAgent.Config.ModelID != "" {
			modelID = reqCtx.customAgent.Config.ModelID
		}
		logger.Infof(reqCtx.ctx, "Session has no title, starting async title generation, session ID: %s, model: %s", reqCtx.sessionID, modelID)
		h.sessionService.GenerateTitleAsync(asyncCtx, reqCtx.session, reqCtx.query, modelID, eventBus)
	}

	return streamCtx
}

// SearchKnowledge godoc
// @Summary      知识搜索
// @Description  在知识库中搜索（不使用LLM总结）
// @Tags         问答
// @Accept       json
// @Produce      json
// @Param        request  body      SearchKnowledgeRequest  true  "搜索请求"
// @Success      200      {object}  map[string]interface{}  "搜索结果"
// @Failure      400      {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/search [post]
func (h *Handler) SearchKnowledge(c *gin.Context) {
	ctx := logger.CloneContext(c.Request.Context())
	logger.Info(ctx, "Start processing knowledge search request")

	// Parse request body
	var request SearchKnowledgeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	// Validate request parameters
	if request.Query == "" {
		logger.Error(ctx, "Query content is empty")
		c.Error(errors.NewBadRequestError("Query content cannot be empty"))
		return
	}

	// Merge single knowledge_base_id into knowledge_base_ids for backward compatibility
	knowledgeBaseIDs := request.KnowledgeBaseIDs
	if request.KnowledgeBaseID != "" {
		// Check if it's already in the list to avoid duplicates
		found := false
		for _, id := range knowledgeBaseIDs {
			if id == request.KnowledgeBaseID {
				found = true
				break
			}
		}
		if !found {
			knowledgeBaseIDs = append(knowledgeBaseIDs, request.KnowledgeBaseID)
		}
	}

	mentionScopes := tagScopesFromMentionedItems(request.MentionedItems)
	requestTagIDs := dedupRequestStrings(request.TagIDs)
	if err := validateUnscopedTagIDs(orphanTagIDsForScope(requestTagIDs, mentionScopes), secutils.SanitizeForLogArray(knowledgeBaseIDs)); err != nil {
		logger.Error(ctx, err.Error())
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}
	tagScopes := mergeTagScopesFromRequestIDs(mentionScopes, requestTagIDs, secutils.SanitizeForLogArray(knowledgeBaseIDs))

	if len(knowledgeBaseIDs) == 0 && len(request.KnowledgeIDs) == 0 && len(tagScopes) == 0 {
		logger.Error(ctx, "No knowledge base IDs, knowledge IDs, or tag scopes provided")
		c.Error(errors.NewBadRequestError("At least one knowledge_base_id, knowledge_base_ids, knowledge_ids, or scoped tag must be provided"))
		return
	}
	if err := types.AuthorizeTenantAPIKeyKnowledgeTargets(ctx, knowledgeBaseIDs, request.KnowledgeIDs); err != nil {
		c.Error(err)
		return
	}

	logger.Infof(
		ctx,
		"Knowledge search request, knowledge base IDs: %v, knowledge IDs: %v, tag scopes: %d, query: %s",
		secutils.SanitizeForLogArray(knowledgeBaseIDs),
		secutils.SanitizeForLogArray(request.KnowledgeIDs),
		len(tagScopes),
		secutils.SanitizeForLog(request.Query),
	)

	// Directly call knowledge retrieval service without LLM summarization
	searchResults, err := h.sessionService.SearchKnowledge(ctx, knowledgeBaseIDs, request.KnowledgeIDs, tagScopes, request.Query)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	logger.Infof(ctx, "Knowledge search completed, found %d results", len(searchResults))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    searchResults,
	})
}

// KnowledgeQA godoc
// @Summary      知识问答
// @Description  基于知识库的问答（使用LLM总结），支持SSE流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string                   true  "会话ID"
// @Param        request     body      CreateKnowledgeQARequest true  "问答请求"
// @Success      200         {object}  map[string]interface{}   "问答结果（SSE流）"
// @Failure      400         {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/knowledge-qa [post]
func (h *Handler) KnowledgeQA(c *gin.Context) {
	// Parse and validate request
	reqCtx, request, err := h.parseQARequest(c, "KnowledgeQA")
	if err != nil {
		c.Error(err)
		return
	}

	// Execute normal mode QA, generate title unless disabled
	h.executeQA(reqCtx, qaModeNormal, !request.DisableTitle)
}

// AgentQA godoc
// @Summary      Agent问答
// @Description  基于Agent的智能问答，支持多轮对话和SSE流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string                   true  "会话ID"
// @Param        request     body      CreateKnowledgeQARequest true  "问答请求"
// @Success      200         {object}  map[string]interface{}   "问答结果（SSE流）"
// @Failure      400         {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/agent-qa [post]
func (h *Handler) AgentQA(c *gin.Context) {
	// Parse and validate request
	reqCtx, request, err := h.parseQARequest(c, "AgentQA")
	if err != nil {
		c.Error(err)
		return
	}

	// Determine if agent mode should be enabled
	// Priority: customAgent.IsAgentMode() > request.AgentEnabled
	agentModeEnabled := request.AgentEnabled
	if reqCtx.customAgent != nil {
		agentModeEnabled = reqCtx.customAgent.IsAgentMode()
		logger.Infof(reqCtx.ctx, "Agent mode determined by custom agent: %v (config.agent_mode=%s)",
			agentModeEnabled, reqCtx.customAgent.Config.AgentMode)
	}

	// Sanity gate: agent mode requires a resolved CustomAgent. If we got
	// here with agent_enabled=true but agent_id missing/unresolvable, the
	// AgentQA service will fail deep inside the async goroutine with a
	// generic "custom agent configuration is required" error and the user
	// just sees a broken stream. Reject early with a clear 400 so the
	// frontend can recover (e.g. fall back to quick-answer). Most likely
	// cause is a stale localStorage settings blob where selectedAgentId
	// got blanked but isAgentEnabled stayed true — usually after a
	// cross-tenant switch where the previously selected agent is no
	// longer visible.
	if agentModeEnabled && reqCtx.customAgent == nil {
		logger.Warnf(reqCtx.ctx,
			"Agent mode requested without a resolvable agent_id, rejecting; session=%s, request.AgentID=%q",
			reqCtx.sessionID, secutils.SanitizeForLog(request.AgentID))
		c.Error(errors.NewBadRequestError(
			"agent_id is required when agent mode is enabled"))
		return
	}

	// Route to appropriate handler based on agent mode
	if agentModeEnabled {
		h.executeQA(reqCtx, qaModeAgent, true)
	} else {
		logger.Infof(reqCtx.ctx, "Agent mode disabled, delegating to normal mode for session: %s", reqCtx.sessionID)
		h.executeQA(reqCtx, qaModeNormal, !request.DisableTitle)
	}
}

// qaMode determines which QA execution path to use.
type qaMode int

const (
	qaModeNormal qaMode = iota // KnowledgeQA pipeline (RAG / pure chat)
	qaModeAgent                // Agent engine with tool calling
)

// executeQA is the unified execution flow for both KnowledgeQA and AgentQA modes.
// It handles message creation, SSE setup, VLM analysis, service invocation, and error handling.
func (h *Handler) executeQA(reqCtx *qaRequestContext, mode qaMode, generateTitle bool) {
	ctx := reqCtx.ctx
	sessionID := reqCtx.sessionID

	// Persist the input-bar state used for this request so reopening the
	// session can rehydrate agent / model / KB / web-search / MCP selections.
	// This is a pure UI memo (no behavioural effect) and runs in a goroutine
	// to avoid adding a DB round-trip to TTFB. Use WithoutCancel so a fast
	// client disconnect doesn't drop the write.
	go h.persistLastRequestState(ctx, reqCtx, mode)

	// Agent mode: emit agent query event before message creation
	if mode == qaModeAgent {
		if err := event.Emit(ctx, event.Event{
			Type:      event.EventAgentQuery,
			SessionID: sessionID,
			RequestID: reqCtx.requestID,
			Data: event.AgentQueryData{
				SessionID: sessionID,
				Query:     reqCtx.query,
				RequestID: reqCtx.requestID,
			},
		}); err != nil {
			logger.Errorf(ctx, "Failed to emit agent query event: %v", err)
			return
		}
	}

	// Create user message. Include pre-uploaded document metadata so history
	// reload shows the attachments even though their content is selected later.
	userMessageAttachments := reqCtx.attachments
	if len(reqCtx.attachmentMetas) > 0 {
		userMessageAttachments = append(append(types.MessageAttachments{}, reqCtx.attachments...), reqCtx.attachmentMetas...)
	}
	userMsg, err := h.createUserMessage(ctx, sessionID, reqCtx.query, reqCtx.requestID, reqCtx.mentionedItems, convertImageAttachments(reqCtx.images), userMessageAttachments, reqCtx.channel, reqCtx.suggestionAttribution)
	if err != nil {
		reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		return
	}
	reqCtx.userMessageID = userMsg.ID

	// Create assistant message
	assistantMessagePtr, err := h.createAssistantMessage(ctx, reqCtx.assistantMessage)
	if err != nil {
		reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		return
	}
	reqCtx.assistantMessage = assistantMessagePtr

	if mode == qaModeNormal {
		logger.Infof(ctx, "Using knowledge bases: %v", reqCtx.knowledgeBaseIDs)
	} else {
		logger.Infof(ctx, "Calling agent QA service, session ID: %s", sessionID)
	}

	// Setup SSE stream
	streamCtx := h.setupSSEStream(reqCtx, generateTitle)

	// Normal mode: register completion handler on EventAgentFinalAnswer
	// (Agent mode handles completion in the defer block instead)
	if mode == qaModeNormal {
		var completionHandled bool

		// 本轮的 token 用量与上下文归因（PRD FR-12）。chat pipeline 通过
		// chat.complete 事件送来（该事件没有其它订阅者，不会污染 SSE 流），这里先写到
		// assistant 消息的内存态上——采集侧保证它早于 answer(Done) 发出，因此赶得上
		// completeAssistantMessage 的那次落库——再转挂到 agent.complete 的 Extra 上，
		// 于是流式 complete 事件也能把 usage 推给前端。
		var turnUsage *types.TokenUsage
		var turnStats *types.ContextTurnStats

		streamCtx.eventBus.On(event.EventChatComplete, func(ctx context.Context, evt event.Event) error {
			data, ok := evt.Data.(types.TurnUsageData)
			if !ok {
				return nil
			}
			// 消息 ID 不匹配说明这份用量属于另一条 assistant 行；宁可丢掉这一轮的归因，
			// 也不能把数字写到错误的行上。空 ID 表示采集侧没拿到消息 ID，按本行处理。
			if data.MessageID != "" && data.MessageID != streamCtx.assistantMessage.ID {
				return nil
			}
			turnUsage, turnStats = data.Usage, data.ContextStats
			applyTurnUsage(streamCtx.assistantMessage, data.Usage, data.ContextStats)
			// 正常时序走不到这里：用量事件早于 answer(Done)，消息还没落库。用户中途停止
			// 等异常时序会让消息先完成，此时补一次定向更新，别让这一轮的归因整个丢掉。
			if streamCtx.assistantMessage.IsCompleted {
				h.persistTurnUsage(streamCtx.asyncCtx, streamCtx.assistantMessage)
			}
			return nil
		})

		// Persist reasoning_content into agent_steps so historical reload can
		// reconstruct the thinking card (same shape as Agent-mode steps).
		// Accumulate on assistantMessage directly so user-initiated stop also
		// keeps whatever reasoning had streamed before the cancel.
		streamCtx.eventBus.On(event.EventAgentThought, func(ctx context.Context, evt event.Event) error {
			data, ok := evt.Data.(event.AgentThoughtData)
			if !ok || data.Content == "" {
				return nil
			}
			appendQuickAnswerReasoning(streamCtx.assistantMessage, data.Content)
			return nil
		})

		streamCtx.eventBus.On(event.EventAgentFinalAnswer, func(ctx context.Context, evt event.Event) error {
			data, ok := evt.Data.(event.AgentFinalAnswerData)
			if !ok {
				return nil
			}
			streamCtx.assistantMessage.Content += data.Content
			if data.IsFallback {
				streamCtx.assistantMessage.IsFallback = true
			}
			if data.Done {
				if completionHandled {
					return nil
				}
				completionHandled = true

				logger.Infof(streamCtx.asyncCtx, "Knowledge QA service completed for session: %s", sessionID)
				updateCtx := context.WithValue(streamCtx.asyncCtx, types.TenantIDContextKey, reqCtx.session.TenantID)
				h.completeAssistantMessage(updateCtx, streamCtx.assistantMessage, reqCtx.query)
				h.enqueueMemoryExtraction(updateCtx, reqCtx, streamCtx.assistantMessage)

				// Soft retrieval-failure signal: the pipeline retrieved context
				// but the LLM explicitly refused to answer from it. Hard failures
				// (no usable context at all) are already handled inside the
				// service's fallback path, so only non-fallback answers are
				// checked here.
				if !streamCtx.assistantMessage.IsFallback &&
					chatpipeline.DetectRetrievalFailure(streamCtx.assistantMessage.Content) {
					h.sessionService.RecordRetrievalFailure(updateCtx, "", nil)
				}

				streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
					Type:      event.EventAgentComplete,
					SessionID: sessionID,
					Data: event.AgentCompleteData{
						FinalAnswer: streamCtx.assistantMessage.Content,
						// 把本轮用量转挂上去，流式 complete 事件才会带 usage（PRD FR-12）。
						// 刻意不带 MessageID：带上会让 AgentStreamHandler.handleComplete
						// 把 FinalAnswer 再追加一次到 Content 上（普通模式的答案已经由
						// 上面的 EventAgentFinalAnswer 累积过了），落库则已由
						// completeAssistantMessage 完成。
						Extra: types.NewTurnUsageExtra(turnUsage, turnStats),
					},
				})
			}
			return nil
		})
	}

	// Execute QA asynchronously
	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 10240)
				runtime.Stack(buf, true)
				stageName := "Knowledge QA"
				if mode == qaModeAgent {
					stageName = "Agent QA"
				}
				logger.ErrorWithFields(streamCtx.asyncCtx,
					errors.NewInternalServerError(fmt.Sprintf("%s service panicked: %v\n%s", stageName, r, string(buf))),
					map[string]interface{}{"session_id": sessionID})
			}
			// Agent mode: complete the assistant message in defer (normal mode does it via event handler)
			if mode == qaModeAgent {
				// Use WithoutCancel so a user-triggered stop (which cancels
				// asyncCtx) doesn't also cancel the GORM UPDATE that persists
				// AgentSteps/Content. Without this, cancelled-ctx makes
				// GORM skip the write and the agent's intermediate steps
				// (thinking / tool_call history) are lost on page refresh.
				updateCtx := context.WithValue(
					context.WithoutCancel(streamCtx.asyncCtx),
					types.TenantIDContextKey, reqCtx.session.TenantID,
				)
				h.completeAssistantMessage(updateCtx, streamCtx.assistantMessage, reqCtx.query)
				h.enqueueMemoryExtraction(updateCtx, reqCtx, streamCtx.assistantMessage)
				logger.Infof(streamCtx.asyncCtx, "Agent QA service completed for session: %s", sessionID)
			}
		}()

		// Resolve pre-uploaded attachments (may still be parsing): waits with a
		// timeline step so the send is not blocked, then injects content/images.
		// Agent 模式改为交付清单引用，由 Agent 自己按需读取（PRD FR-22 ②③）。
		if mode == qaModeAgent {
			h.describeTemporaryAttachmentsForAgent(streamCtx, reqCtx)
		} else {
			h.resolveTemporaryAttachments(streamCtx, reqCtx)
		}

		// Run VLM image analysis if applicable
		h.runVLMAnalysisIfNeeded(streamCtx, reqCtx, mode)

		// Build QA request and invoke the appropriate service
		qaReq := reqCtx.buildQARequest()

		var serviceErr error
		var stageName string
		if mode == qaModeNormal {
			stageName = "knowledge_qa_execution"
			serviceErr = h.sessionService.KnowledgeQA(streamCtx.asyncCtx, qaReq, streamCtx.eventBus)
		} else {
			stageName = "agent_execution"
			serviceErr = h.sessionService.AgentQA(streamCtx.asyncCtx, qaReq, streamCtx.eventBus)
		}

		if serviceErr != nil {
			// A user-requested stop cancels asyncCtx, which surfaces here as a
			// context cancellation. That is an expected outcome, not a failure:
			// the stop event already notifies the client, so don't emit a
			// spurious error event (which would otherwise show an error toast).
			if streamCtx.asyncCtx.Err() != nil {
				logger.Infof(streamCtx.asyncCtx, "QA cancelled by user stop for session: %s", sessionID)
			} else {
				logger.ErrorWithFields(streamCtx.asyncCtx, serviceErr, nil)
				streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
					Type:      event.EventError,
					SessionID: sessionID,
					Data: event.ErrorData{
						Error:     serviceErr.Error(),
						Stage:     stageName,
						SessionID: sessionID,
					},
				})
			}
		}
	}()

	// Handle SSE events (blocking)
	shouldWaitForTitle := generateTitle && reqCtx.session.Title == ""
	h.handleAgentEventsForSSE(ctx, reqCtx.c, sessionID, reqCtx.assistantMessage.ID,
		reqCtx.requestID, streamCtx.eventBus, shouldWaitForTitle)
}

// runVLMAnalysisIfNeeded runs VLM image analysis within the async goroutine,
// emitting tool_call/tool_result events so the user can see progress.
// For normal mode, VLM only runs on the pure-chat path (no KB, no web search);
// RAG paths defer VLM to the pipeline rewrite step.
// For agent mode, VLM always runs when images and a VLM model are present.
func (h *Handler) runVLMAnalysisIfNeeded(streamCtx *sseStreamContext, reqCtx *qaRequestContext, mode qaMode) {
	if len(reqCtx.images) == 0 || reqCtx.customAgent == nil || reqCtx.customAgent.Config.VLMModelID == "" {
		return
	}

	sessionID := reqCtx.sessionID

	// In normal mode, only run VLM for pure-chat path
	if mode == qaModeNormal {
		hasRequestKBs := len(reqCtx.knowledgeBaseIDs) > 0 || len(reqCtx.knowledgeIDs) > 0
		agentWillResolveKBs := false
		if !hasRequestKBs && reqCtx.customAgent != nil && !reqCtx.customAgent.Config.RetrieveKBOnlyWhenMentioned {
			switch reqCtx.customAgent.Config.KBSelectionMode {
			case "all":
				agentWillResolveKBs = true
			case "selected", "":
				agentWillResolveKBs = len(reqCtx.customAgent.Config.KnowledgeBases) > 0
			case "none":
				agentWillResolveKBs = false
			default:
				agentWillResolveKBs = len(reqCtx.customAgent.Config.KnowledgeBases) > 0
			}
		}
		if hasRequestKBs || agentWillResolveKBs || reqCtx.webSearchEnabled {
			return // VLM will be handled by the pipeline rewrite step
		}
	}

	// Emit VLM tool call/result events
	toolCallID := uuid.New().String()
	iteration := 0 // agent mode uses iteration field

	streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
		Type:      event.EventAgentToolCall,
		SessionID: sessionID,
		Data: event.AgentToolCallData{
			ToolCallID: toolCallID,
			ToolName:   "image_analysis",
			Iteration:  iteration,
		},
	})

	vlmStart := time.Now()
	h.analyzeImageAttachments(streamCtx.asyncCtx, reqCtx.images,
		reqCtx.customAgent.Config.VLMModelID, reqCtx.query)

	outputMsg := "已分析图片内容"
	if mode == qaModeAgent {
		outputMsg = "已查看图片内容"
	}
	streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
		Type:      event.EventAgentToolResult,
		SessionID: sessionID,
		Data: event.AgentToolResultData{
			ToolCallID: toolCallID,
			ToolName:   "image_analysis",
			Output:     outputMsg,
			Success:    true,
			Duration:   time.Since(vlmStart).Milliseconds(),
			Iteration:  iteration,
		},
	})
}

// defaultAttachmentParseWaitTimeout bounds how long a QA turn waits for
// still-parsing attachments before proceeding with only the finished ones.
// Large or scanned documents can exceed this; raise it via
// WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC when needed.
const defaultAttachmentParseWaitTimeout = 60 * time.Second

// attachmentParseWaitTimeout returns the configured wait timeout, honoring the
// WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC override (in seconds) and falling
// back to the default when unset or invalid.
func attachmentParseWaitTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultAttachmentParseWaitTimeout
}

// attachmentWaitTimeout 返回本轮等待附件解析完成的预算：Agent 级覆盖优先，
// 其次环境变量，最后默认值（PRD FR-22 ②）。
func (h *Handler) attachmentWaitTimeout(reqCtx *qaRequestContext) time.Duration {
	if reqCtx != nil && reqCtx.customAgent != nil &&
		reqCtx.customAgent.Config.AttachmentParseWaitTimeoutSec > 0 {
		return time.Duration(reqCtx.customAgent.Config.AttachmentParseWaitTimeoutSec) * time.Second
	}
	return attachmentParseWaitTimeout()
}

// resolveTemporaryAttachments selects prompt content for pre-uploaded documents
// after the SSE stream is live. When any attachment is still parsing it emits a
// "attachment_parsing" timeline step and waits (bounded); unfinished attachments
// are skipped rather than blocking or failing the whole turn.
func (h *Handler) resolveTemporaryAttachments(streamCtx *sseStreamContext, reqCtx *qaRequestContext) {
	if len(reqCtx.attachmentIDs) == 0 {
		return
	}
	ctx := streamCtx.asyncCtx
	sessionID := reqCtx.sessionID
	// Prefer the session's tenant over gin.Context: this runs in an async
	// goroutine after the HTTP handler may have returned.
	tenantID := reqCtx.session.TenantID

	start := time.Now()
	var toolCallID string
	if h.hasPendingAttachments(ctx, tenantID, sessionID, reqCtx.attachmentIDs) {
		toolCallID = uuid.New().String()
		streamCtx.eventBus.Emit(ctx, event.Event{
			Type:      event.EventAgentToolCall,
			SessionID: sessionID,
			Data: event.AgentToolCallData{
				ToolCallID: toolCallID,
				ToolName:   "attachment_parsing",
				Iteration:  0,
			},
		})
		h.waitForAttachments(ctx, tenantID, sessionID, reqCtx.attachmentIDs, h.attachmentWaitTimeout(reqCtx))
	}

	readyIDs, skipped := h.partitionReadyAttachments(ctx, tenantID, sessionID, reqCtx.attachmentIDs)

	var temporaryResult *types.TemporaryDocumentPromptResult
	var resolveErr error
	if len(readyIDs) > 0 {
		temporaryResult, resolveErr = h.temporaryDocuments.ResolveForPrompt(ctx, tenantID, sessionID, readyIDs, reqCtx.query)
	}

	if toolCallID != "" {
		output := fmt.Sprintf("已解析 %d 个附件", len(readyIDs))
		if skipped > 0 {
			output += fmt.Sprintf("，%d 个未完成已跳过", skipped)
		}
		success := resolveErr == nil
		if resolveErr != nil {
			output = fmt.Sprintf("附件解析失败: %v", resolveErr)
		}
		streamCtx.eventBus.Emit(ctx, event.Event{
			Type:      event.EventAgentToolResult,
			SessionID: sessionID,
			Data: event.AgentToolResultData{
				ToolCallID: toolCallID,
				ToolName:   "attachment_parsing",
				Output:     output,
				Success:    success,
				Duration:   time.Since(start).Milliseconds(),
				Iteration:  0,
				Data: map[string]interface{}{
					"display_type":  "attachment_parsing",
					"parsed_count":  len(readyIDs),
					"skipped_count": skipped,
				},
			},
		})
	}
	if resolveErr != nil || temporaryResult == nil {
		if resolveErr != nil {
			logger.Warnf(ctx, "temporary attachment resolution failed for session %s: %v", sessionID, resolveErr)
		}
		return
	}

	h.applyTemporaryAttachments(ctx, reqCtx, temporaryResult)
}

// describeTemporaryAttachmentsForAgent 把预上传文档的清单交给 Agent 轮次，
// 既不等待解析完成，也不按问题挑选内容（PRD FR-22 ③）。Agent 通过 read_file 的
// attachment:// 地址自己按需读取；只有真正需要某份还在解析的文档时才会短暂等待。
func (h *Handler) describeTemporaryAttachmentsForAgent(streamCtx *sseStreamContext, reqCtx *qaRequestContext) {
	if len(reqCtx.attachmentIDs) == 0 {
		return
	}
	ctx := streamCtx.asyncCtx
	// 独立上传的图片仍然要等：它们直接进视觉上下文，不走 read_file。
	h.awaitImageAttachmentsForAgent(streamCtx, reqCtx)
	result, err := h.temporaryDocuments.DescribeForAgent(
		ctx, reqCtx.session.TenantID, reqCtx.sessionID, reqCtx.attachmentIDs,
	)
	if err != nil || result == nil {
		if err != nil {
			logger.Warnf(ctx, "temporary attachment manifest failed for session %s: %v", reqCtx.sessionID, err)
		}
		return
	}
	h.applyTemporaryAttachments(ctx, reqCtx, result)
}

// awaitImageAttachmentsForAgent 只对"独立图片上传"等待解析完成；
// 文档内嵌的图片由 Agent 通过 read_file 按需读取，不在这里阻塞。
func (h *Handler) awaitImageAttachmentsForAgent(streamCtx *sseStreamContext, reqCtx *qaRequestContext) {
	if reqCtx.customAgent == nil || !reqCtx.customAgent.Config.ImageUploadEnabled {
		return
	}
	var imageIDs []string
	for _, meta := range reqCtx.attachmentMetas {
		if meta.ID != "" && docparser.IsImageFormat(meta.FileType) {
			imageIDs = append(imageIDs, meta.ID)
		}
	}
	ctx := streamCtx.asyncCtx
	tenantID := reqCtx.session.TenantID
	if len(imageIDs) == 0 || !h.hasPendingAttachments(ctx, tenantID, reqCtx.sessionID, imageIDs) {
		return
	}
	start := time.Now()
	toolCallID := uuid.New().String()
	streamCtx.eventBus.Emit(ctx, event.Event{
		Type:      event.EventAgentToolCall,
		SessionID: reqCtx.sessionID,
		Data:      event.AgentToolCallData{ToolCallID: toolCallID, ToolName: "attachment_parsing"},
	})
	waitTimeout := h.attachmentWaitTimeout(reqCtx)
	h.waitForAttachments(ctx, tenantID, reqCtx.sessionID, imageIDs, waitTimeout)
	ready, skipped := h.partitionReadyAttachments(ctx, tenantID, reqCtx.sessionID, imageIDs)
	output := fmt.Sprintf("已解析 %d 个附件", len(ready))
	if skipped > 0 {
		output += fmt.Sprintf("，%d 个未完成已跳过", skipped)
	}
	streamCtx.eventBus.Emit(ctx, event.Event{
		Type:      event.EventAgentToolResult,
		SessionID: reqCtx.sessionID,
		Data: event.AgentToolResultData{
			ToolCallID: toolCallID,
			ToolName:   "attachment_parsing",
			Output:     output,
			Success:    true,
			Duration:   time.Since(start).Milliseconds(),
			Data: map[string]interface{}{
				"display_type":  "attachment_parsing",
				"parsed_count":  len(ready),
				"skipped_count": skipped,
			},
		},
	})
}

// applyTemporaryAttachments 按 Agent 支持的文件类型过滤解析结果，把它们（及其图片）
// 加进本轮，并把选中的内容写回用户消息以便后续历史回放。
func (h *Handler) applyTemporaryAttachments(
	ctx context.Context, reqCtx *qaRequestContext, temporaryResult *types.TemporaryDocumentPromptResult,
) {
	attachments := temporaryResult.Attachments
	if reqCtx.customAgent != nil && len(reqCtx.customAgent.Config.SupportedFileTypes) > 0 {
		filtered := attachments[:0]
		for _, att := range attachments {
			ext := strings.TrimPrefix(strings.ToLower(att.FileType), ".")
			if containsFileType(reqCtx.customAgent.Config.SupportedFileTypes, ext) {
				filtered = append(filtered, att)
			}
		}
		attachments = filtered
	}
	reqCtx.attachments = append(reqCtx.attachments, attachments...)
	// Persist the freshly selected content back onto the stored user message.
	// The message was created with metadata-only attachment entries (content is
	// selected here, after the SSE stream is live), so without this write a
	// later Agent-mode turn rebuilds history from the Attachments column and
	// sees empty attachments (see buildUserHistoryMessage in agent_history.go).
	h.persistResolvedAttachmentContent(ctx, reqCtx, attachments)
	if reqCtx.customAgent != nil && reqCtx.customAgent.Config.ImageUploadEnabled {
		for _, imageURL := range temporaryResult.ImageURLs {
			reqCtx.images = append(reqCtx.images, ImageAttachment{URL: imageURL})
		}
	}
}

// persistResolvedAttachmentContent writes the parsed content of pre-uploaded
// attachments back onto the stored user message so multi-turn history can
// replay it. Entries are matched by their temporary-document ID; only those
// present on the message are enriched. Failures are logged but never bubble up
// — losing the write only degrades follow-up context, it must not fail the turn.
func (h *Handler) persistResolvedAttachmentContent(
	ctx context.Context, reqCtx *qaRequestContext, resolved types.MessageAttachments,
) {
	if reqCtx.userMessageID == "" || len(resolved) == 0 {
		return
	}
	// Detach from the request/stream lifetime and pin the session tenant so a
	// user-triggered stop (which cancels asyncCtx) does not drop the write.
	updateCtx := context.WithValue(
		context.WithoutCancel(ctx), types.TenantIDContextKey, reqCtx.session.TenantID,
	)
	msg, err := h.messageService.GetMessage(updateCtx, reqCtx.sessionID, reqCtx.userMessageID)
	if err != nil || msg == nil {
		logger.Warnf(updateCtx, "persist attachment content: load user message %s failed: %v",
			reqCtx.userMessageID, err)
		return
	}
	byID := make(map[string]types.MessageAttachment, len(resolved))
	for _, att := range resolved {
		if att.ID != "" {
			byID[att.ID] = att
		}
	}
	changed := false
	for i := range msg.Attachments {
		if msg.Attachments[i].ID == "" {
			continue
		}
		if enriched, ok := byID[msg.Attachments[i].ID]; ok {
			msg.Attachments[i] = enriched
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := h.messageService.UpdateMessage(updateCtx, msg); err != nil {
		logger.Warnf(updateCtx, "persist attachment content: update user message %s failed: %v",
			reqCtx.userMessageID, err)
	}
}

// normalizeTemporaryAttachmentIDs rejects oversized ID lists before any DB
// lookup, then returns a deduplicated, order-preserving list of non-empty IDs.
func normalizeTemporaryAttachmentIDs(ids []string) ([]string, error) {
	if len(ids) > types.MaxTemporaryAttachmentsPerMessage {
		return nil, fmt.Errorf("a message can use at most %d attachments", types.MaxTemporaryAttachmentsPerMessage)
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// hasPendingAttachments reports whether any of the given documents is still
// uploading or being parsed.
func (h *Handler) hasPendingAttachments(ctx context.Context, tenantID uint64, sessionID string, ids []string) bool {
	for _, id := range ids {
		doc, err := h.temporaryDocuments.Get(ctx, tenantID, sessionID, id)
		if err != nil || doc == nil {
			continue
		}
		if doc.Status == types.TemporaryDocumentStatusUploaded ||
			doc.Status == types.TemporaryDocumentStatusProcessing {
			return true
		}
	}
	return false
}

// waitForAttachments polls until no attachment is pending or the timeout / ctx
// cancellation fires.
func (h *Handler) waitForAttachments(ctx context.Context, tenantID uint64, sessionID string, ids []string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !h.hasPendingAttachments(ctx, tenantID, sessionID, ids) || time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// partitionReadyAttachments splits the ids into ready ones and a count of those
// skipped (missing, failed, or still parsing after the wait).
func (h *Handler) partitionReadyAttachments(ctx context.Context, tenantID uint64, sessionID string, ids []string) (ready []string, skipped int) {
	for _, id := range ids {
		doc, err := h.temporaryDocuments.Get(ctx, tenantID, sessionID, id)
		if err != nil || doc == nil || doc.Status != types.TemporaryDocumentStatusReady {
			skipped++
			continue
		}
		ready = append(ready, id)
	}
	return ready, skipped
}

// persistLastRequestState records the input-bar state the user just sent so
// that reopening this session restores agent/model/KB/web-search/MCP picks.
// Pure UI memo — failures are logged but never bubble up; the caller runs
// this in a goroutine and is safe to discard the returned context.
func (h *Handler) persistLastRequestState(parentCtx context.Context, reqCtx *qaRequestContext, mode qaMode) {
	// Detach from the HTTP request lifetime: this write must survive both
	// SSE disconnects and the parent gin context being released after the
	// handler returns.
	ctx := logger.CloneContext(context.WithoutCancel(parentCtx))

	agentEnabled := reqCtx.reqAgentEnabled
	// Mirror the resolution rule used in AgentQA: a resolved custom agent's
	// agent_mode wins over the request flag. For KnowledgeQA the request
	// itself carries agent_enabled=false, so this collapses correctly.
	if mode == qaModeAgent && reqCtx.customAgent != nil {
		agentEnabled = reqCtx.customAgent.IsAgentMode()
	}

	state := &types.SessionLastRequestState{
		AgentID:          reqCtx.reqAgentID,
		AgentEnabled:     agentEnabled,
		ModelID:          reqCtx.summaryModelID,
		KnowledgeBaseIDs: reqCtx.knowledgeBaseIDs,
		KnowledgeIDs:     reqCtx.knowledgeIDs,
		TagIDs:           reqCtx.tagIDs,
		MCPServiceIDs:    reqCtx.mcpServiceIDs,
		SkillNames:       reqCtx.skillNames,
		MentionedItems:   reqCtx.mentionedItems,
		WebSearchEnabled: reqCtx.webSearchEnabled,
	}

	if err := h.sessionService.UpdateSessionLastRequestState(ctx, reqCtx.sessionID, state); err != nil {
		logger.Warnf(ctx, "persist last_request_state failed for session %s: %v", reqCtx.sessionID, err)
	}
}

// appendQuickAnswerReasoning accumulates streamed reasoning_content from
// KnowledgeQA (fast answer) into a single AgentStep for history replay.
func appendQuickAnswerReasoning(msg *types.Message, content string) {
	if content == "" {
		return
	}
	if len(msg.AgentSteps) == 0 {
		msg.AgentSteps = types.AgentSteps{{
			Iteration: 0,
			Timestamp: time.Now(),
			ToolCalls: make([]types.ToolCall, 0),
		}}
	}
	msg.AgentSteps[0].ReasoningContent += content
}

// persistTurnUsage 在 assistant 消息已经落库之后补写一次本轮的用量与上下文归因
// （PRD FR-12）。
//
// 正常时序走不到这里：采集侧保证用量事件早于 answer(Done)，而 completeAssistantMessage
// 是在收到 Done 之后才跑的。只有异常时序（用户中途停止、通道提前关闭）会让消息先完成，
// 此时补一次定向更新，别让这一轮的归因整个丢掉。尽力而为：失败只记日志，绝不影响答案。
func (h *Handler) persistTurnUsage(ctx context.Context, assistantMessage *types.Message) {
	if assistantMessage == nil {
		return
	}
	// 与 completeAssistantMessage 一样脱离请求生命周期：用户停止会取消 asyncCtx，
	// 而这次补写恰恰最常发生在那条路径上。
	updateCtx := context.WithoutCancel(ctx)
	if err := h.messageService.UpdateMessage(updateCtx, assistantMessage); err != nil {
		logger.Warnf(updateCtx, "persist turn usage failed for message %s: %v",
			assistantMessage.ID, err)
	}
}

// completeAssistantMessage marks an assistant message as complete, updates it,
// and asynchronously indexes the Q&A pair into the chat history knowledge base.
func (h *Handler) completeAssistantMessage(ctx context.Context, assistantMessage *types.Message, userQuery string) {
	assistantMessage.UpdatedAt = time.Now()
	assistantMessage.IsCompleted = true
	_ = h.messageService.UpdateMessage(ctx, assistantMessage)

	// Asynchronously index the Q&A pair into the chat history knowledge base for vector search.
	// Use WithoutCancel so the goroutine survives after the HTTP request context is done.
	bgCtx := context.WithoutCancel(ctx)
	go h.messageService.IndexMessageToKB(bgCtx, userQuery, assistantMessage.Content, assistantMessage.ID, assistantMessage.SessionID)
	if userQuery != "" && h.suggestionService != nil {
		go func() {
			if _, err := h.suggestionService.EnsureFollowUps(
				bgCtx, assistantMessage.SessionID, assistantMessage.ID, false,
			); err != nil {
				logger.Warnf(bgCtx, "follow-up suggestion generation failed for message %s: %v", assistantMessage.ID, err)
			}
		}()
	}
}

// enqueueMemoryExtraction fires the async memory-extraction task for one
// completed QA turn (L3 fact extraction + L2 rolling session summary).
// Best-effort: gated on a non-empty query (stopped/aborted turns are not
// worth memorizing, mirroring the chat-history indexing rule), and any
// failure is logged inside the service, never surfaced to the turn.
func (h *Handler) enqueueMemoryExtraction(ctx context.Context, reqCtx *qaRequestContext, assistantMessage *types.Message) {
	if h.memoryService == nil || reqCtx == nil || assistantMessage == nil {
		return
	}
	if reqCtx.query == "" || reqCtx.userMessageID == "" {
		return
	}
	tenantID := reqCtx.session.TenantID
	if tenantID == 0 {
		if tid, ok := types.TenantIDFromContext(ctx); ok {
			tenantID = tid
		}
	}
	if tenantID == 0 {
		return
	}
	h.memoryService.EnqueueMemoryExtract(context.WithoutCancel(ctx), &types.MemoryExtractPayload{
		TenantID:           tenantID,
		UserID:             types.SessionOwnerIDFromContext(ctx),
		SessionID:          assistantMessage.SessionID,
		UserMessageID:      reqCtx.userMessageID,
		AssistantMessageID: assistantMessage.ID,
		SummaryModelID:     reqCtx.summaryModelID,
	})
}
