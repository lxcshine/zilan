package handler

import (
	"encoding/json"
	"io"
	"net/http"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// GraphSearchHandler 暴露单个已授权知识库图谱的只读 JSON 视图（FR-19③）。
// 它复用 KnowledgeBaseHandler 的知识库解析与鉴权逻辑，不重复实现一套。
type GraphSearchHandler struct {
	kbHandler *KnowledgeBaseHandler
	service   interfaces.GraphSearchService
}

// NewGraphSearchHandler 使用标准知识库访问解析器构造 handler。
func NewGraphSearchHandler(kbHandler *KnowledgeBaseHandler, service interfaces.GraphSearchService) *GraphSearchHandler {
	return &GraphSearchHandler{kbHandler: kbHandler, service: service}
}

// Search godoc
// @Summary      查询知识图谱
// @Description  对单个授权知识库按实体名进行区分大小写的子串查询，返回实体、定向关系及仍然有效的来源分块。不调用模型，不影响默认检索。上限传 0 表示使用默认值。
// @Tags         知识库
// @Accept       json
// @Produce      json
// @Param        id       path string                   true "知识库ID"
// @Param        request  body types.GraphSearchRequest true "查询及结果上限"
// @Success      200 {object} map[string]interface{} "{ success: true, data: types.GraphSearchResult }"
// @Failure      400 {object} apperrors.AppError "参数错误"
// @Failure      401 {object} apperrors.AppError "未登录"
// @Failure      403 {object} apperrors.AppError "无权访问知识库"
// @Failure      404 {object} apperrors.AppError "知识库不存在"
// @Failure      409 {object} apperrors.AppError "知识图谱未启用"
// @Failure      500 {object} apperrors.AppError "来源校验失败"
// @Failure      503 {object} apperrors.AppError "图谱服务不可用"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-bases/{id}/graph-search [post]
func (h *GraphSearchHandler) Search(c *gin.Context) {
	// validateAndGetKnowledgeBase 负责租户隔离、组织共享与 RBAC 校验，
	// 图谱读取必须发生在它成功之后。
	kb, _, _, _, err := h.kbHandler.validateAndGetKnowledgeBase(c)
	if err != nil {
		_ = c.Error(err)
		return
	}
	// 限制请求体大小并拒绝未知字段/尾部 JSON：宁可 400，也不要静默地只执行
	// 第一个查询对象。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var req types.GraphSearchRequest
	if err := decoder.Decode(&req); err != nil {
		_ = c.Error(apperrors.NewBadRequestError("Invalid graph query request"))
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		_ = c.Error(apperrors.NewBadRequestError("Expected one graph query object"))
		return
	}
	if err := req.Normalize(); err != nil {
		_ = c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	result, err := h.service.Search(c.Request.Context(), kb, req)
	if err != nil {
		_ = c.Error(err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}
