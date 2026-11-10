package handler

import (
	"context"
	goerrors "errors"
	"io"
	"net/http"
	"strings"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

// FR-18 文档图片画廊：HTTP handler。
// 从上游 WeKnora-main internal/handler/knowledge.go 的图片部分拆出为独立
// handler（Zilan 的 KnowledgeHandler 未持有 chunkService，且并行开发下应避免
// 改动既有文件）。KB 访问校验直接复用 KnowledgeHandler.validateKnowledgeBaseAccess。
//
// 端点（均要求 tenant 范围 + KB 读权限，见 routes_gallery.go 的
// g.Viewer + g.KBAccessRead("id") 中间件）：
//   GET  /api/v1/knowledge-bases/:id/images   查询串过滤
//   POST /api/v1/knowledge-bases/:id/images   JSON 过滤（多文档来源组合时用，
//                                             避免 GET 查询串超长）
// 返回：{success, data: ImageAsset[], total, page, page_size}。

// FR-18 画廊多来源文档组合过滤的两档上限（与上游一致）：
// GET 的逗号分隔查询串截断到 50（URL 长度安全），POST 的 JSON 数组最多
// 1000、超出直接 400 —— 防止构造无上界的 IN 子句。
const (
	maxKnowledgeIDFilterGET  = 50
	maxKnowledgeIDFilterBody = 1000
)

// ImageGalleryHandler serves the knowledge base image gallery.
type ImageGalleryHandler struct {
	knowledgeHandler *KnowledgeHandler
	chunkService     interfaces.ChunkService
	kgService        interfaces.KnowledgeService
}

// NewImageGalleryHandler builds the gallery handler over the existing
// KnowledgeHandler（复用其访问校验）与 chunk/knowledge 服务。
func NewImageGalleryHandler(
	knowledgeHandler *KnowledgeHandler,
	chunkService interfaces.ChunkService,
	kgService interfaces.KnowledgeService,
) *ImageGalleryHandler {
	return &ImageGalleryHandler{
		knowledgeHandler: knowledgeHandler,
		chunkService:     chunkService,
		kgService:        kgService,
	}
}

// imageGalleryRequest is the on-the-wire filter form: GET binds it from the
// query string (form tags), POST from the JSON body. The namespaced attribute
// ids ("<sourceID>:<name>")、attr 过滤与逐值规则与上游画廊契约一致；Zilan
// 无属性观测来源时传未知 id 会被安全忽略而非报错。
type imageGalleryRequest struct {
	Keyword      string                       `form:"keyword" json:"keyword"`
	SearchIn     []string                     `form:"search_in" json:"search_in"`
	SortBy       string                       `form:"sort_by" json:"sort_by"`
	SortOrder    string                       `form:"sort_order" json:"sort_order"`
	AttrFilters  map[string][]string          `form:"attr_filters" json:"attr_filters"`
	AttrRules    map[string]map[string]string `form:"attr_rules" json:"attr_rules"`
	IsEnabled    *bool                        `form:"is_enabled" json:"is_enabled"`
	KnowledgeIDs []string                     `form:"knowledge_ids" json:"knowledge_ids"`
}

func (r imageGalleryRequest) toFilter() *types.ImageListFilter {
	return &types.ImageListFilter{
		Keyword:      r.Keyword,
		SearchIn:     r.SearchIn,
		SortBy:       r.SortBy,
		SortOrder:    r.SortOrder,
		AttrFilters:  r.AttrFilters,
		AttrRules:    r.AttrRules,
		IsEnabled:    r.IsEnabled,
		KnowledgeIDs: r.KnowledgeIDs,
	}
}

// resolveImageAssetSourceNames fills each asset's SourceName with a batch
// lookup. A failed lookup leaves the names empty — the gallery still lists
// the images with their knowledge IDs, it just doesn't label them.
func (h *ImageGalleryHandler) resolveImageAssetSourceNames(ctx context.Context, assets []types.ImageAsset) {
	if len(assets) == 0 {
		return
	}
	seen := make(map[string]bool, len(assets))
	ids := make([]string, 0, len(assets))
	for _, a := range assets {
		if a.KnowledgeID != "" && !seen[a.KnowledgeID] {
			seen[a.KnowledgeID] = true
			ids = append(ids, a.KnowledgeID)
		}
	}
	if len(ids) == 0 {
		return
	}
	tenantID := types.MustTenantIDFromContext(ctx)
	items, err := h.kgService.GetKnowledgeBatch(ctx, tenantID, ids)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_ids": ids,
			"tenant_id":     tenantID,
		})
		return
	}
	names := make(map[string]string, len(items))
	for _, item := range items {
		if item == nil || item.ID == "" {
			continue
		}
		// Zilan 的文档名为 Title，无标题的文件型文档回落到原始文件名。
		name := item.Title
		if name == "" {
			name = item.FileName
		}
		names[item.ID] = name
	}
	for i := range assets {
		assets[i].SourceName = names[assets[i].KnowledgeID]
	}
}

func (h *ImageGalleryHandler) listImages(c *gin.Context, request imageGalleryRequest) {
	ctx := c.Request.Context()

	// 与 ListKnowledge 相同的访问校验：tenant 范围 + KB 读权限（含共享 KB /
	// Agent 分享 / API Key scope），绝不允许跨工作区读取。
	_, kbID, effectiveTenantID, _, err := h.knowledgeHandler.validateKnowledgeBaseAccess(c)
	if err != nil {
		c.Error(err)
		return
	}
	// 用有效租户（KB 归属方）覆盖 ctx，保证共享访问下查询仍按 KB 所在
	// 工作区取数。
	ctx = context.WithValue(ctx, types.TenantIDContextKey, effectiveTenantID)

	switch c.Request.Method {
	case http.MethodPost:
		if len(request.KnowledgeIDs) > maxKnowledgeIDFilterBody {
			c.Error(errors.NewBadRequestError("too many knowledge_ids"))
			return
		}
	default:
		// GET：截断而非报错（上游行为），保证长过滤列表不因 URL 超长炸掉。
		if len(request.KnowledgeIDs) > maxKnowledgeIDFilterGET {
			request.KnowledgeIDs = request.KnowledgeIDs[:maxKnowledgeIDFilterGET]
		}
	}

	var pagination types.Pagination
	if err := c.ShouldBindQuery(&pagination); err != nil {
		logger.Error(ctx, "Failed to parse pagination parameters", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	logger.Infof(
		ctx,
		"Listing gallery images, kb_id=%s keyword=%s sort_by=%s sort_order=%s page=%d page_size=%d knowledge_ids=%d effectiveTenantID=%d",
		secutils.SanitizeForLog(kbID),
		secutils.SanitizeForLog(request.Keyword),
		request.SortBy,
		request.SortOrder,
		pagination.GetPage(),
		pagination.GetPageSize(),
		len(request.KnowledgeIDs),
		effectiveTenantID,
	)

	result, err := h.chunkService.ListImagesByKnowledgeBaseID(ctx, kbID, &pagination, request.toFilter())
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": kbID,
			"tenant_id":         effectiveTenantID,
		})
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	assets, _ := result.Data.([]types.ImageAsset)
	h.resolveImageAssetSourceNames(ctx, assets)

	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"data":      assets,
		"total":     result.Total,
		"page":      result.Page,
		"page_size": result.PageSize,
	})
}

// ListImages godoc
// @Summary      获取知识库图片画廊（查询串过滤）
// @Description  列出知识库内文档图片（每图去重），支持 caption/OCR 关键词搜索、排序与属性过滤
// @Tags         知识管理
// @Produce      json
// @Param        id          path   string  true   "知识库ID"
// @Param        page        query  int     false  "页码"
// @Param        page_size   query  int     false  "每页数量"
// @Param        keyword     query  string  false  "关键词（匹配图片说明/OCR文本）"
// @Param        sort_by     query  string  false  "排序字段（created_at/updated_at/caption）"
// @Param        sort_order  query  string  false  "排序方向（asc/desc）"
// @Param        knowledge_ids query string false "来源文档ID过滤，逗号分隔"
// @Success      200  {object}  map[string]interface{}  "图片资产列表"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-bases/{id}/images [get]
func (h *ImageGalleryHandler) ListImages(c *gin.Context) {
	var request imageGalleryRequest
	if err := c.ShouldBindQuery(&request); err != nil {
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}
	// 逗号分隔的 knowledge_ids 查询串（POST 用 JSON 数组）。gin 的 form
	// 绑定不拆逗号，这里手动解析，与上游 GET 形态一致。
	if raw := c.Query("knowledge_ids"); raw != "" {
		ids := make([]string, 0, 8)
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		request.KnowledgeIDs = ids
	}
	h.listImages(c, request)
}

// ListImagesPost godoc
// @Summary      获取知识库图片画廊（JSON 过滤）
// @Description  同 GET /images，过滤条件放请求体（多来源文档组合时避免查询串超长）
// @Tags         知识管理
// @Accept       json
// @Produce      json
// @Param        id   path  string  true  "知识库ID"
// @Param        page query int     false "页码"
// @Param        page_size query int false "每页数量"
// @Param        body body imageGalleryRequest false "过滤条件"
// @Success      200  {object}  map[string]interface{}  "图片资产列表"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-bases/{id}/images [post]
func (h *ImageGalleryHandler) ListImagesPost(c *gin.Context) {
	var request imageGalleryRequest
	// 空 body 视为无过滤条件（上游行为），其余 JSON 错误仍然 400。
	if err := c.ShouldBindJSON(&request); err != nil && !goerrors.Is(err, io.EOF) {
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}
	h.listImages(c, request)
}
