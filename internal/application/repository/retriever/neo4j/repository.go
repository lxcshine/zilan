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

// Neo4jRepository is a repository for Neo4j
type Neo4jRepository struct {
	driver     neo4j.Driver
	nodePrefix string
}

// NewNeo4jRepository creates a new Neo4j repository
func NewNeo4jRepository(driver neo4j.Driver) interfaces.RetrieveGraphRepository {
	return &Neo4jRepository{driver: driver, nodePrefix: "ENTITY"}
}

// _remove_hyphen removes hyphens from a string
func _remove_hyphen(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

// Labels returns the labels for a namespace
func (n *Neo4jRepository) Labels(namespace types.NameSpace) []string {
	res := make([]string, 0)
	for _, label := range namespace.Labels() {
		res = append(res, n.nodePrefix+_remove_hyphen(label))
	}
	return res
}

// Label returns the label for a namespace
func (n *Neo4jRepository) Label(namespace types.NameSpace) string {
	labels := n.Labels(namespace)
	return strings.Join(labels, ":")
}

// AddGraph adds a graph to the Neo4j repository
func (n *Neo4jRepository) AddGraph(ctx context.Context, namespace types.NameSpace, graphs []*types.GraphData) error {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil
	}
	for _, graph := range graphs {
		if err := n.addGraph(ctx, namespace, graph); err != nil {
			return err
		}
	}
	return nil
}

// addGraph adds a graph to the Neo4j repository
func (n *Neo4jRepository) addGraph(ctx context.Context, namespace types.NameSpace, graph *types.GraphData) error {
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		// Node import query
		node_import_query := `
			UNWIND $data AS row
			CALL apoc.merge.node(row.labels, {name: row.name, kg: row.knowledge_id}, row.props, {}) YIELD node
			SET node.chunks = apoc.coll.union(node.chunks, row.chunks)
			RETURN distinct 'done' AS result
		`
		nodeData := []map[string]interface{}{}
		for _, node := range graph.Node {
			nodeData = append(nodeData, map[string]interface{}{
				"name":         node.Name,
				"knowledge_id": namespace.Knowledge,
				"props":        map[string][]string{"attributes": node.Attributes},
				"chunks":       node.Chunks,
				"labels":       n.Labels(namespace),
			})
		}
		if _, err := tx.Run(ctx, node_import_query, map[string]interface{}{"data": nodeData}); err != nil {
			return nil, fmt.Errorf("failed to create nodes: %v", err)
		}

		// Relationship import query
		rel_import_query := `
			UNWIND $data AS row
			CALL apoc.merge.node(row.source_labels, {name: row.source, kg: row.knowledge_id}, {}, {}) YIELD node as source
			CALL apoc.merge.node(row.target_labels, {name: row.target, kg: row.knowledge_id}, {}, {}) YIELD node as target
			CALL apoc.merge.relationship(source, row.type, {}, row.attributes, target) YIELD rel
			RETURN distinct 'done'
		`
		relData := []map[string]interface{}{}
		for _, rel := range graph.Relation {
			relData = append(relData, map[string]interface{}{
				"source":        rel.Node1,
				"target":        rel.Node2,
				"knowledge_id":  namespace.Knowledge,
				"type":          rel.Type,
				"source_labels": n.Labels(namespace),
				"target_labels": n.Labels(namespace),
			})
		}
		if _, err := tx.Run(ctx, rel_import_query, map[string]interface{}{"data": relData}); err != nil {
			return nil, fmt.Errorf("failed to create relationships: %v", err)
		}
		return nil, nil
	})
	if err != nil {
		logger.Errorf(ctx, "failed to add graph: %v", err)
		return err
	}
	return nil
}

// DelGraph deletes a graph from the Neo4j repository
func (n *Neo4jRepository) DelGraph(ctx context.Context, namespaces []types.NameSpace) error {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil
	}
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)

	result, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		for _, namespace := range namespaces {
			labelExpr := n.Label(namespace)

			deleteRelsQuery := `
				CALL apoc.periodic.iterate(
					"MATCH (n:` + labelExpr + ` {kg: $knowledge_id})-[r]-(m:` + labelExpr + ` {kg: $knowledge_id}) RETURN r",
					"DELETE r",
					{batchSize: 1000, parallel: true, params: {knowledge_id: $knowledge_id}}
				) YIELD batches, total
				RETURN total
        	`
			if _, err := tx.Run(ctx, deleteRelsQuery, map[string]interface{}{"knowledge_id": namespace.Knowledge}); err != nil {
				return nil, fmt.Errorf("failed to delete relationships: %v", err)
			}

			deleteNodesQuery := `
				CALL apoc.periodic.iterate(
					"MATCH (n:` + labelExpr + ` {kg: $knowledge_id}) RETURN n",
					"DELETE n",
					{batchSize: 1000, parallel: true, params: {knowledge_id: $knowledge_id}}
				) YIELD batches, total
				RETURN total
        	`
			if _, err := tx.Run(ctx, deleteNodesQuery, map[string]interface{}{"knowledge_id": namespace.Knowledge}); err != nil {
				return nil, fmt.Errorf("failed to delete nodes: %v", err)
			}
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	logger.Infof(ctx, "delete graph result: %v", result)
	return nil
}

// getStringList safely extracts a string list property from a Neo4j node.
// Placeholder nodes created by the relationship import carry only name/kg,
// so a direct type assertion would panic; missing/!list values yield nil.
func getStringList(props map[string]interface{}, key string) []string {
	raw, ok := props[key]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, len(list))
	for i, v := range list {
		result[i] = fmt.Sprintf("%v", v)
	}
	return result
}

// recordToGraph accumulates one (n, r, m) triple into graphData, deduping
// nodes by name and relationships by element id.
func recordToGraph(graphData *types.GraphData, nodeSeen, relSeen map[string]bool,
	node, rel, target interface{},
) {
	nodeData, ok := node.(neo4j.Node)
	if !ok {
		return
	}
	targetNodeData, ok := target.(neo4j.Node)
	if !ok {
		return
	}
	for _, n := range []neo4j.Node{nodeData, targetNodeData} {
		nameStr, ok := n.Props["name"].(string)
		if !ok || nameStr == "" {
			continue
		}
		if !nodeSeen[nameStr] {
			nodeSeen[nameStr] = true
			graphData.Node = append(graphData.Node, &types.GraphNode{
				Name:       nameStr,
				Chunks:     getStringList(n.Props, "chunks"),
				Attributes: getStringList(n.Props, "attributes"),
			})
		}
	}
	if rel == nil {
		return
	}
	relData, ok := rel.(neo4j.Relationship)
	if !ok {
		return
	}
	relID := relData.ElementId
	if relSeen[relID] {
		return
	}
	relSeen[relID] = true
	src, _ := nodeData.Props["name"].(string)
	dst, _ := targetNodeData.Props["name"].(string)
	if src == "" || dst == "" {
		return
	}
	graphData.Relation = append(graphData.Relation, &types.GraphRelation{
		Node1: src,
		Node2: dst,
		Type:  relData.Type,
	})
}

// collectNames runs a query returning (n, r, m) triples and returns the
// graph plus the retained node names.
func (n *Neo4jRepository) collectGraph(ctx context.Context, tx neo4j.ManagedTransaction,
	query string, params map[string]interface{},
) (*types.GraphData, []string, error) {
	result, err := tx.Run(ctx, query, params)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to run query: %v", err)
	}
	graphData := &types.GraphData{}
	nodeSeen := make(map[string]bool)
	relSeen := make(map[string]bool)
	for result.Next(ctx) {
		record := result.Record()
		node, _ := record.Get("n")
		rel, _ := record.Get("r")
		targetNode, _ := record.Get("m")
		recordToGraph(graphData, nodeSeen, relSeen, node, rel, targetNode)
	}
	if err := result.Err(); err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(nodeSeen))
	for name := range nodeSeen {
		names = append(names, name)
	}
	return graphData, names, nil
}

// SearchSubgraph returns the bounded ego-graph around the given entity names:
// seed nodes matched by name CONTAINS, then expanded up to maxLevel hops via
// apoc.path.expandConfig (BFS, hard node limit). Only nodes carrying the
// namespace label are retained, and edges are returned once (directed match).
func (n *Neo4jRepository) SearchSubgraph(
	ctx context.Context,
	namespace types.NameSpace,
	nodes []string,
	maxLevel, maxNodes int,
) (*types.GraphData, error) {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil, nil
	}
	if maxLevel <= 0 {
		maxLevel = 1
	}
	if maxNodes <= 0 {
		maxNodes = types.GraphSubgraphMaxNodes
	}
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		labelExpr := n.Label(namespace)
		// Step 1: BFS expand from seed nodes; each row is one reachable node.
		expandQuery := `
			MATCH (start:` + labelExpr + `)
			WHERE ANY(nodeText IN $nodes WHERE start.name CONTAINS nodeText)
			WITH collect(start) AS starts
			CALL apoc.path.expandConfig(starts, {
				maxLevel: $maxLevel,
				limit: $limit,
				bfs: true
			}) YIELD path
			UNWIND nodes(path) AS m
			RETURN DISTINCT m.name AS name
		`
		expandResult, err := tx.Run(ctx, expandQuery, map[string]interface{}{
			"nodes":    nodes,
			"maxLevel": maxLevel,
			"limit":    maxNodes,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to expand subgraph: %v", err)
		}
		names := []string{}
		for expandResult.Next(ctx) {
			if name, ok := expandResult.Record().Values[0].(string); ok && name != "" {
				names = append(names, name)
			}
		}
		if err := expandResult.Err(); err != nil {
			return nil, err
		}
		if len(names) == 0 {
			return &types.GraphData{}, nil
		}

		// Step 2: fetch edges among retained nodes (directed => no dupes).
		edgeQuery := `
			MATCH (n:` + labelExpr + `)-[r]->(m:` + labelExpr + `)
			WHERE n.name IN $names AND m.name IN $names
			RETURN n, r, m
		`
		graphData, _, err := n.collectGraph(ctx, tx, edgeQuery, map[string]interface{}{"names": names})
		if err != nil {
			return nil, err
		}
		return graphData, nil
	})
	if err != nil {
		logger.Errorf(ctx, "search subgraph failed: %v", err)
		return nil, err
	}
	return result.(*types.GraphData), nil
}

// GetGraph exports the whole namespace graph for offline analytics
// (community detection). Nodes are capped at types.GraphExportMaxNodes with
// a deterministic order (name) so rebuilds are reproducible.
func (n *Neo4jRepository) GetGraph(
	ctx context.Context,
	namespace types.NameSpace,
) (*types.GraphData, error) {
	if n.driver == nil {
		logger.Warnf(ctx, "NOT SUPPORT RETRIEVE GRAPH")
		return nil, nil
	}
	session := n.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		labelExpr := n.Label(namespace)
		nodeQuery := `
			MATCH (n:` + labelExpr + `)
			RETURN n.name AS name
			ORDER BY n.name
			LIMIT $limit
		`
		nodeResult, err := tx.Run(ctx, nodeQuery, map[string]interface{}{"limit": types.GraphExportMaxNodes})
		if err != nil {
			return nil, fmt.Errorf("failed to list graph nodes: %v", err)
		}
		names := []string{}
		for nodeResult.Next(ctx) {
			if name, ok := nodeResult.Record().Values[0].(string); ok && name != "" {
				names = append(names, name)
			}
		}
		if err := nodeResult.Err(); err != nil {
			return nil, err
		}
		if len(names) == 0 {
			return &types.GraphData{}, nil
		}

		// Fetch nodes (for chunks/attributes) and directed edges among them.
		edgeQuery := `
			MATCH (n:` + labelExpr + `)
			WHERE n.name IN $names
			OPTIONAL MATCH (n)-[r]->(m:` + labelExpr + `)
			WHERE m.name IN $names
			RETURN n, r, m
		`
		graphData, _, err := n.collectGraph(ctx, tx, edgeQuery, map[string]interface{}{"names": names})
		if err != nil {
			return nil, err
		}
		return graphData, nil
	})
	if err != nil {
		logger.Errorf(ctx, "get graph failed: %v", err)
		return nil, err
	}
	return result.(*types.GraphData), nil
}
