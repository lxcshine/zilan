package router

import (
	"net/http"

	"github.com/Tencent/WeKnora/internal/application/repository"
	neo4jRepo "github.com/Tencent/WeKnora/internal/application/repository/retriever/neo4j"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RegisterGraphSearchRoutes 注册知识图谱 JSON 检索入口（FR-19③）。
//
// 尽管查询是 POST 提交的，它在语义上是一条只读的知识库内容路由：角色、
// API Key 能力与“对该知识库的明确授权”三者相互独立，缺一不可。
//
// h 为 nil 时按依赖就地装配（容器尚未提供专用 handler），依赖不齐则跳过注册
// ——宁可少一条路由，也不要注册一个必然 500 的端点。
func RegisterGraphSearchRoutes(
	r *gin.RouterGroup,
	h *handler.GraphSearchHandler,
	kbHandler *handler.KnowledgeBaseHandler,
	db *gorm.DB,
	graphRepo interfaces.RetrieveGraphRepository,
	g *rbacGuards,
) {
	if h == nil {
		h = newGraphSearchHandler(kbHandler, db, graphRepo)
	}
	if h == nil {
		return
	}
	g.apiKeyRoute(r, http.MethodPost, "/knowledge-bases/:id/graph-search",
		apiKeyRetrieve(apiKeyFullAccess()), g.Viewer(), g.KBAccessRead("id"), h.Search)
}

// newGraphSearchHandler 用已有依赖装配图谱检索 handler：图谱侧复用容器里
// 已经注册的 Neo4j 仓储实例（同一个具体类型同时实现 GraphSearchRepository），
// 溯源侧只需要一个 *gorm.DB。任一依赖缺失就返回 nil。
func newGraphSearchHandler(
	kbHandler *handler.KnowledgeBaseHandler,
	db *gorm.DB,
	graphRepo interfaces.RetrieveGraphRepository,
) *handler.GraphSearchHandler {
	if kbHandler == nil || db == nil {
		return nil
	}
	var searchRepo interfaces.GraphSearchRepository
	if graphRepo != nil {
		// 未部署 Neo4j 时 graphRepo 内部的 driver 为 nil，SearchGraph 会返回
		// (nil, nil)，服务层据此给出明确的 503。
		searchRepo, _ = graphRepo.(interfaces.GraphSearchRepository)
	}
	if searchRepo == nil {
		// 容器里注册的实例不是 Neo4j 实现（例如换成了别的图谱后端）时，
		// 退化为“未配置”，由服务层返回 503 而不是 panic。
		searchRepo, _ = neo4jRepo.NewGraphSearchRepository(nil).(interfaces.GraphSearchRepository)
	}
	if searchRepo == nil {
		return nil
	}
	return handler.NewGraphSearchHandler(kbHandler,
		service.NewGraphSearchService(searchRepo, repository.NewGraphSearchEvidenceRepository(db)))
}
