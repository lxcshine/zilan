package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDimEmbedder 只服务于维度校验测试。
type fakeDimEmbedder struct {
	name string
	dims int
	embedding.Embedder
}

func (f *fakeDimEmbedder) GetModelName() string { return f.name }
func (f *fakeDimEmbedder) GetDimensions() int   { return f.dims }

func buildModelWithDimension(id, name, baseURL string, dimension int) *types.Model {
	m := buildModel(id, name, baseURL)
	m.Parameters.EmbeddingParameters.Dimension = dimension
	return m
}

// FR-19⑤：模型身份 key 必须包含向量维度，否则同名同 endpoint 但不同宽度的
// 两个模型会共用一份预计算 query embedding，跨维度混检。
func TestResolveEmbeddingModelKeys_DimensionIsPartOfTheKey(t *testing.T) {
	t.Parallel()
	models := map[string]*types.Model{
		"m-1536": buildModelWithDimension("m-1536", "text-embedding-3", "http://embed-a", 1536),
		"m-512":  buildModelWithDimension("m-512", "text-embedding-3", "http://embed-a", 512),
		"m-512b": buildModelWithDimension("m-512b", "text-embedding-3", "http://embed-a", 512),
	}
	svc := newKBSvcForValidate(&fakeModelSvcForKeys{byID: models})

	keys := svc.ResolveEmbeddingModelKeys(context.Background(), []*types.KnowledgeBase{
		{ID: "kb-a", EmbeddingModelID: "m-1536", TenantID: 1},
		{ID: "kb-b", EmbeddingModelID: "m-512", TenantID: 1},
		{ID: "kb-c", EmbeddingModelID: "m-512b", TenantID: 2},
	})

	if keys["kb-a"] == keys["kb-b"] {
		t.Fatalf("different dimensions must not share a key: %q", keys["kb-a"])
	}
	if keys["kb-b"] != keys["kb-c"] {
		t.Fatalf("same name/endpoint/dimension must share a key: %q vs %q", keys["kb-b"], keys["kb-c"])
	}
}

// 维度不同 ⇒ 多库检索必须被拒绝，且错误文案要说清是维度问题。
func TestValidateSameEmbeddingModel_DifferentDimension_400(t *testing.T) {
	t.Parallel()
	models := map[string]*types.Model{
		"m-1": buildModelWithDimension("m-1", "text-embedding-3", "http://embed-a", 1536),
		"m-2": buildModelWithDimension("m-2", "text-embedding-3", "http://embed-a", 512),
	}
	svc := newKBSvcForValidate(&fakeModelSvcForKeys{byID: models})

	err := svc.validateSameEmbeddingModel(context.Background(), []*types.KnowledgeBase{
		{ID: "kb-1", EmbeddingModelID: "m-1", TenantID: 1},
		{ID: "kb-2", EmbeddingModelID: "m-2", TenantID: 1},
	})
	app, ok := apperrors.IsAppError(err)
	require.True(t, ok, "expected AppError, got %T", err)
	assert.Equal(t, apperrors.ErrBadRequest, app.Code)
	assert.Contains(t, app.Message, "vector dimension")
}

// 维度校验必须放行两类历史情况：模型未声明维度、以及空向量；
// 只有“两边都有值且不一致”才是错误，并且要报 2201 而不是 400。
func TestValidateQueryEmbeddingDimension(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		model   embedding.Embedder
		actual  int
		wantErr bool
	}{
		{"nil model passes", nil, 1536, false},
		{"empty vector passes", &fakeDimEmbedder{name: "m", dims: 1536}, 0, false},
		{"undeclared dimension passes", &fakeDimEmbedder{name: "m", dims: 0}, 768, false},
		{"matching dimension passes", &fakeDimEmbedder{name: "m", dims: 1536}, 1536, false},
		{"mismatched dimension fails", &fakeDimEmbedder{name: "m", dims: 1536}, 512, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateQueryEmbeddingDimension(tc.model, tc.actual)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			app, ok := apperrors.IsAppError(err)
			require.True(t, ok, "expected AppError, got %T", err)
			assert.Equal(t, apperrors.ErrVectorStoreUnavailable, app.Code)
			details, ok := app.Details.(map[string]any)
			require.True(t, ok, "expected structured details, got %T", app.Details)
			assert.Equal(t, "m", details["model"])
			assert.Equal(t, 1536, details["expected_dimension"])
			assert.Equal(t, 512, details["actual_dimension"])
			assert.NotEmpty(t, details["hint"])
		})
	}
}

// FR-19⑤：部分失败的结果必须保留，失败在 WARN 级别留痕后不再向上抛；
// 一条结果都没有时失败仍然是致命的。
func TestRetainPartialResults(t *testing.T) {
	t.Parallel()
	group := &storeGroup{StoreID: "", OwnerTenantID: 1, KBIDs: []string{"kb-1", "kb-2"}}
	boom := errors.New("1 of 2 matched collections failed")
	sets := []*types.RetrieveResult{{
		Results:       []*types.IndexWithScore{{ChunkID: "c1"}},
		RetrieverType: types.VectorRetrieverType,
		Error:         boom,
	}}

	kept, err := retainPartialResults(context.Background(), group, sets, boom)
	assert.NoError(t, err, "a partial failure must not fail the whole search")
	require.Len(t, kept, 1)
	assert.Equal(t, "c1", kept[0].Results[0].ChunkID)

	// 没有任何结果时，错误必须原样上抛。
	_, err = retainPartialResults(context.Background(), group, nil, boom)
	assert.ErrorIs(t, err, boom)

	// 没有错误时原样返回。
	same, err := retainPartialResults(context.Background(), group, sets, nil)
	assert.NoError(t, err)
	assert.Len(t, same, 1)
}

// 单组快速路径也必须走同一套部分失败策略。
func TestRetrieveGroupKeepsPartialResults(t *testing.T) {
	t.Parallel()
	cause := errors.New("milvus keyword search failed in 1 of 2 collections")
	fake := &fakeRetrieveEngineService{
		engineType:      types.MilvusRetrieverEngineType,
		support:         []types.RetrieverType{types.VectorRetrieverType},
		canned:          []*types.IndexWithScore{{ChunkID: "c1"}},
		cannedResultErr: cause,
	}
	group := &storeGroup{
		StoreID:    "store-x",
		KBIDs:      []string{"kb-1"},
		Engine:     buildBoundComposite(t, fake),
		BaseParams: vectorParams("q"),
		TopK:       50,
	}

	res, err := retrieveGroup(context.Background(), group)
	require.NoError(t, err, "partial failure must not fail the search")
	require.Len(t, res, 1)
	assert.Equal(t, "c1", res[0].Results[0].ChunkID)
	assert.ErrorIs(t, res[0].Error, cause, "the result set must still carry its partial failure")

	// 整腿失败（没有任何结果）仍然是致命错误。
	fatal := &fakeRetrieveEngineService{
		engineType: types.MilvusRetrieverEngineType,
		support:    []types.RetrieverType{types.VectorRetrieverType},
		cannedErr:  cause,
	}
	group.Engine = buildBoundComposite(t, fatal)
	_, err = (&knowledgeBaseService{}).retrieveFromStores(
		context.Background(), []*storeGroup{group}, retriever.EngineAwareNormalizer{})
	assert.ErrorIs(t, err, cause)
}
