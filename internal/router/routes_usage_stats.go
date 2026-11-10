package router

import (
	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/handler"
)

// RegisterTokenUsageRoutes 注册 FR-12 的每轮 Token 用量看板路由。
//
// 独立成文件而不是塞进既有路由文件：/memory/admin 所在的
// routes_auth_tenant.go 由别的特性维护，混在一起会让两条互不相干的权限面
// 在同一次改动里互相牵制。
//
// 权限（PRD §5.1 权限矩阵）：整组挂 g.Admin()——只有工作区管理员（Admin/Owner）
// 能看聚合视图；服务层还会再校验一次角色并拒绝 API Key 主体，双重保险。
// 这里刻意不套 g.apiKeyGroup：未声明的 API Key 路由由 /api/v1 上的 API Key 网关
// 默认拒绝，看板因此对机器主体天然关闭。
//
// 跨工作区聚合入口没有实现：仓储层每条查询都写死 tenant_id，工作区边界不依赖
// 调用方传参。若将来要开跨工作区视图，必须叠加
// middleware.RequireCrossTenantAccess(cfg)（集群开关 + 超级权限属性双条件，
// 缺一拒绝），且只返回聚合数字。
//
// usageHandler 可能为 nil（未装配该依赖的构建形态）：此时是空注册，
// 优于启动即崩。
func RegisterTokenUsageRoutes(r *gin.RouterGroup, usageHandler *handler.UsageStatsHandler, g *rbacGuards) {
	if usageHandler == nil || g == nil {
		return
	}
	// 单轮明细不走这里：它随消息持久化，由 GET /messages/:session_id/load 带出
	// （usage / context_stats 两个字段），因此本组只有一个聚合端点。
	usage := r.Group("/token-usage", g.Admin())
	{
		usage.GET("/overview", usageHandler.GetTokenUsageOverview)
	}
}
