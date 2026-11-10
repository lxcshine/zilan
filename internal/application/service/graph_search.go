package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"unicode/utf8"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// graphSearchService 是知识图谱 JSON 检索入口的“溯源边界”：图谱给出候选实体
// 与关系，关系型库负责证明这些实体引用的分块此刻仍然有效、并且确实属于这个
// 知识库的这个租户。任何一侧不满足，对应实体就不会出现在响应里。
type graphSearchService struct {
	graph    interfaces.GraphSearchRepository
	evidence interfaces.GraphSearchEvidenceRepository
}

// NewGraphSearchService 构造上述服务。两个依赖都可以为 nil：图谱后端未部署时
// Search 会返回明确的 503，而不是一个看起来正常的空结果。
func NewGraphSearchService(graph interfaces.GraphSearchRepository,
	evidence interfaces.GraphSearchEvidenceRepository,
) interfaces.GraphSearchService {
	return &graphSearchService{graph: graph, evidence: evidence}
}

// Search 执行一次有界的实体名子串查询。
//
// 授权前提：调用方（路由守卫 + handler.validateAndGetKnowledgeBase）已经确认
// 调用者对这个知识库有读权限；这里再补一层 API Key 能力校验，避免一把只有
// 写权限的 key 通过检索能力越权读取图谱内容。
func (s *graphSearchService) Search(
	ctx context.Context, kb *types.KnowledgeBase, req types.GraphSearchRequest,
) (*types.GraphSearchResult, error) {
	if kb == nil {
		return nil, apperrors.NewNotFoundError("knowledge base not found")
	}
	if scope, ok := types.TenantAPIKeyScopeFromContext(ctx); ok && !scope.FullAccess &&
		!scope.HasCapability(types.APIKeyCapabilityRetrieve) {
		return nil, apperrors.NewForbiddenError("The API key requires retrieve capability")
	}
	if err := req.Normalize(); err != nil {
		return nil, apperrors.NewBadRequestError(err.Error())
	}
	if !kb.IsGraphEnabled() {
		return nil, apperrors.NewConflictError("Knowledge graph is not enabled for this knowledge base")
	}
	if s.graph == nil || s.evidence == nil {
		return nil, apperrors.NewServiceUnavailableError("Knowledge graph backend is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, types.GraphSearchTimeout)
	defer cancel()

	candidates, err := s.graph.SearchGraph(ctx, types.NameSpace{KnowledgeBase: kb.ID}, req.Query)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"knowledge_base_id": kb.ID})
		return nil, apperrors.NewServiceUnavailableError("Knowledge graph query is temporarily unavailable")
	}
	if candidates == nil || candidates.Graph == nil {
		// 与“查到 0 个实体”区分开：这里说明图谱后端根本没配置。
		return nil, apperrors.NewServiceUnavailableError("Knowledge graph backend is not configured")
	}

	result := &types.GraphSearchResult{
		KnowledgeBaseID: kb.ID, Query: req.Query,
		Entities: []types.GraphSearchEntity{}, Relationships: []types.GraphSearchRelation{},
		SourceChunks: []types.GraphSearchSourceChunk{},
		Meta: types.GraphSearchMeta{
			TruncationReasons: []string{}, MaxNodes: req.MaxNodes,
			MaxRelations: req.MaxRelations, MaxSourceChunks: req.MaxSourceChunks,
		},
	}
	mark := func(reason string) {
		result.Meta.Truncated = true
		if !slices.Contains(result.Meta.TruncationReasons, reason) {
			result.Meta.TruncationReasons = append(result.Meta.TruncationReasons, reason)
		}
	}
	for _, reason := range candidates.TruncationReasons {
		mark(reason)
	}

	// 第一遍：收集需要校验的 chunk ID，并把明显越界的实体字段挡在外面。
	ids := []string{}
	wanted := map[string]bool{}
	eligible := map[string][]string{}
	for _, node := range candidates.Graph.Node {
		if node == nil || node.ID == "" || node.Name == "" || node.KnowledgeID == "" || len(node.Chunks) == 0 {
			continue
		}
		if utf8.RuneCountInString(node.Name) > types.GraphSearchMaxNameRunes ||
			len(node.KnowledgeID) > 128 || len(node.Chunks) > types.GraphSearchMaxEvidence {
			mark("entity_fields")
			continue
		}
		chunkIDs := []string{}
		additional := []string{}
		valid := true
		for _, id := range node.Chunks {
			if id == "" || len(id) > 128 {
				valid = false
				mark("entity_fields")
				break
			}
			if !slices.Contains(chunkIDs, id) {
				chunkIDs = append(chunkIDs, id)
				if !wanted[id] {
					additional = append(additional, id)
				}
			}
		}
		if !valid {
			continue
		}
		if len(ids)+len(additional) > types.GraphSearchMaxEvidence {
			mark("candidate_source_chunks")
			continue
		}
		for _, id := range additional {
			wanted[id] = true
			ids = append(ids, id)
		}
		slices.Sort(chunkIDs)
		eligible[node.ID] = chunkIDs
	}

	// 第二遍：向关系型库确认这些分块此刻仍然有效且属于本租户本库。
	sources := map[string]types.GraphSearchSourceChunk{}
	if len(ids) != 0 {
		rows, err := s.evidence.GetGraphSourceChunks(ctx, kb.TenantID, kb.ID, ids)
		if err != nil {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{"knowledge_base_id": kb.ID})
			return nil, apperrors.NewInternalServerError("Failed to validate graph source chunks")
		}
		for _, row := range rows {
			if wanted[row.ID] && row.TenantID == kb.TenantID && row.KnowledgeBaseID == kb.ID {
				sources[row.ID] = row
			}
		}
	}

	// 保守溯源：实体引用的**每一个**分块都必须仍然启用、有效、且属于同一篇
	// 文档。属性是文档级聚合，只校验其中一个存活分块可能把已禁用来源的事实
	// 暴露出去。
	nodes := map[string]types.GraphSearchEntity{}
	publicIDs := map[string]bool{}
	returnedSources := map[string]bool{}
	for _, node := range candidates.Graph.Node {
		if node == nil {
			continue
		}
		chunkIDs, ok := eligible[node.ID]
		if !ok {
			continue
		}
		valid, extra := true, 0
		for _, id := range chunkIDs {
			if row, ok := sources[id]; !ok || row.KnowledgeID != node.KnowledgeID {
				valid = false
				break
			}
			if !returnedSources[id] {
				extra++
			}
		}
		if !valid {
			continue
		}
		id := graphSearchPublicID("entity", kb.ID, node.KnowledgeID, node.Name)
		if publicIDs[id] {
			continue
		}
		if len(result.Entities) >= req.MaxNodes {
			mark("max_nodes")
			continue
		}
		if len(result.SourceChunks)+extra > req.MaxSourceChunks {
			mark("max_source_chunks")
			continue
		}
		attributes := []string{}
		for i, attribute := range node.Attributes {
			if i == types.GraphSearchMaxAttributes {
				mark("entity_fields")
				break
			}
			text := []rune(attribute)
			if len(text) > types.GraphSearchMaxAttributeRunes {
				attribute = string(text[:types.GraphSearchMaxAttributeRunes])
				mark("entity_fields")
			}
			attributes = append(attributes, attribute)
		}
		entity := types.GraphSearchEntity{
			ID: id, Name: node.Name, KnowledgeID: node.KnowledgeID,
			Attributes: attributes, SourceChunkIDs: chunkIDs,
		}
		nodes[node.ID] = entity
		publicIDs[id] = true
		result.Entities = append(result.Entities, entity)
		for _, id := range chunkIDs {
			if !returnedSources[id] {
				row := sources[id]
				result.SourceChunks = append(result.SourceChunks, row)
				returnedSources[id] = true
				if row.ContentTruncated || row.TitleTruncated {
					mark("source_fields")
				}
			}
		}
	}

	// 关系只保留两端都已返回、且属于同一篇文档的边；方向按图谱里存储的方向。
	seenRelations := map[string]bool{}
	for _, relation := range candidates.Graph.Relation {
		if relation == nil {
			continue
		}
		source, sourceOK := nodes[relation.SourceID]
		target, targetOK := nodes[relation.TargetID]
		if !sourceOK || !targetOK || source.KnowledgeID != target.KnowledgeID || relation.Type == "" {
			continue
		}
		if utf8.RuneCountInString(relation.Type) > 128 {
			mark("relation_fields")
			continue
		}
		id := graphSearchPublicID("relation", source.ID, target.ID, relation.Type)
		if seenRelations[id] {
			continue
		}
		seenRelations[id] = true
		if len(result.Relationships) >= req.MaxRelations {
			mark("max_relations")
			continue
		}
		chunkIDs := append([]string{}, source.SourceChunkIDs...)
		for _, id := range target.SourceChunkIDs {
			if !slices.Contains(chunkIDs, id) {
				chunkIDs = append(chunkIDs, id)
			}
		}
		slices.Sort(chunkIDs)
		result.Relationships = append(result.Relationships, types.GraphSearchRelation{
			ID: id, SourceID: source.ID, TargetID: target.ID, Type: relation.Type, SourceChunkIDs: chunkIDs,
		})
	}
	return result, nil
}

// graphSearchPublicID 用 JSON 元组编码避免分隔符冲突。公开 ID 在图谱重建后
// 依然稳定，既不会泄露 Neo4j 的 elementId，也不会把不同文档里的同名实体混为
// 一个。
func graphSearchPublicID(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
