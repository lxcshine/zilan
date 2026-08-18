package session

import (
	stderrors "errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// CreateWikiFromSession godoc
// @Summary      将本次对话整理为 Wiki
// @Description  一键把会话内容蒸馏为指定 Wiki 知识库中的页面（幂等：重复调用刷新同一会话页面），页面通过 source_refs 回链到触发会话
// @Tags         会话
// @Accept       json
// @Produce      json
// @Param        session_id  path      string                    true  "会话ID"
// @Param        request     body      types.SessionWikiRequest  true  "目标 Wiki 知识库"
// @Success      200         {object}  map[string]interface{}    "创建或更新的 Wiki 页面"
// @Failure      400         {object}  errors.AppError           "请求参数错误"
// @Failure      404         {object}  errors.AppError           "会话不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/wiki [post]
func (h *Handler) CreateWikiFromSession(c *gin.Context) {
	ctx := c.Request.Context()

	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	if sessionID == "" {
		c.Error(errors.NewBadRequestError(errors.ErrInvalidSessionID.Error()))
		return
	}

	var req types.SessionWikiRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.Error(ctx, "Failed to parse session wiki request", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	if h.precipitationService == nil {
		c.Error(errors.NewInternalServerError("session precipitation service unavailable"))
		return
	}

	page, err := h.precipitationService.CreateWikiFromSession(ctx, sessionID, &req)
	if err != nil {
		if stderrors.Is(err, errors.ErrSessionNotFound) {
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"session_id": sessionID})
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    page,
	})
}
