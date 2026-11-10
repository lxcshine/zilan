package retriever

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// partialEngine 复用 factory_test.go 里的 fakeEngine 方法集，只重写 Retrieve：
// 按检索类型决定“成功返回结果”“返回结果 + 部分失败错误”还是“整腿失败”。
type partialEngine struct {
	*fakeEngine
	// byType 决定每种 RetrieverType 的应答。
	byType map[types.RetrieverType]struct {
		results []*types.RetrieveResult
		err     error
	}
}

func (p *partialEngine) Retrieve(_ context.Context, param types.RetrieveParams) ([]*types.RetrieveResult, error) {
	answer := p.byType[param.RetrieverType]
	return answer.results, answer.err
}

// newPartialComposite 用一个同时支持向量与关键词检索的引擎构造 Composite。
func newPartialComposite(t *testing.T, engine *partialEngine) *CompositeRetrieveEngine {
	t.Helper()
	const engineType = types.RetrieverEngineType("partial-test-engine")
	engine.fakeEngine = &fakeEngine{
		engineType: engineType,
		support:    []types.RetrieverType{types.VectorRetrieverType, types.KeywordsRetrieverType},
	}
	registry := &RetrieveEngineRegistry{
		byEngineType: map[types.RetrieverEngineType]interfaces.RetrieveEngineService{engineType: engine},
		byStoreID:    map[string]interfaces.RetrieveEngineService{},
	}
	composite, err := NewCompositeRetrieveEngine(registry, []types.RetrieverEngineParams{
		{RetrieverEngineType: engineType, RetrieverType: types.VectorRetrieverType},
		{RetrieverEngineType: engineType, RetrieverType: types.KeywordsRetrieverType},
	})
	if err != nil {
		t.Fatalf("NewCompositeRetrieveEngine: %v", err)
	}
	return composite
}

func resultSet(retrieverType types.RetrieverType, chunkIDs ...string) []*types.RetrieveResult {
	hits := make([]*types.IndexWithScore, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		hits = append(hits, &types.IndexWithScore{ChunkID: id})
	}
	return []*types.RetrieveResult{{
		Results:             hits,
		RetrieverEngineType: "partial-test-engine",
		RetrieverType:       retrieverType,
	}}
}

func bothLegParams() []types.RetrieveParams {
	return []types.RetrieveParams{
		{RetrieverType: types.VectorRetrieverType, Query: "q"},
		{RetrieverType: types.KeywordsRetrieverType, Query: "q"},
	}
}

// 一条腿整腿失败时，另一条腿已经取回的结果必须保留，同时错误照常上抛。
func TestCompositeRetrieve_KeepsSuccessfulLegAlongsideError(t *testing.T) {
	t.Parallel()
	boom := errors.New("keyword store exploded")
	engine := &partialEngine{byType: map[types.RetrieverType]struct {
		results []*types.RetrieveResult
		err     error
	}{
		types.VectorRetrieverType:   {results: resultSet(types.VectorRetrieverType, "v1", "v2")},
		types.KeywordsRetrieverType: {err: boom},
	}}
	composite := newPartialComposite(t, engine)

	results, err := composite.Retrieve(context.Background(), bothLegParams())
	if err == nil {
		t.Fatal("expected the failing leg to surface as an error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v", err, boom)
	}
	if len(results) != 1 || len(results[0].Results) != 2 {
		t.Fatalf("successful leg results were dropped: %#v", results)
	}
}

// 底层引擎把局部失败写在 RetrieveResult.Error 上时，Composite 必须消费它：
// 结果保留 + 返回“部分失败”错误，而不是像以前那样静默丢弃。
func TestCompositeRetrieve_SurfacesResultLevelPartialFailure(t *testing.T) {
	t.Parallel()
	cause := errors.New("1 of 3 matched collections failed")
	partial := resultSet(types.VectorRetrieverType, "c1")
	partial[0].Error = cause
	engine := &partialEngine{byType: map[types.RetrieverType]struct {
		results []*types.RetrieveResult
		err     error
	}{
		types.VectorRetrieverType:   {results: partial},
		types.KeywordsRetrieverType: {results: resultSet(types.KeywordsRetrieverType, "k1")},
	}}
	composite := newPartialComposite(t, engine)

	results, err := composite.Retrieve(context.Background(), bothLegParams())
	if err == nil {
		t.Fatal("expected a partial-failure error alongside the results")
	}
	if !strings.Contains(err.Error(), "partial results") {
		t.Fatalf("error = %v, want it to mention partial results", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want it to wrap the underlying cause", err)
	}
	if len(results) != 2 {
		t.Fatalf("result sets = %d, want 2 (hits must be kept)", len(results))
	}
}

// 全部腿都成功时不得凭空报错。
func TestCompositeRetrieve_NoErrorWhenAllLegsSucceed(t *testing.T) {
	t.Parallel()
	engine := &partialEngine{byType: map[types.RetrieverType]struct {
		results []*types.RetrieveResult
		err     error
	}{
		types.VectorRetrieverType:   {results: resultSet(types.VectorRetrieverType, "v1")},
		types.KeywordsRetrieverType: {results: resultSet(types.KeywordsRetrieverType, "k1")},
	}}
	composite := newPartialComposite(t, engine)

	results, err := composite.Retrieve(context.Background(), bothLegParams())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("result sets = %d, want 2", len(results))
	}
}
