package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// alwaysFullEngine 每轮都恰好返回 TopK 条命中，让“取回数量 < TopK”这个提前
// 退出条件永远不成立，从而把迭代次数完全交给 FAQExpandCount 决定。
type alwaysFullEngine struct {
	*fakeRetrieveEngineService
	calls atomic.Int64
}

func (e *alwaysFullEngine) Retrieve(_ context.Context, p types.RetrieveParams) ([]*types.RetrieveResult, error) {
	e.calls.Add(1)
	hits := make([]*types.IndexWithScore, 0, p.TopK)
	for i := 0; i < p.TopK; i++ {
		hits = append(hits, &types.IndexWithScore{ChunkID: fmt.Sprintf("c%d", i), Score: 0.5})
	}
	return []*types.RetrieveResult{{Results: hits, RetrieverType: p.RetrieverType}}, nil
}

// negativeChunkRepo 把每个分块都标成“命中负向问题”的 FAQ 条目，
// 于是有效结果始终为 0，迭代只能靠轮数上限收尾。
type negativeChunkRepo struct {
	interfaces.ChunkRepository
	negative string
}

func (r *negativeChunkRepo) ListChunksByID(_ context.Context, _ uint64, ids []string) ([]*types.Chunk, error) {
	out := make([]*types.Chunk, 0, len(ids))
	for _, id := range ids {
		chunk := &types.Chunk{ID: id, ChunkType: types.ChunkTypeFAQ}
		if err := chunk.SetFAQMetadata(&types.FAQChunkMetadata{
			StandardQuestion:  "标准问题",
			NegativeQuestions: []string{r.negative},
		}); err != nil {
			return nil, err
		}
		out = append(out, chunk)
	}
	return out, nil
}

func faqIterationCtx(tenantCfg *types.RetrievalConfig) context.Context {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
	return context.WithValue(ctx, types.TenantInfoContextKey,
		&types.Tenant{RetrievalConfig: tenantCfg})
}

// FR-19②：FAQ 列表展开轮数必须来自工作区级检索配置，未配置时回落到默认值。
func TestIterativeRetrieveWithDeduplication_HonoursFAQExpandCount(t *testing.T) {
	t.Parallel()
	const query = "不要匹配这个问题"
	cases := []struct {
		name       string
		cfg        *types.RetrievalConfig
		wantRounds int
	}{
		{"未配置时用默认值", nil, types.DefaultFAQExpandCount},
		{"配置值生效", &types.RetrievalConfig{FAQExpandCount: 2}, 2},
		{"超过上限被钳制", &types.RetrievalConfig{FAQExpandCount: 999}, types.MaxFAQExpandCount},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := &alwaysFullEngine{fakeRetrieveEngineService: &fakeRetrieveEngineService{
				engineType: types.MilvusRetrieverEngineType,
				support:    []types.RetrieverType{types.VectorRetrieverType},
			}}
			group := &storeGroup{
				StoreID:       "store-x",
				OwnerTenantID: 1,
				KBIDs:         []string{"kb-faq"},
				Engine:        buildBoundComposite(t, engine),
				BaseParams:    vectorParams(query),
				TopK:          3,
			}
			svc := &knowledgeBaseService{chunkRepo: &negativeChunkRepo{negative: query}}

			results, err := svc.iterativeRetrieveWithDeduplication(
				faqIterationCtx(tc.cfg), []*storeGroup{group}, 1, query)
			if err != nil {
				t.Fatalf("iterativeRetrieveWithDeduplication() = %v, want nil", err)
			}
			if len(results) != 0 {
				t.Fatalf("results = %d, want 0 (every chunk is negatively matched)", len(results))
			}
			if got := engine.calls.Load(); got != int64(tc.wantRounds) {
				t.Fatalf("retrieve rounds = %d, want %d", got, tc.wantRounds)
			}
		})
	}
}
