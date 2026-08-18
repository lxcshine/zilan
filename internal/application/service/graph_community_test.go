package service

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func communityGraphForTest() *types.GraphData {
	names := []string{"a", "b", "c", "x", "y", "z", "lonely"}
	nodes := make([]*types.GraphNode, 0, len(names))
	for _, n := range names {
		nodes = append(nodes, &types.GraphNode{Name: n})
	}
	return &types.GraphData{
		Node: nodes,
		Relation: []*types.GraphRelation{
			// dense cluster {a,b,c}
			{Node1: "a", Node2: "b", Type: "knows"},
			{Node1: "b", Node2: "c", Type: "knows"},
			{Node1: "a", Node2: "c", Type: "knows"},
			// dense cluster {x,y,z}
			{Node1: "x", Node2: "y", Type: "works_on"},
			{Node1: "y", Node2: "z", Type: "works_on"},
			{Node1: "x", Node2: "z", Type: "works_on"},
			// dangling relation referencing an unknown node must be ignored
			{Node1: "a", Node2: "ghost", Type: "mentions"},
			// self loop must be ignored
			{Node1: "a", Node2: "a", Type: "self"},
		},
	}
}

func sortedSets(communities [][]string) [][]string {
	out := make([][]string, 0, len(communities))
	for _, members := range communities {
		cp := append([]string(nil), members...)
		sort.Strings(cp)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i][0] < out[j][0]
	})
	return out
}

func TestDetectCommunitiesClustersAndIsolated(t *testing.T) {
	communities := detectCommunities(communityGraphForTest())
	require.Equal(t, [][]string{
		{"a", "b", "c"},
		{"x", "y", "z"},
		{"lonely"},
	}, sortedSets(communities))
}

func TestDetectCommunitiesDeterministic(t *testing.T) {
	first := detectCommunities(communityGraphForTest())
	for i := 0; i < 10; i++ {
		require.Equal(t, first, detectCommunities(communityGraphForTest()))
	}
}

func TestDetectCommunitiesEmptyAndBlankNames(t *testing.T) {
	require.Nil(t, detectCommunities(&types.GraphData{}))
	got := detectCommunities(&types.GraphData{
		Node: []*types.GraphNode{{Name: ""}, {Name: "n1"}, {Name: "n1"}},
	})
	require.Equal(t, [][]string{{"n1"}}, got)
}

func TestParseCommunitySummary(t *testing.T) {
	title, summary := parseCommunitySummary("Title: 项目 X 团队\nSummary: 这些实体描述了项目 X 的核心成员。")
	require.Equal(t, "项目 X 团队", title)
	require.Equal(t, "这些实体描述了项目 X 的核心成员。", summary)

	// case-insensitive prefixes + multiline summary continuation
	title, summary = parseCommunitySummary("TITLE: Alpha\nsummary: first line\nsecond line")
	require.Equal(t, "Alpha", title)
	require.Equal(t, "first line second line", summary)

	// missing title is tolerated
	title, summary = parseCommunitySummary("Summary: only a summary")
	require.Empty(t, title)
	require.Equal(t, "only a summary", summary)

	// garbage yields nothing
	title, summary = parseCommunitySummary("no structured output here")
	require.Empty(t, title)
	require.Empty(t, summary)
}

func TestBuildRelationIndexAndCountInternal(t *testing.T) {
	graph := communityGraphForTest()
	index := buildRelationIndex(graph)
	// both endpoints index the same rendering
	require.Contains(t, index["a"], "a -[knows]-> b")
	require.Contains(t, index["b"], "a -[knows]-> b")

	// community {a,b,c}: 3 internal edges; the dangling ghost edge and the
	// self loop involve "a" but must not be counted as internal relations.
	require.Equal(t, 3, countInternalRelations([]string{"a", "b", "c"}, graph))
	require.Equal(t, 0, countInternalRelations([]string{"lonely"}, graph))
}

type fakeCommunityRepo struct {
	rows map[string][]*types.GraphCommunity
	err  error
}

func (f *fakeCommunityRepo) UpsertCommunities(ctx context.Context, rows []*types.GraphCommunity) error {
	return nil
}

func (f *fakeCommunityRepo) ListCommunities(
	ctx context.Context, tenantID uint64, kbID string,
) ([]*types.GraphCommunity, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[kbID], nil
}

func (f *fakeCommunityRepo) DeleteCommunitiesNotIn(
	ctx context.Context, tenantID uint64, kbID string, keepKeys []string,
) error {
	return nil
}

func (f *fakeCommunityRepo) DeleteByKnowledgeBase(ctx context.Context, tenantID uint64, kbID string) error {
	return nil
}

func TestGraphCommunityRecallScoringAndTopK(t *testing.T) {
	repo := &fakeCommunityRepo{rows: map[string][]*types.GraphCommunity{
		"kb-1": {
			{ID: "c-high", Embedding: types.VectorBlob{1, 0}},
			{ID: "c-mid", Embedding: types.VectorBlob{0.9, 0.1}},
			{ID: "c-low", Embedding: types.VectorBlob{0, 1}},
			{ID: "c-noemb"},
		},
		"kb-2": {
			{ID: "c-other-kb", Embedding: types.VectorBlob{0.95, 0.05}},
		},
	}}
	svc := &graphCommunityService{communityRepo: repo}

	hits, err := svc.Recall(context.Background(), 1,
		[]string{"kb-1", "kb-2"}, []float32{1, 0}, 10, 0.5)
	require.NoError(t, err)
	// sorted by similarity desc across KBs; c-low below threshold, c-noemb skipped
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	require.Equal(t, []string{"c-high", "c-other-kb", "c-mid"}, ids)

	// topK truncates the best-scoring tail
	hits, err = svc.Recall(context.Background(), 1, []string{"kb-1"}, []float32{1, 0}, 2, 0.5)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	require.Equal(t, "c-high", hits[0].ID)

	// defaults kick in for non-positive topK/threshold: default threshold 0.25
	// admits c-mid but not the orthogonal c-low.
	hits, err = svc.Recall(context.Background(), 1, []string{"kb-1"}, []float32{1, 0}, 0, 0)
	require.NoError(t, err)
	require.Len(t, hits, 2)

	// empty query vector or KB set short-circuits
	hits, err = svc.Recall(context.Background(), 1, nil, []float32{1, 0}, 3, 0.5)
	require.NoError(t, err)
	require.Empty(t, hits)
	hits, err = svc.Recall(context.Background(), 1, []string{"kb-1"}, nil, 3, 0.5)
	require.NoError(t, err)
	require.Empty(t, hits)
}

func TestGraphCommunityRecallRepoErrorIsSoft(t *testing.T) {
	svc := &graphCommunityService{communityRepo: &fakeCommunityRepo{err: errors.New("db down")}}
	hits, err := svc.Recall(context.Background(), 1, []string{"kb-1"}, []float32{1, 0}, 3, 0.5)
	require.NoError(t, err, "per-KB recall failure must degrade, not fail the query")
	require.Empty(t, hits)
}
