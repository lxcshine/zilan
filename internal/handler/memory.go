package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
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
// @Description  分页列出当前用户的长期记忆事实（画像/事实/偏好/待办/反馈），支持分类与关键词过滤
// @Tags         记忆
// @Accept       json
// @Produce      json
// @Param        category  query     string  false  "分类过滤: profile|fact|preference|todo|feedback"
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
// @Description  删除当前用户名下指定的一条记忆（软删除，召回立即失效）
// @Tags         记忆
// @Produce      json
// @Param        id   path      string                  true  "记忆 ID"
// @Success      200  {object}  map[string]interface{}  "删除成功"
// @Failure      404  {object}  errors.AppError         "记忆不存在"
// @Security     Bearer
// @Router       /memory/facts/{id} [delete]
func (h *MemoryHandler) DeleteMemoryFact(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if id == "" {
		c.Error(errors.NewBadRequestError("memory fact id is required"))
		return
	}

	if err := h.memoryService.DeleteFact(ctx, id); err != nil {
		if err.Error() == "memory fact not found" {
			c.Error(errors.NewNotFoundError("memory fact not found"))
			return
		}
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"fact_id": id})
		c.Error(errors.NewInternalServerError("Failed to delete memory fact").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// DeleteAllMemories godoc
// @Summary      清空全部记忆（GDPR 遗忘权）
// @Description  删除当前用户的全部长期记忆事实与短期会话摘要，不可恢复
// @Tags         记忆
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "删除条数"
// @Security     Bearer
// @Router       /memory [delete]
func (h *MemoryHandler) DeleteAllMemories(c *gin.Context) {
	ctx := c.Request.Context()

	deleted, err := h.memoryService.DeleteAllForUser(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to erase memories").WithDetails(err.Error()))
		return
	}

	logger.Infof(ctx, "[Memory] user erased all memories, deleted=%d", deleted)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"deleted": deleted},
	})
}

// GetMemoryStatus godoc
// @Summary      记忆功能状态
// @Description  返回当前用户的记忆开关状态与记忆条数，供设置页展示
// @Tags         记忆
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "状态"
// @Security     Bearer
// @Router       /memory/status [get]
func (h *MemoryHandler) GetMemoryStatus(c *gin.Context) {
	ctx := c.Request.Context()

	userID := types.SessionOwnerIDFromContext(ctx)
	enabled := h.memoryService.IsEnabled(ctx, userID)
	_, total, err := h.memoryService.ListFacts(ctx, &types.MemoryFactQuery{Page: 1, PageSize: 1})
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError("Failed to get memory status").WithDetails(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"enabled":    enabled,
			"fact_count": total,
		},
	})
}
