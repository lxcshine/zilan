package types

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 内部候选预算与调用方的响应上限是两件事：前者约束一次查询最多从图谱/数据库
// 里捞多少行，后者约束最终 JSON 里返回多少行。两者都必须有界。
const (
	GraphSearchMaxSeeds          = 100
	GraphSearchMaxRows           = 512
	GraphSearchMaxEvidence       = 512
	GraphSearchMaxNameRunes      = 512
	GraphSearchMaxAttributeRunes = 1000
	GraphSearchMaxAttributes     = 20
	GraphSearchMaxContentRunes   = 2000
	GraphSearchTimeout           = 10 * time.Second
)

// GraphSearchRequest 是知识图谱 JSON 检索入口的请求体（FR-19③）。
// 语义是**区分大小写的实体名子串匹配**：不调用模型、不生成 embedding、
// 不介入常规检索链路，因此结果稳定、可被外部系统按结构直接消费。
type GraphSearchRequest struct {
	Query           string `json:"query" binding:"required" minLength:"1" maxLength:"512"`
	MaxNodes        int    `json:"max_nodes" minimum:"0" maximum:"100" default:"50"`
	MaxRelations    int    `json:"max_relations" minimum:"0" maximum:"200" default:"100"`
	MaxSourceChunks int    `json:"max_source_chunks" minimum:"0" maximum:"50" default:"20"`
}

// Normalize 同时服务 HTTP 与直接调用服务层的调用方：校验取值并把 0 归一成
// 默认上限（0 表示“用默认值”，负数与超上限都是错误）。
func (r *GraphSearchRequest) Normalize() error {
	r.Query = strings.TrimSpace(r.Query)
	if r.Query == "" || !utf8.ValidString(r.Query) || utf8.RuneCountInString(r.Query) > GraphSearchMaxNameRunes {
		return fmt.Errorf("query must contain 1 to %d characters", GraphSearchMaxNameRunes)
	}
	for _, limit := range []struct {
		name                  string
		value                 *int
		defaultValue, maximum int
	}{
		{"max_nodes", &r.MaxNodes, 50, 100},
		{"max_relations", &r.MaxRelations, 100, 200},
		{"max_source_chunks", &r.MaxSourceChunks, 20, 50},
	} {
		if *limit.value < 0 || *limit.value > limit.maximum {
			return fmt.Errorf("%s must be between 1 and %d, or 0 for the default", limit.name, limit.maximum)
		}
		if *limit.value == 0 {
			*limit.value = limit.defaultValue
		}
	}
	return nil
}

// GraphSearchCandidates 是内部结构：图谱的物理身份（Neo4j elementId 等）
// 绝不跨越公开 API 边界。TruncationReasons 上报的是“候选被上限裁掉了”，
// 而不是编造一个总数。
type GraphSearchCandidates struct {
	Graph             *GraphData
	TruncationReasons []string
}

// GraphSearchEntity 是某个文档实例内的一个实体。
type GraphSearchEntity struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	KnowledgeID    string   `json:"knowledge_id"`
	Attributes     []string `json:"attributes"`
	SourceChunkIDs []string `json:"source_chunk_ids"`
}

// GraphSearchRelation 连接两个**都已返回**且属于同一文档的实体。
type GraphSearchRelation struct {
	ID             string   `json:"id"`
	SourceID       string   `json:"source_id"`
	TargetID       string   `json:"target_id"`
	Type           string   `json:"type"`
	SourceChunkIDs []string `json:"source_chunk_ids"`
}

// GraphSearchSourceChunk 是有界的、取自实时 SQL 的投影，而不是原始 Chunk 或
// Knowledge 行：它刻意剔除文件路径、metadata、图片 URL 与所有者信息。
type GraphSearchSourceChunk struct {
	ID               string `json:"id"`
	KnowledgeID      string `json:"knowledge_id"`
	Title            string `json:"title"`
	Content          string `json:"content"`
	ChunkIndex       int    `json:"chunk_index"`
	StartAt          int    `json:"start_at"`
	EndAt            int    `json:"end_at"`
	ContentTruncated bool   `json:"content_truncated" gorm:"-"`
	TitleTruncated   bool   `json:"title_truncated" gorm:"-"`
	TenantID         uint64 `json:"-"`
	KnowledgeBaseID  string `json:"-"`
}

// GraphSearchMeta 上报实际生效的上限与观察到的裁剪，不报“总数”。
type GraphSearchMeta struct {
	Truncated         bool     `json:"truncated"`
	TruncationReasons []string `json:"truncation_reasons"`
	MaxNodes          int      `json:"max_nodes"`
	MaxRelations      int      `json:"max_relations"`
	MaxSourceChunks   int      `json:"max_source_chunks"`
}

// GraphSearchResult 是稳定的公开 JSON 契约，与内部 GraphData 解耦。
type GraphSearchResult struct {
	KnowledgeBaseID string                   `json:"knowledge_base_id"`
	Query           string                   `json:"query"`
	Entities        []GraphSearchEntity      `json:"entities"`
	Relationships   []GraphSearchRelation    `json:"relationships"`
	SourceChunks    []GraphSearchSourceChunk `json:"source_chunks"`
	Meta            GraphSearchMeta          `json:"meta"`
}
