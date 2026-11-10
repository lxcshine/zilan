package handler

import (
	goerrors "errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
)

// ---------------------------------------------------------------------------
// FR-09 记忆管理控制台（管理员侧）
//
// 路由挂在 /memory/admin 组下并统一套 g.Admin()（见 router/routes_auth_tenant.go）；
// 服务层再校验一次管理员身份并拒绝 API Key 主体，双重保险。
//
// 本文件的所有响应只有聚合计数与操作结果，不含任何记忆正文（PRD §5.1）。
// 跨工作区聚合入口没有实现，因此也不存在需要叠加 RequireCrossTenantAccess 的路径。
// ---------------------------------------------------------------------------

// memoryAdminPaging 解析 limit / offset：非法值回落默认，越界值由
// types.MemoryAdminPage 收敛（默认 50，硬顶 200）。
func memoryAdminPaging(c *gin.Context) (int, int) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	return types.MemoryAdminPage(limit, offset)
}

// normalizeMemoryAdminIDs 去掉空白 ID。请求里全是空白串等同于没给 ID，
// 应当在 handler 层就拦成 400，而不是把一个必然无效的批量请求放进服务层。
func normalizeMemoryAdminIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// AdminMemoryOverview godoc
// @Summary      工作区记忆总览
// @Description  返回本工作区的记忆总量、按状态（active/pending/superseded/archived）与按类型（category）的分布，以及成员数与生效的容量上限（FR-09 规则 1）。仅工作区管理员可访问，响应不含记忆正文
// @Tags         记忆管理控制台
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "聚合总览"
// @Failure      403  {object}  errors.AppError         "需要工作区管理员"
// @Security     Bearer
// @Router       /memory/admin/overview [get]
func (h *MemoryHandler) AdminMemoryOverview(c *gin.Context) {
	ctx := c.Request.Context()

	overview, err := h.memoryService.AdminMemoryOverview(ctx)
	if err != nil {
		h.failMemoryAdmin(c, err, "Failed to load memory admin overview")
		return
	}
	// PRD §5.2：管理员侧记忆操作一律留痕，读操作也不例外——控制台暴露的是
	// 全工作区的记忆规模。
	h.auditMemoryAdmin(c, types.AuditActionMemoryAdminOverviewViewed, "", "", map[string]any{
		"total":      overview.Total,
		"user_count": overview.UserCount,
	})
	c.JSON(http.StatusOK, gin.H{"success": true, "data": overview})
}

// AdminListMemoryUsers godoc
// @Summary      按用户维度查看记忆规模
// @Description  分页返回本工作区各成员的记忆规模（总量与四状态计数、最近活动时间），按规模倒序，用于识别异常增长（FR-09 规则 2）
// @Tags         记忆管理控制台
// @Produce      json
// @Param        limit   query     int     false  "每页条数，默认 50，最大 200"
// @Param        offset  query     int     false  "偏移量，默认 0"
// @Success      200     {object}  map[string]interface{}  "成员规模列表与总数"
// @Failure      403     {object}  errors.AppError         "需要工作区管理员"
// @Security     Bearer
// @Router       /memory/admin/users [get]
func (h *MemoryHandler) AdminListMemoryUsers(c *gin.Context) {
	ctx := c.Request.Context()
	limit, offset := memoryAdminPaging(c)

	summaries, total, err := h.memoryService.AdminListMemoryUsers(ctx, limit, offset)
	if err != nil {
		h.failMemoryAdmin(c, err, "Failed to list memory users")
		return
	}
	h.auditMemoryAdmin(c, types.AuditActionMemoryAdminUsersListed, "", "", map[string]any{
		"limit": limit, "offset": offset, "count": len(summaries), "total": total,
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"items": summaries, "total": total, "limit": limit, "offset": offset,
		},
	})
}

// AdminGetMemoryUser godoc
// @Summary      查看单个成员的记忆规模明细
// @Description  返回指定成员的状态计数、按类型分布与治理状态（自动抽取是否暂停、围栏拦截数）。目标不属于本工作区时与"不存在"同返 404，响应不含记忆正文
// @Tags         记忆管理控制台
// @Produce      json
// @Param        user_id  path      string                  true  "成员用户 ID"
// @Success      200      {object}  map[string]interface{}  "成员明细"
// @Failure      403      {object}  errors.AppError         "需要工作区管理员"
// @Failure      404      {object}  errors.AppError         "成员不存在或不属于本工作区"
// @Security     Bearer
// @Router       /memory/admin/users/{user_id} [get]
func (h *MemoryHandler) AdminGetMemoryUser(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.Param("user_id")
	if userID == "" {
		c.Error(errors.NewBadRequestError("user_id is required"))
		return
	}

	detail, err := h.memoryService.AdminGetMemoryUser(ctx, userID)
	if err != nil {
		h.failMemoryAdmin(c, err, "Failed to load memory user detail")
		return
	}
	h.auditMemoryAdmin(c, types.AuditActionMemoryAdminUserInspected, userID, userID, map[string]any{
		"item_count":    detail.ItemCount,
		"active_count":  detail.ActiveCount,
		"pending_count": detail.PendingCount,
	})
	c.JSON(http.StatusOK, gin.H{"success": true, "data": detail})
}

// AdminBatchArchiveMemoryFacts godoc
// @Summary      批量归档成员记忆
// @Description  工作区管理员把指定成员的一批记忆转为 archived（可逆，成员仍可在「已归档」分组恢复）。影响条目数落审计日志（FR-09 规则 3/5）
// @Tags         记忆管理控制台
// @Accept       json
// @Produce      json
// @Param        request  body      types.MemoryAdminBatchRequest  true  "目标成员与记忆 ID 列表"
// @Success      200      {object}  map[string]interface{}         "归档结果"
// @Failure      400      {object}  errors.AppError                "请求参数错误"
// @Failure      403      {object}  errors.AppError                "需要工作区管理员"
// @Security     Bearer
// @Router       /memory/admin/facts/batch-archive [post]
func (h *MemoryHandler) AdminBatchArchiveMemoryFacts(c *gin.Context) {
	req, ok := h.bindMemoryAdminBatch(c)
	if !ok {
		return
	}
	ids := normalizeMemoryAdminIDs(req.IDs)
	if len(ids) == 0 {
		c.Error(errors.NewBadRequestError("ids is required"))
		return
	}
	if len(ids) > types.MemoryAdminBatchLimit {
		c.Error(errors.NewBadRequestError(
			fmt.Sprintf("at most %d ids per request", types.MemoryAdminBatchLimit)))
		return
	}

	ctx := c.Request.Context()
	result, err := h.memoryService.AdminBatchArchiveFacts(ctx, req.UserID, ids)
	if err != nil {
		h.failMemoryAdmin(c, err, "Failed to batch archive memory facts")
		return
	}
	// PRD §5.2：操作者、操作类型、影响条目数、目标用户。
	h.auditMemoryAdmin(c, types.AuditActionMemoryAdminBatchArchived, req.UserID, req.UserID, map[string]any{
		"requested": result.Requested, "affected": result.Affected, "fact_ids": ids,
	})
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// AdminBatchDeletePendingMemoryFacts godoc
// @Summary      批量删除待确认项
// @Description  工作区管理员物理删除指定成员的待确认（pending）记忆，不可逆。只作用于 pending：请求里混入的 active/archived/superseded 条目会被拒绝并在响应的 skipped 中回报（FR-09 规则 3、FR-02 规则 6）
// @Tags         记忆管理控制台
// @Accept       json
// @Produce      json
// @Param        request  body      types.MemoryAdminBatchRequest  true  "目标成员与记忆 ID 列表；ids 为空表示该成员全部待确认项"
// @Success      200      {object}  map[string]interface{}         "删除结果"
// @Failure      400      {object}  errors.AppError                "请求参数错误"
// @Failure      403      {object}  errors.AppError                "需要工作区管理员"
// @Security     Bearer
// @Router       /memory/admin/facts/batch-delete-pending [post]
func (h *MemoryHandler) AdminBatchDeletePendingMemoryFacts(c *gin.Context) {
	req, ok := h.bindMemoryAdminBatch(c)
	if !ok {
		return
	}
	// ids 为空是合法请求：语义是"该成员的全部待确认项"，由服务层解析。
	ids := normalizeMemoryAdminIDs(req.IDs)
	if len(ids) > types.MemoryAdminBatchLimit {
		c.Error(errors.NewBadRequestError(
			fmt.Sprintf("at most %d ids per request", types.MemoryAdminBatchLimit)))
		return
	}

	ctx := c.Request.Context()
	result, err := h.memoryService.AdminBatchDeletePendingFacts(ctx, req.UserID, ids)
	if err != nil {
		h.failMemoryAdmin(c, err, "Failed to batch delete pending memory facts")
		return
	}
	h.auditMemoryAdmin(c, types.AuditActionMemoryAdminBatchDeleted, req.UserID, req.UserID, map[string]any{
		"requested": result.Requested, "affected": result.Affected,
		"skipped": result.Skipped, "fact_ids": ids,
	})
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// bindMemoryAdminBatch 解析并校验批量请求体；返回 false 表示错误已挂到 gin 上下文。
func (h *MemoryHandler) bindMemoryAdminBatch(c *gin.Context) (types.MemoryAdminBatchRequest, bool) {
	var req types.MemoryAdminBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(errors.NewValidationError("Invalid memory admin batch request").WithDetails(err.Error()))
		return req, false
	}
	if req.UserID == "" {
		c.Error(errors.NewBadRequestError("user_id is required"))
		return req, false
	}
	return req, true
}

// failMemoryAdmin 把服务层错误映射为 HTTP 错误。
//
// "目标成员不存在"与"目标属于其他工作区"共用 types.ErrMemoryAdminTargetNotFound，
// 因此状态码与文案完全一致——调用方无法用 404 的差异探测其他工作区是否存在某个
// 用户（与上游 failMemoryAdmin 的防探测口径一致，FR-09 规则 4）。
func (h *MemoryHandler) failMemoryAdmin(c *gin.Context, err error, message string) {
	switch {
	case err == nil:
		return
	case goerrors.Is(err, types.ErrMemoryForbidden):
		c.Error(errors.NewForbiddenError("memory administration requires a workspace administrator"))
	case goerrors.Is(err, types.ErrMemoryAdminBadRequest):
		c.Error(errors.NewBadRequestError(err.Error()))
	case goerrors.Is(err, types.ErrMemoryAdminTargetNotFound):
		c.Error(errors.NewNotFoundError("memory not found"))
	case goerrors.Is(err, types.ErrMemoryConflict):
		c.Error(errors.NewConflictError("memory state conflict").WithDetails(err.Error()))
	default:
		logger.ErrorWithFields(c.Request.Context(), err, nil)
		c.Error(errors.NewInternalServerError(message).WithDetails(err.Error()))
	}
}

// auditMemoryAdmin 落一条控制台审计日志（FR-09 规则 5 / PRD §5.2）。
//
// 与既有 auditMemory 的区别：这里**不设置 ScopeType / ScopeID**。租户审计 feed
// （GET /tenants/:id/audit-log）用 UnscopedOnly 过滤，带 scope 的行不会出现在
// 管理员的审计视图里；资源身份改写在 TargetType / TargetID / TargetUserID 上。
// 审计明细只含计数与 ID，绝不含记忆正文。
//
// 审计服务未注入或写入失败都不改变业务结果——留痕属于增强能力，
// 不能因为审计侧故障就让管理员的治理操作失败（上游同样只 Warn）。
func (h *MemoryHandler) auditMemoryAdmin(
	c *gin.Context, action types.AuditAction, targetID, targetUserID string, details map[string]any,
) {
	audit := middleware.AuditServiceFromContext(c)
	if audit == nil {
		return
	}
	ctx := c.Request.Context()
	tenantID, _ := types.TenantIDFromContext(ctx)
	actorID, _ := types.UserIDFromContext(ctx)

	targetType := "memory_workspace"
	if targetID == "" {
		targetID = strconv.FormatUint(tenantID, 10)
	} else {
		targetType = "memory_user"
	}
	entry := &types.AuditLog{
		TenantID:      tenantID,
		ActorUserID:   actorID,
		ActorRole:     string(types.TenantRoleFromContext(ctx)),
		Action:        action,
		TargetType:    targetType,
		TargetID:      targetID,
		TargetUserID:  targetUserID,
		RequestPath:   c.FullPath(),
		RequestMethod: c.Request.Method,
		Outcome:       types.AuditOutcomeSuccess,
		Details:       types.NewJSON(details),
	}
	if err := audit.Log(ctx, entry); err != nil {
		logger.Warnf(ctx, "[Memory] admin audit log write failed for %s: %v", action, err)
	}
}
