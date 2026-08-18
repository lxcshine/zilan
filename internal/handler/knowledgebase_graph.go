package handler

import (
	"net/http"
	"strings"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/gin-gonic/gin"
)

// ListGraphCommunities godoc
// @Summary      获取知识库的图谱社区摘要
// @Description  返回该知识库 GraphRAG 社区检测+LLM 摘要的当前结果（可能为空）
// @Tags         知识库
// @Produce      json
// @Param        id  path      string  true  "知识库ID"
// @Success      200  {object}  map[string]interface{}  "社区摘要列表"
// @Router       /knowledge-bases/{id}/graph/communities [get]
func (h *KnowledgeBaseHandler) ListGraphCommunities(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if id == "" {
		c.Error(errors.NewBadRequestError("knowledge base ID is required"))
		return
	}
	if h.graphCommunitySvc == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": []interface{}{}})
		return
	}
	communities, err := h.graphCommunitySvc.ListCommunities(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": communities})
}

// RebuildGraphCommunities godoc
// @Summary      重建知识库的图谱社区摘要
// @Description  触发一次 GraphRAG 社区检测+摘要重建（异步任务，防抖合并）
// @Tags         知识库
// @Produce      json
// @Param        id  path      string  true  "知识库ID"
// @Success      200  {object}  map[string]interface{}  "任务已入队"
// @Router       /knowledge-bases/{id}/graph/communities/rebuild [post]
func (h *KnowledgeBaseHandler) RebuildGraphCommunities(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if id == "" {
		c.Error(errors.NewBadRequestError("knowledge base ID is required"))
		return
	}
	if h.graphCommunitySvc == nil {
		c.Error(errors.NewBadRequestError("graph RAG is not available"))
		return
	}
	if err := h.graphCommunitySvc.EnqueueRebuild(ctx, id); err != nil {
		if strings.Contains(err.Error(), "not enabled") || strings.Contains(err.Error(), "not found") {
			c.Error(errors.NewBadRequestError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "graph community rebuild enqueued"})
}
