package neo4j

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// NewGraphSearchRepository 构造公开图谱查询适配器。它与 NewNeo4jRepository
// 返回同一个具体类型，但只暴露 SearchGraph 这一个有界只读入口。
func NewGraphSearchRepository(driver neo4j.Driver) interfaces.GraphSearchRepository {
	return &Neo4jRepository{driver: driver, nodePrefix: "ENTITY"}
}

// graphSearchProjection 在数据跨越 Bolt 之前就把字段限住。每一项（或每一个
// 字符）都多取一个，好让服务层能上报“被裁掉了”，而不是给出一个假的总数。
func graphSearchProjection(alias string) string {
	return alias + ` {
		id: elementId(` + alias + `),
		name: substring(` + alias + `.name, 0, 513),
		knowledge_id: substring(` + alias + `.kg, 0, 129),
		chunks: [c IN ` + alias + `.chunks[..513] | substring(c, 0, 129)],
		attributes: [a IN ` + alias + `.attributes[..21] | substring(a, 0, 1001)]
	}`
}

// graphSearchLabel 给每个标识符加反引号。知识库 ID 是数据、不是 Cypher，
// 包括历史上导入的、含标点或反引号的 ID。
func (n *Neo4jRepository) graphSearchLabel(namespace types.NameSpace) (string, error) {
	if namespace.KnowledgeBase == "" || namespace.Knowledge != "" {
		return "", fmt.Errorf("graph search requires exactly one knowledge base")
	}
	labels := n.Labels(namespace)
	for i, label := range labels {
		labels[i] = "`" + strings.ReplaceAll(label, "`", "``") + "`"
	}
	return strings.Join(labels, ":"), nil
}

// SearchGraph 返回排序后的种子实体以及有界的一跳邻域。
// driver 为 nil（未部署 Neo4j）时返回 (nil, nil)，与“后端可用但没有匹配”
// 区分开：服务层据此返回明确的 503，而不是一个看起来正常的空结果。
func (n *Neo4jRepository) SearchGraph(
	ctx context.Context, namespace types.NameSpace, query string,
) (*types.GraphSearchCandidates, error) {
	if n.driver == nil {
		return nil, nil
	}
	label, err := n.graphSearchLabel(namespace)
	if err != nil {
		return nil, err
	}
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() {
		if err := session.Close(ctx); err != nil {
			logger.Warnf(ctx, "Failed to close graph query session: %v", err)
		}
	}()
	value, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		out := &types.GraphSearchCandidates{Graph: &types.GraphData{}}
		seedResult, err := tx.Run(ctx, `
			MATCH (n:`+label+`)
			WHERE n.name CONTAINS $query
			WITH n ORDER BY CASE WHEN n.name = $query THEN 0 ELSE 1 END,
				size(n.name), n.name, n.kg, elementId(n)
			LIMIT $probeLimit
			RETURN `+graphSearchProjection("n")+` AS n`,
			map[string]any{"query": query, "probeLimit": types.GraphSearchMaxSeeds + 1})
		if err != nil {
			return nil, err
		}
		seedIDs := []string{}
		seen := map[string]bool{}
		for seedResult.Next(ctx) {
			if len(seedIDs) == types.GraphSearchMaxSeeds {
				out.TruncationReasons = append(out.TruncationReasons, "candidate_seeds")
				continue
			}
			node, err := decodeGraphSearchProjection(seedResult.Record(), "n")
			if err != nil {
				return nil, err
			}
			seedIDs = append(seedIDs, node.ID)
			seen[node.ID] = true
			out.Graph.Node = append(out.Graph.Node, node)
		}
		if err := seedResult.Err(); err != nil {
			return nil, err
		}
		if len(seedIDs) == 0 {
			return out, nil
		}
		// 保留没有关系边的种子；每条已存储的边只从它最早的那个种子出发扩展，
		// 因此命中目标既不能反转存储方向，也不能把预算占两次。
		// 两个端点都必须带知识库标签。
		rows, err := tx.Run(ctx, `
			UNWIND range(0, size($seeds) - 1) AS seed_index
			MATCH (n:`+label+`) WHERE elementId(n) = $seeds[seed_index]
			MATCH (n)-[r]-(m:`+label+`)
			WHERE NOT elementId(m) IN $seeds[0..seed_index]
			WITH n, r, m, seed_index ORDER BY seed_index, elementId(r)
			LIMIT $probeLimit
			RETURN `+graphSearchProjection("n")+` AS n, `+graphSearchProjection("m")+` AS m,
				elementId(r) AS id, elementId(startNode(r)) AS source_id,
				elementId(endNode(r)) AS target_id, substring(type(r), 0, 129) AS type`,
			map[string]any{"seeds": seedIDs, "probeLimit": types.GraphSearchMaxRows + 1})
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next(ctx) {
			if count == types.GraphSearchMaxRows {
				out.TruncationReasons = append(out.TruncationReasons, "candidate_relations")
				continue
			}
			count++
			for _, alias := range []string{"n", "m"} {
				node, err := decodeGraphSearchProjection(rows.Record(), alias)
				if err != nil {
					return nil, err
				}
				if !seen[node.ID] {
					out.Graph.Node = append(out.Graph.Node, node)
					seen[node.ID] = true
				}
			}
			relation := &types.GraphRelation{}
			for _, field := range []struct {
				name string
				dest *string
			}{
				{"id", &relation.ID}, {"source_id", &relation.SourceID},
				{"target_id", &relation.TargetID}, {"type", &relation.Type},
			} {
				v, err := graphSearchString(rows.Record(), field.name)
				if err != nil || v == "" {
					return nil, fmt.Errorf("invalid graph relationship field %s", field.name)
				}
				*field.dest = v
			}
			if !seen[relation.SourceID] || !seen[relation.TargetID] {
				return nil, fmt.Errorf("graph relationship has an unreturned endpoint")
			}
			out.Graph.Relation = append(out.Graph.Relation, relation)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return out, nil
	}, neo4j.WithTxTimeout(types.GraphSearchTimeout))
	if err != nil {
		return nil, err
	}
	candidates, ok := value.(*types.GraphSearchCandidates)
	if !ok {
		return nil, fmt.Errorf("unexpected graph search result type")
	}
	return candidates, nil
}

// graphSearchString 从记录里取出一个非空字符串字段。
func graphSearchString(record *neo4j.Record, field string) (string, error) {
	if record == nil {
		return "", fmt.Errorf("nil graph search record")
	}
	value, ok := record.Get(field)
	if !ok {
		return "", fmt.Errorf("graph record has no field %s", field)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("graph record field %s is not a string", field)
	}
	return text, nil
}

// decodeGraphSearchProjection 把投影出来的 map 还原成 GraphNode。任何字段缺失
// 或类型不符都直接报错——宁可让一次查询失败，也不要把脏数据放进公开响应。
func decodeGraphSearchProjection(record *neo4j.Record, alias string) (*types.GraphNode, error) {
	if record == nil {
		return nil, fmt.Errorf("nil graph search record")
	}
	value, ok := record.Get(alias)
	props, isMap := value.(map[string]any)
	if !ok || !isMap {
		return nil, fmt.Errorf("invalid graph node projection")
	}
	node := &types.GraphNode{}
	for _, field := range []struct {
		name string
		dest *string
	}{
		{"id", &node.ID}, {"name", &node.Name}, {"knowledge_id", &node.KnowledgeID},
	} {
		text, isString := props[field.name].(string)
		if !isString || text == "" {
			return nil, fmt.Errorf("graph node has no %s", field.name)
		}
		*field.dest = text
	}
	for _, field := range []struct {
		name string
		dest *[]string
	}{
		{"chunks", &node.Chunks}, {"attributes", &node.Attributes},
	} {
		*field.dest = []string{}
		if props[field.name] == nil {
			continue
		}
		values, isList := props[field.name].([]any)
		if !isList {
			return nil, fmt.Errorf("invalid graph node %s", field.name)
		}
		for _, value := range values {
			text, isString := value.(string)
			if !isString {
				return nil, fmt.Errorf("invalid graph node %s item", field.name)
			}
			*field.dest = append(*field.dest, text)
		}
	}
	return node, nil
}
