package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// GraphSearchRepository 与 Agent 侧的图谱召回契约（RetrieveGraphRepository）
// 分开定义：公开 API 需要的是孤立实体、有界字段与显式的上限元信息，
// 而召回链路要的是可以塞进提示词的文本。
type GraphSearchRepository interface {
	SearchGraph(context.Context, types.NameSpace, string) (*types.GraphSearchCandidates, error)
}

// GraphSearchEvidenceRepository 读取“已校验且有界”的来源正文。
type GraphSearchEvidenceRepository interface {
	// GetGraphSourceChunks 在**同一个** owner 租户与知识库范围内联查仍然有效的
	// chunk / knowledge，并在同一次 SQL 读取里完成校验与有界投影。
	GetGraphSourceChunks(context.Context, uint64, string, []string) ([]types.GraphSearchSourceChunk, error)
}

// GraphSearchService 要求调用方在任何图谱/数据库读取之前已经拿到该知识库的
// 明确授权（由路由守卫与 handler 的 validateAndGetKnowledgeBase 保证）。
type GraphSearchService interface {
	Search(context.Context, *types.KnowledgeBase, types.GraphSearchRequest) (*types.GraphSearchResult, error)
}
