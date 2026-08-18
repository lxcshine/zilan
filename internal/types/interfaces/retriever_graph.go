package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// RetrieveGraphRepository is a repository for retrieving graphs
type RetrieveGraphRepository interface {
	// AddGraph adds a graph to the repository
	AddGraph(ctx context.Context, namespace types.NameSpace, graphs []*types.GraphData) error
	// DelGraph deletes a graph from the repository
	DelGraph(ctx context.Context, namespace []types.NameSpace) error
	// SearchSubgraph returns the bounded multi-hop ego-graph around the given
	// entity names (GraphRAG "local search"). maxLevel is the expansion depth,
	// maxNodes caps the returned node set. Implementations must be no-op (nil,
	// nil) when the graph backend is disabled.
	SearchSubgraph(ctx context.Context, namespace types.NameSpace, nodes []string,
		maxLevel, maxNodes int) (*types.GraphData, error)
	// GetGraph exports the whole namespace graph (nodes + relations) for
	// offline analytics such as community detection. Implementations may
	// truncate to a deterministic node cap.
	GetGraph(ctx context.Context, namespace types.NameSpace) (*types.GraphData, error)
}
