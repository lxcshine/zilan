package handler

import (
	"context"
	goerrors "errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// MemoryHandler exposes the user-facing long-term memory management surface:
// "AI 记住了我什么" listing, explicit edit/delete, and full erasure (GDPR).
// All operations are scoped to the caller's (tenant, user) pair by the
// MemoryService via the request context.
type MemoryHandler struct {
	memoryService interfaces.MemoryService
}

// NewMemoryHandler creates a new memory handler instance.
func NewMemoryHandler(memoryService interfaces.MemoryService) *MemoryHandler {
	return &MemoryHandler{memoryService: memoryService}
}

// ListMemoryFacts godoc
// @Summary      查看 AI 长期记忆
// @Description  分页列出当前用户的长期记忆事实（画像/事实/偏好/待办/反馈/风格/技巧），支持分类与关键词过滤
// @Tags         记忆
// @Accept       json
// @Produce      json
// @Param        category  query     string  false  "分类过滤: profile|fact|preference|todo|feedback|soul|skill"
// @Param        status    query     string  false  "状态过滤: active|done|archived|all (默认 active)"
// @Param        keyword   query     string  false  "内容关键词"
// @Param        page      query     int     false  "页码 (默认 1)"
// @Param        page_size query     int     false  "每页条数 (默认 20, 最大 100)"
// @Success      200       {object}  map[string]interface{}  "记忆列表"
// @Failure      401       {object}  errors.AppError         "未授权"
// @Security     Bearer
// @Router       /memory/facts [get]
func (h *MemoryHandler) ListMemoryFacts(c *gin.Context) {
	ctx := c.Request.Context()

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}

	facts, total, err := h.memoryService.ListFacts(ctx, &types.MemoryFactQuery{
		Category: c.Query("category"),
		Status:   c.Query("status"),
		Keyword:  c.Query("keyword"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to list memory facts").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"items":     facts,
			"total":     total,
			"page":      page,
			"page_size": pageSize,
		},
	})
}

// updateMemoryFactRequest is the body for PUT /memory/facts/:id. All fields
// optional; omitted fields keep their stored values.
type updateMemoryFactRequest struct {
	Content    string  `json:"content"`
	Object     string  `json:"object"`
	Status     string  `json:"status"` // active | done | archived
	Importance float64 `json:"importance"`
	DueAt      string  `json:"due_at"` // ISO date/datetime; empty clears nothing (edit keeps stored value)
}

// createMemoryFactRequest 是 POST /memory/facts 的请求体（来源 manual）。
type createMemoryFactRequest struct {
	Category   string  `json:"category" binding:"required"`
	Subject    string  `json:"subject"`
	Predicate  string  `json:"predicate"`
	Object     string  `json:"object"`
	Content    string  `json:"content" binding:"required"`
	Importance float64 `json:"importance"`
	Confidence float64 `json:"confidence"`
	DueAt      string  `json:"due_at"`
}

// batchMemoryFactsRequest 是批量操作的请求体。
type batchMemoryFactsRequest struct {
	// Action 取 confirm | reject | archive | delete。
	Action string   `json:"action" binding:"required"`
	IDs    []string `json:"ids"    binding:"required"`
}

// ackMemoryNoticesRequest 是治理提示确认的请求体；ids 为空表示全部已读。
type ackMemoryNoticesRequest struct {
	IDs []int64 `json:"ids"`
}

// UpdateMemoryFact godoc
// @Summary      编辑一条长期记忆
// @Description  修改记忆内容/对象/状态/重要度；内容变更会自动重新生成语义向量
// @Tags         记忆
// @Accept       json
// @Produce      json
// @Param        id       path      string                   true  "记忆 ID"
// @Param        request  body      updateMemoryFactRequest  true  "编辑内容"
// @Success      200      {object}  map[string]interface{}   "更新成功"
// @Failure      400      {object}  errors.AppError          "请求参数错误"
// @Failure      404      {object}  errors.AppError          "记忆不存在"
// @Security     Bearer
// @Router       /memory/facts/{id} [put]
func (h *MemoryHandler) UpdateMemoryFact(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if id == "" {
		c.Error(errors.NewBadRequestError("memory fact id is required"))
		return
	}

	var req updateMemoryFactRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewValidationError("Invalid update request").WithDetails(err.Error()))
		return
	}
	if req.Status != "" && req.Status != types.MemoryStatusActive &&
		req.Status != types.MemoryStatusDone && req.Status != types.MemoryStatusArchived {
		c.Error(errors.NewValidationError("status must be one of active|done|archived"))
		return
	}

	fact := &types.MemoryFact{
		ID:         id,
		Content:    req.Content,
		Object:     req.Object,
		Status:     req.Status,
		Importance: req.Importance,
	}
	if req.DueAt != "" {
		for _, layout := range []string{"2006-01-02", time.RFC3339} {
			if t, err := time.Parse(layout, req.DueAt); err == nil {
				fact.DueAt = &t
				break
			}
		}
		if fact.DueAt == nil {
			c.Error(errors.NewValidationError("due_at must be an ISO date (2006-01-02) or RFC3339 datetime"))
			return
		}
	}

	if err := h.memoryService.UpdateFact(ctx, fact); err != nil {
		if err.Error() == "memory fact not found" {
			c.Error(errors.NewNotFoundError("memory fact not found"))
			return
		}
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"fact_id": id})
		c.Error(errors.NewInternalServerError("Failed to update memory fact").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// DeleteMemoryFact godoc
// @Summary      删除一条长期记忆
// @Description  删除当前用户名下指定的一条记忆；默认软删除（召回立即失效），hard=true 时物理移除且不可恢复
// @Tags         记忆
// @Produce      json
// @Param        id    path      string                  true   "记忆 ID"
// @Param        hard  query     bool                    false  "true 表示物理移除（不可逆，需前端二次确认）"
// @Success      200   {object}  map[string]interface{}  "删除成功"
// @Failure      404   {object}  errors.AppError         "记忆不存在"
// @Security     Bearer
// @Router       /memory/facts/{id} [delete]
func (h *MemoryHandler) DeleteMemoryFact(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if id == "" {
		c.Error(errors.NewBadRequestError("memory fact id is required"))
		return
	}
	hard, _ := strconv.ParseBool(c.Query("hard"))

	if err := h.memoryService.DeleteFact(ctx, id, hard); err != nil {
		h.respondMemoryError(c, err, "Failed to delete memory fact", map[string]interface{}{"fact_id": id})
		return
	}

	if hard {
		// PRD §5.2：删除（物理移除）需留审计痕迹。
		auditMemory(c, types.AuditActionMemoryBatchDeleted, types.AuditOutcomeSuccess,
			types.SessionOwnerIDFromContext(ctx), map[string]any{"fact_ids": []string{id}, "hard": true})
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// DeleteAllMemories godoc
// @Summary      清空全部记忆（GDPR 遗忘权）
// @Description  物理删除当前用户的全部长期记忆事实与短期会话摘要，并在同一事务内生成清空截止点，拦截在途的后台写入（FR-01）
// @Tags         记忆
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "删除条数与围栏信息"
// @Security     Bearer
// @Router       /memory [delete]
func (h *MemoryHandler) DeleteAllMemories(c *gin.Context) {
	ctx := c.Request.Context()

	result, err := h.memoryService.DeleteAllForUser(ctx)
	if err != nil {
		h.respondMemoryError(c, err, "Failed to erase memories", nil)
		return
	}

	userID := types.SessionOwnerIDFromContext(ctx)
	logger.Infof(ctx, "[Memory] user erased all memories, deleted=%d fence=%s", result.Deleted(), result.Generation)
	// PRD §5.2：记忆清空必须落审计日志，含围栏拦截数。
	auditMemory(c, types.AuditActionMemoryCleared, types.AuditOutcomeSuccess, userID, memoryClearAuditDetails(result))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"deleted": result.Deleted(), "clear": result},
	})
}

// memoryClearAuditDetails 把一次清空的结果整理成审计明细。
func memoryClearAuditDetails(result *types.MemoryClearResult) map[string]any {
	if result == nil {
		return nil
	}
	return map[string]any{
		"deleted_facts":     result.DeletedFacts,
		"deleted_summaries": result.DeletedSummaries,
		"fence_generation":  result.Generation,
		"cutoff_at":         result.CutoffAt,
		"blocked_writes":    result.BlockedWrites,
	}
}

// CreateMemoryFact godoc
// @Summary      新建一条长期记忆
// @Description  记忆管理页手工创建，来源为 manual，按状态机直接进入 active（PRD §3.2）
// @Tags         记忆
// @Accept       json
// @Produce      json
// @Param        request  body      createMemoryFactRequest  true  "记忆内容"
// @Success      200      {object}  map[string]interface{}   "创建成功"
// @Failure      400      {object}  errors.AppError          "请求参数错误"
// @Failure      409      {object}  errors.AppError          "同一条记忆已存在"
// @Security     Bearer
// @Router       /memory/facts [post]
func (h *MemoryHandler) CreateMemoryFact(c *gin.Context) {
	ctx := c.Request.Context()

	var req createMemoryFactRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewValidationError("Invalid create request").WithDetails(err.Error()))
		return
	}
	fact := &types.MemoryFact{
		Category:   req.Category,
		Subject:    req.Subject,
		Predicate:  req.Predicate,
		Object:     req.Object,
		Content:    req.Content,
		Importance: req.Importance,
		Confidence: req.Confidence,
	}
	if req.DueAt != "" {
		for _, layout := range []string{"2006-01-02", time.RFC3339} {
			if t, err := time.Parse(layout, req.DueAt); err == nil {
				fact.DueAt = &t
				break
			}
		}
		if fact.DueAt == nil {
			c.Error(errors.NewValidationError("due_at must be an ISO date (2006-01-02) or RFC3339 datetime"))
			return
		}
	}

	if err := h.memoryService.CreateFact(ctx, fact); err != nil {
		h.respondMemoryError(c, err, "Failed to create memory fact", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": fact})
}

// ConfirmMemoryFact godoc
// @Summary      确认一条待确认记忆
// @Description  pending → active，并在同一事务内把与之矛盾的旧记忆置为 superseded（FR-02/FR-03）。仅记忆归属用户本人可操作
// @Tags         记忆
// @Produce      json
// @Param        id   path      string                  true  "记忆 ID"
// @Success      200   {object}  map[string]interface{}  "确认结果"
// @Failure      404   {object}  errors.AppError         "记忆不存在"
// @Failure      409   {object}  errors.AppError         "记忆状态已变化"
// @Security     Bearer
// @Router       /memory/facts/{id}/confirm [post]
func (h *MemoryHandler) ConfirmMemoryFact(c *gin.Context) {
	h.batchMemoryFacts(c, types.MemoryBatchActionConfirm, []string{c.Param("id")}, "")
}

// RejectMemoryFact godoc
// @Summary      拒绝一条待确认记忆
// @Description  pending → archived（可恢复）。仅记忆归属用户本人可操作
// @Tags         记忆
// @Produce      json
// @Param        id   path      string                  true  "记忆 ID"
// @Success      200   {object}  map[string]interface{}  "拒绝结果"
// @Failure      404   {object}  errors.AppError         "记忆不存在"
// @Failure      409   {object}  errors.AppError         "记忆状态已变化"
// @Security     Bearer
// @Router       /memory/facts/{id}/reject [post]
func (h *MemoryHandler) RejectMemoryFact(c *gin.Context) {
	h.batchMemoryFacts(c, types.MemoryBatchActionReject, []string{c.Param("id")}, "")
}

// RestoreMemoryFact godoc
// @Summary      恢复一条已归档或已被取代的记忆
// @Description  archived / superseded → active；恢复被取代的旧值时，当前 active 的冲突项转为 superseded（PRD §3.2 / FR-03 规则 5）
// @Tags         记忆
// @Produce      json
// @Param        id   path      string                  true  "记忆 ID"
// @Success      200   {object}  map[string]interface{}  "恢复成功"
// @Failure      404   {object}  errors.AppError         "记忆不存在"
// @Failure      409   {object}  errors.AppError         "记忆状态不允许恢复"
// @Security     Bearer
// @Router       /memory/facts/{id}/restore [post]
func (h *MemoryHandler) RestoreMemoryFact(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if id == "" {
		c.Error(errors.NewBadRequestError("memory fact id is required"))
		return
	}
	if err := h.memoryService.RestoreFact(ctx, id); err != nil {
		h.respondMemoryError(c, err, "Failed to restore memory fact", map[string]interface{}{"fact_id": id})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// GetMemoryFactChain godoc
// @Summary      查看一条记忆的取代链
// @Description  返回同一矛盾判定键下的当前值、历史值与变更时间（FR-03 规则 4）
// @Tags         记忆
// @Produce      json
// @Param        id   path      string                  true  "记忆 ID"
// @Success      200   {object}  map[string]interface{}  "取代链"
// @Failure      404   {object}  errors.AppError         "记忆不存在"
// @Security     Bearer
// @Router       /memory/facts/{id}/chain [get]
func (h *MemoryHandler) GetMemoryFactChain(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if id == "" {
		c.Error(errors.NewBadRequestError("memory fact id is required"))
		return
	}
	chain, err := h.memoryService.GetFactChain(ctx, id)
	if err != nil {
		h.respondMemoryError(c, err, "Failed to load memory chain", map[string]interface{}{"fact_id": id})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"items": chain}})
}

// BatchMemoryFacts godoc
// @Summary      批量处理记忆
// @Description  action 取 confirm（批量确认待确认项）| reject（批量拒绝）| archive（批量归档）| delete（批量物理删除）。批量归档/删除同时开放给工作区管理员
// @Tags         记忆
// @Accept       json
// @Produce      json
// @Param        request  body      batchMemoryFactsRequest  true  "批量操作"
// @Success      200      {object}  map[string]interface{}   "批量结果"
// @Failure      400      {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Router       /memory/facts/batch [post]
func (h *MemoryHandler) BatchMemoryFacts(c *gin.Context) {
	var req batchMemoryFactsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewValidationError("Invalid batch request").WithDetails(err.Error()))
		return
	}
	if len(req.IDs) == 0 {
		c.Error(errors.NewBadRequestError("ids is required"))
		return
	}
	h.batchMemoryFacts(c, req.Action, req.IDs, "")
}

// BatchMemberMemoryFacts godoc
// @Summary      管理员批量处理成员记忆
// @Description  工作区管理员对成员记忆批量归档或删除（PRD §5.1）；确认/拒绝仍仅限记忆归属用户本人
// @Tags         记忆
// @Accept       json
// @Produce      json
// @Param        user_id  path      string                   true  "成员用户 ID"
// @Param        request  body      batchMemoryFactsRequest  true  "批量操作"
// @Success      200      {object}  map[string]interface{}   "批量结果"
// @Failure      403      {object}  errors.AppError          "无权操作"
// @Security     Bearer
// @Router       /memory/admin/users/{user_id}/facts/batch [post]
func (h *MemoryHandler) BatchMemberMemoryFacts(c *gin.Context) {
	var req batchMemoryFactsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewValidationError("Invalid batch request").WithDetails(err.Error()))
		return
	}
	if len(req.IDs) == 0 {
		c.Error(errors.NewBadRequestError("ids is required"))
		return
	}
	h.batchMemoryFacts(c, req.Action, req.IDs, c.Param("user_id"))
}

// ClearMemberMemory godoc
// @Summary      管理员清空成员记忆
// @Description  工作区管理员对成员执行与本人清空等价的操作，同样在同一事务内生成清空截止点拦截在途写入（FR-01 / PRD §5.1）
// @Tags         记忆
// @Produce      json
// @Param        user_id  path      string                  true  "成员用户 ID"
// @Success      200      {object}  map[string]interface{}  "删除条数与围栏信息"
// @Failure      403      {object}  errors.AppError         "无权操作"
// @Security     Bearer
// @Router       /memory/admin/users/{user_id} [delete]
func (h *MemoryHandler) ClearMemberMemory(c *gin.Context) {
	ctx := c.Request.Context()
	targetUserID := c.Param("user_id")
	if targetUserID == "" {
		c.Error(errors.NewBadRequestError("user_id is required"))
		return
	}
	result, err := h.memoryService.ClearMemoryForTarget(ctx, targetUserID)
	if err != nil {
		h.respondMemoryError(c, err, "Failed to clear member memories", map[string]interface{}{"user_id": targetUserID})
		return
	}
	logger.Infof(ctx, "[Memory] admin cleared memories of user %s, deleted=%d", targetUserID, result.Deleted())
	auditMemory(c, types.AuditActionMemoryAdminCleared, types.AuditOutcomeSuccess, targetUserID,
		memoryClearAuditDetails(result))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"deleted": result.Deleted(), "clear": result},
	})
}

// batchMemoryFacts 是逐条与批量操作的共同实现。
func (h *MemoryHandler) batchMemoryFacts(c *gin.Context, action string, ids []string, targetUserID string) {
	ctx := c.Request.Context()
	switch action {
	case types.MemoryBatchActionConfirm, types.MemoryBatchActionReject,
		types.MemoryBatchActionArchive, types.MemoryBatchActionDelete:
	default:
		c.Error(errors.NewBadRequestError("action must be one of confirm|reject|archive|delete"))
		return
	}

	result, err := h.memoryService.BatchUpdateFacts(ctx, targetUserID, action, ids)
	if err != nil {
		h.respondMemoryError(c, err, "Failed to batch update memory facts", nil)
		return
	}
	// PRD §5.2：批量归档 / 批量删除必须落审计日志。
	switch action {
	case types.MemoryBatchActionArchive:
		auditMemory(c, types.AuditActionMemoryBatchArchived, types.AuditOutcomeSuccess,
			memoryAuditTarget(ctx, targetUserID), map[string]any{"fact_ids": ids, "affected": result.Affected})
	case types.MemoryBatchActionDelete:
		auditMemory(c, types.AuditActionMemoryBatchDeleted, types.AuditOutcomeSuccess,
			memoryAuditTarget(ctx, targetUserID), map[string]any{"fact_ids": ids, "affected": result.Affected})
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// memoryAuditTarget 返回审计条目的目标用户：管理员代操作时是被操作成员，
// 否则是调用者本人。
func memoryAuditTarget(ctx context.Context, targetUserID string) string {
	if targetUserID != "" {
		return targetUserID
	}
	return types.SessionOwnerIDFromContext(ctx)
}

// respondMemoryError 把服务层错误映射为 HTTP 错误。
func (h *MemoryHandler) respondMemoryError(
	c *gin.Context, err error, message string, fields map[string]interface{},
) {
	switch {
	case err == nil:
		return
	case goerrors.Is(err, types.ErrMemoryForbidden):
		c.Error(errors.NewForbiddenError("no permission to operate on this memory"))
	case goerrors.Is(err, types.ErrMemoryConflict):
		c.Error(errors.NewConflictError("memory state conflict").WithDetails(err.Error()))
	case err.Error() == "memory fact not found":
		c.Error(errors.NewNotFoundError("memory fact not found"))
	default:
		logger.ErrorWithFields(c.Request.Context(), err, fields)
		c.Error(errors.NewInternalServerError(message).WithDetails(err.Error()))
	}
}

// auditMemory 落一条记忆治理审计日志（PRD §5.2）。审计服务由 AuditServiceProvider
// 中间件注入 gin 上下文；未注入或写入失败都不影响业务响应。
func auditMemory(
	c *gin.Context, action types.AuditAction, outcome types.AuditOutcome,
	targetUserID string, details map[string]any,
) {
	audit := middleware.AuditServiceFromContext(c)
	if audit == nil {
		return
	}
	ctx := c.Request.Context()
	tenantID, _ := types.TenantIDFromContext(ctx)
	actorID, _ := types.UserIDFromContext(ctx)
	entry := &types.AuditLog{
		TenantID:      tenantID,
		ActorUserID:   actorID,
		ActorRole:     string(types.TenantRoleFromContext(ctx)),
		Action:        action,
		ScopeType:     "memory",
		ScopeID:       targetUserID,
		TargetType:    "memory_fact",
		TargetID:      targetUserID,
		TargetUserID:  targetUserID,
		RequestPath:   c.Request.URL.Path,
		RequestMethod: c.Request.Method,
		Outcome:       outcome,
		Details:       types.NewJSON(details),
	}
	if err := audit.Log(ctx, entry); err != nil {
		logger.Warnf(ctx, "[Memory] audit log write failed for %s: %v", action, err)
	}
}

// ListMemoryNotices godoc
// @Summary      待提示的记忆治理事件
// @Description  返回尚未提示过的自动归档等治理事件，供记忆管理页做一次性提示（FR-04 规则 5）
// @Tags         记忆
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "事件列表"
// @Security     Bearer
// @Router       /memory/notices [get]
func (h *MemoryHandler) ListMemoryNotices(c *gin.Context) {
	notices, err := h.memoryService.ListGovernanceNotices(c.Request.Context())
	if err != nil {
		h.respondMemoryError(c, err, "Failed to list memory notices", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"items": notices}})
}

// AckMemoryNotices godoc
// @Summary      确认记忆治理提示
// @Description  标记治理事件已提示，保证同一次自动归档只提示一次；ids 为空表示全部确认
// @Tags         记忆
// @Accept       json
// @Produce      json
// @Param        request  body      ackMemoryNoticesRequest  false  "待确认的事件 ID"
// @Success      200      {object}  map[string]interface{}   "确认条数"
// @Security     Bearer
// @Router       /memory/notices/ack [post]
func (h *MemoryHandler) AckMemoryNotices(c *gin.Context) {
	ctx := c.Request.Context()
	var req ackMemoryNoticesRequest
	// 空 body 表示"全部已读"。
	_ = c.ShouldBindJSON(&req)

	ids := req.IDs
	if len(ids) == 0 {
		notices, err := h.memoryService.ListGovernanceNotices(ctx)
		if err != nil {
			h.respondMemoryError(c, err, "Failed to list memory notices", nil)
			return
		}
		for _, notice := range notices {
			ids = append(ids, notice.ID)
		}
	}
	acked, err := h.memoryService.AcknowledgeGovernanceNotices(ctx, ids)
	if err != nil {
		h.respondMemoryError(c, err, "Failed to acknowledge memory notices", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"acknowledged": acked}})
}

// GetMemoryStatus godoc
// @Summary      记忆功能状态
// @Description  返回当前用户的记忆开关、写入模式、各状态条数（含待确认角标）、生效容量上限与围栏拦截数
// @Tags         记忆
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "状态"
// @Security     Bearer
// @Router       /memory/status [get]
func (h *MemoryHandler) GetMemoryStatus(c *gin.Context) {
	ctx := c.Request.Context()

	info, err := h.memoryService.GetStatusInfo(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to get memory status").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    info,
	})
}

// GetMemoryModules godoc
// @Summary      记忆四模块总览
// @Description  返回 Soul/User/Memory/Agent 四个模块各自的记忆条数（Memory 模块额外含 L2 会话摘要数），供记忆页导航徽标
// @Tags         记忆
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "模块计数列表"
// @Failure      401  {object}  errors.AppError         "未授权"
// @Security     Bearer
// @Router       /memory/modules [get]
func (h *MemoryHandler) GetMemoryModules(c *gin.Context) {
	ctx := c.Request.Context()

	modules, err := h.memoryService.GetModuleOverview(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to get memory modules").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"modules": modules},
	})
}

// GetSoulCard godoc
// @Summary      Soul 灵魂卡
// @Description  返回全局人设（只读）与用户的风格微调指令（category=soul），供 Soul 模块视图渲染
// @Tags         记忆
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "灵魂卡"
// @Failure      401  {object}  errors.AppError         "未授权"
// @Security     Bearer
// @Router       /memory/soul [get]
func (h *MemoryHandler) GetSoulCard(c *gin.Context) {
	ctx := c.Request.Context()

	card, err := h.memoryService.GetSoulCard(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to get soul card").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    card,
	})
}

// GetProfileCard godoc
// @Summary      User 用户档案卡
// @Description  将画像/事实/偏好记忆聚合为身份/职责/偏好/事实四个分组，返回分组条目与档案完整度
// @Tags         记忆
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "档案卡"
// @Failure      401  {object}  errors.AppError         "未授权"
// @Security     Bearer
// @Router       /memory/profile [get]
func (h *MemoryHandler) GetProfileCard(c *gin.Context) {
	ctx := c.Request.Context()

	card, err := h.memoryService.GetProfileCard(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to get profile card").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    card,
	})
}

// GetAgentTips godoc
// @Summary      Agent 经验技巧卡
// @Description  返回助手沉淀的技巧（category=skill）与原始反馈墙（category=feedback，含升级关联 skill ID），反馈支持分页
// @Tags         记忆
// @Produce      json
// @Param        page      query     int     false  "反馈页码 (默认 1)"
// @Param        page_size query     int     false  "反馈每页条数 (默认 20, 最大 100)"
// @Success      200  {object}  map[string]interface{}  "技巧卡"
// @Failure      401  {object}  errors.AppError         "未授权"
// @Security     Bearer
// @Router       /memory/agent-tips [get]
func (h *MemoryHandler) GetAgentTips(c *gin.Context) {
	ctx := c.Request.Context()

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	card, err := h.memoryService.GetAgentTipsCard(ctx, page, pageSize)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to get agent tips").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    card,
	})
}
