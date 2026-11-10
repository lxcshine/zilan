package router

import (
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/gin-gonic/gin"
)

// RegisterImageGalleryRoutes 注册 FR-18 文档图片画廊路由。
//
// 画廊是纯读侧功能：两个端点都是 Viewer+，且必须过 g.KBAccessRead("id")
// （解析 own/shared/agent-visible 访问并改写请求的租户上下文），handler 内
// 再走一遍 validateKnowledgeBaseAccess 兜底 —— 与 knowledge 列表一致，
// 绝不允许跨工作区读取。API key 需 retrieve 或 full-access（kb）。
func RegisterImageGalleryRoutes(r *gin.RouterGroup, h *handler.ImageGalleryHandler, g *rbacGuards) {
	if h == nil {
		return
	}
	images := g.apiKeyGroup(r.Group("/knowledge-bases/:id/images"), apiKeyRetrieve(apiKeyFullAccess()))
	{
		// GET：查询串过滤（keyword/sort/attr_filters/knowledge_ids 逗号分隔）。
		images.GET("", g.Viewer(), g.KBAccessRead("id"), h.ListImages)
		// POST：JSON 过滤。多来源文档组合（knowledge_ids 数组）时用 POST，
		// 避免 GET 查询串超长。同 GET 一样只读，故 retrieve 能力的 key 也可达。
		images.POST("", g.Viewer(), g.KBAccessRead("id"), h.ListImagesPost)
	}
}
