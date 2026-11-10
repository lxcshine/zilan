package service

import (
	"context"
	"errors"
	"testing"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

// fakeGraphSearchRepo 模拟 Neo4j 侧的有界图谱查询。
type fakeGraphSearchRepo struct {
	candidates *types.GraphSearchCandidates
	err        error
	gotQuery   string
	gotNS      types.NameSpace
}

func (f *fakeGraphSearchRepo) SearchGraph(_ context.Context, ns types.NameSpace, query string) (*types.GraphSearchCandidates, error) {
	f.gotNS = ns
	f.gotQuery = query
	return f.candidates, f.err
}

// fakeGraphEvidenceRepo 模拟关系型库侧的来源分块校验。
type fakeGraphEvidenceRepo struct {
	rows      []types.GraphSearchSourceChunk
	err       error
	gotIDs    []string
	gotTenant uint64
	gotKB     string
}

func (f *fakeGraphEvidenceRepo) GetGraphSourceChunks(_ context.Context, tenantID uint64, kbID string, ids []string) ([]types.GraphSearchSourceChunk, error) {
	f.gotTenant, f.gotKB, f.gotIDs = tenantID, kbID, ids
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func graphEnabledKB() *types.KnowledgeBase {
	return &types.KnowledgeBase{
		ID:               "kb-1",
		TenantID:         7,
		IndexingStrategy: types.IndexingStrategy{GraphEnabled: true},
		ExtractConfig:    &types.ExtractConfig{Enabled: true},
	}
}

func graphNode(id, name, knowledgeID string, chunks ...string) *types.GraphNode {
	return &types.GraphNode{ID: id, Name: name, KnowledgeID: knowledgeID, Chunks: chunks}
}

func evidenceRow(id, knowledgeID string, tenantID uint64, kbID string) types.GraphSearchSourceChunk {
	return types.GraphSearchSourceChunk{
		ID: id, KnowledgeID: knowledgeID, TenantID: tenantID, KnowledgeBaseID: kbID,
		Title: "文档标题", Content: "正文内容",
	}
}

func httpCode(t *testing.T, err error) int {
	t.Helper()
	appErr, ok := apperrors.IsAppError(err)
	if !ok {
		t.Fatalf("error %v is not an AppError", err)
	}
	return appErr.HTTPCode
}

// 图谱后端未部署时必须给出明确的 503，而不是一个看起来正常的空结果。
func TestGraphSearch_BackendNotConfigured(t *testing.T) {
	t.Parallel()
	svc := NewGraphSearchService(&fakeGraphSearchRepo{}, &fakeGraphEvidenceRepo{})

	_, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: "张三"})
	if code := httpCode(t, err); code != 503 {
		t.Fatalf("HTTP code = %d, want 503 (nil candidates means no backend)", code)
	}

	// 依赖缺失同样报 503。
	svc = NewGraphSearchService(nil, nil)
	if _, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: "张三"}); httpCode(t, err) != 503 {
		t.Fatalf("nil dependencies must also map to 503, got %v", err)
	}
}

// 图谱查询失败 → 503；来源校验失败 → 500；两者都不能伪装成空结果。
func TestGraphSearch_BackendAndEvidenceFailures(t *testing.T) {
	t.Parallel()
	boom := errors.New("bolt connection refused")
	svc := NewGraphSearchService(
		&fakeGraphSearchRepo{err: boom}, &fakeGraphEvidenceRepo{})
	if _, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: "张三"}); httpCode(t, err) != 503 {
		t.Fatalf("graph failure must map to 503, got %v", err)
	}

	candidates := &types.GraphSearchCandidates{Graph: &types.GraphData{
		Node: []*types.GraphNode{graphNode("4:1", "张三", "doc-1", "c1")},
	}}
	svc = NewGraphSearchService(&fakeGraphSearchRepo{candidates: candidates},
		&fakeGraphEvidenceRepo{err: boom})
	if _, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: "张三"}); httpCode(t, err) != 500 {
		t.Fatalf("evidence failure must map to 500, got %v", err)
	}
}

// 未开启图谱的知识库 → 409；参数非法 → 400；知识库为 nil → 404。
func TestGraphSearch_Preconditions(t *testing.T) {
	t.Parallel()
	svc := NewGraphSearchService(&fakeGraphSearchRepo{}, &fakeGraphEvidenceRepo{})

	noGraph := graphEnabledKB()
	noGraph.IndexingStrategy.GraphEnabled = false
	if _, err := svc.Search(context.Background(), noGraph, types.GraphSearchRequest{Query: "张三"}); httpCode(t, err) != 409 {
		t.Fatalf("graph disabled must map to 409, got %v", err)
	}

	if _, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: "  "}); httpCode(t, err) != 400 {
		t.Fatalf("blank query must map to 400, got %v", err)
	}
	if _, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: "q", MaxNodes: 999}); httpCode(t, err) != 400 {
		t.Fatalf("over-cap max_nodes must map to 400, got %v", err)
	}
	if _, err := svc.Search(context.Background(), nil, types.GraphSearchRequest{Query: "q"}); httpCode(t, err) != 404 {
		t.Fatalf("nil knowledge base must map to 404, got %v", err)
	}
}

// 正常路径：实体、同文档定向关系与来源分块一起返回，公开 ID 不泄露 Neo4j
// 的 elementId，来源查询被限定在该租户与该知识库。
func TestGraphSearch_HappyPath(t *testing.T) {
	t.Parallel()
	candidates := &types.GraphSearchCandidates{Graph: &types.GraphData{
		Node: []*types.GraphNode{
			graphNode("4:seed:1", "张三", "doc-1", "c1"),
			graphNode("4:seed:2", "李四", "doc-1", "c2"),
		},
		Relation: []*types.GraphRelation{{
			ID: "5:rel:1", SourceID: "4:seed:1", TargetID: "4:seed:2", Type: "同事",
		}},
	}}
	evidence := &fakeGraphEvidenceRepo{rows: []types.GraphSearchSourceChunk{
		evidenceRow("c1", "doc-1", 7, "kb-1"),
		evidenceRow("c2", "doc-1", 7, "kb-1"),
	}}
	svc := NewGraphSearchService(&fakeGraphSearchRepo{candidates: candidates}, evidence)

	result, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: " 张"})
	if err != nil {
		t.Fatalf("Search() = %v, want nil", err)
	}
	if len(result.Entities) != 2 || len(result.Relationships) != 1 || len(result.SourceChunks) != 2 {
		t.Fatalf("unexpected shape: %+v", result)
	}
	if result.Meta.Truncated {
		t.Fatalf("nothing was clipped, meta must not claim truncation: %+v", result.Meta)
	}
	if result.Meta.MaxNodes != 50 || result.Meta.MaxRelations != 100 || result.Meta.MaxSourceChunks != 20 {
		t.Fatalf("effective limits not reported: %+v", result.Meta)
	}
	for _, e := range result.Entities {
		if e.ID == "4:seed:1" || e.ID == "4:seed:2" || len(e.ID) != 64 {
			t.Fatalf("public entity ID leaks or is not a hash: %q", e.ID)
		}
	}
	rel := result.Relationships[0]
	if rel.SourceID != result.Entities[0].ID || rel.TargetID != result.Entities[1].ID {
		t.Fatalf("relation endpoints must use the public entity IDs: %+v", rel)
	}
	if rel.Type != "同事" {
		t.Fatalf("relation type = %q, want 同事", rel.Type)
	}
	// 来源查询必须被限定在本租户本库。
	if evidence.gotTenant != 7 || evidence.gotKB != "kb-1" {
		t.Fatalf("evidence scope = (%d, %q), want (7, kb-1)", evidence.gotTenant, evidence.gotKB)
	}
	if result.KnowledgeBaseID != "kb-1" || result.Query != "张" {
		t.Fatalf("echo fields wrong: %+v", result)
	}
}

// 上限裁剪必须如实上报原因，而不是编造一个总数。
func TestGraphSearch_CapsAreReportedNotInvented(t *testing.T) {
	t.Parallel()
	candidates := &types.GraphSearchCandidates{
		Graph: &types.GraphData{Node: []*types.GraphNode{
			graphNode("4:1", "张三", "doc-1", "c1"),
			graphNode("4:2", "李四", "doc-1", "c2"),
			graphNode("4:3", "王五", "doc-1", "c3"),
		}},
		// 图谱侧自己也被候选预算裁过。
		TruncationReasons: []string{"candidate_seeds"},
	}
	evidence := &fakeGraphEvidenceRepo{rows: []types.GraphSearchSourceChunk{
		evidenceRow("c1", "doc-1", 7, "kb-1"),
		evidenceRow("c2", "doc-1", 7, "kb-1"),
		evidenceRow("c3", "doc-1", 7, "kb-1"),
	}}
	svc := NewGraphSearchService(&fakeGraphSearchRepo{candidates: candidates}, evidence)

	result, err := svc.Search(context.Background(), graphEnabledKB(),
		types.GraphSearchRequest{Query: "张", MaxNodes: 2, MaxSourceChunks: 2})
	if err != nil {
		t.Fatalf("Search() = %v, want nil", err)
	}
	if len(result.Entities) != 2 {
		t.Fatalf("entities = %d, want the requested cap 2", len(result.Entities))
	}
	if !result.Meta.Truncated {
		t.Fatal("meta must report truncation")
	}
	for _, want := range []string{"candidate_seeds", "max_nodes"} {
		found := false
		for _, got := range result.Meta.TruncationReasons {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("truncation reason %q missing from %v", want, result.Meta.TruncationReasons)
		}
	}
}

// 保守溯源：引用的分块只要有一个已经失效（被删/被禁用/不属于同一篇文档），
// 整个实体就不返回；跨文档的关系也不返回。
func TestGraphSearch_DropsUnverifiableProvenance(t *testing.T) {
	t.Parallel()
	candidates := &types.GraphSearchCandidates{Graph: &types.GraphData{
		Node: []*types.GraphNode{
			graphNode("4:1", "张三", "doc-1", "c1"),
			// c-missing 已经不在库里 → 整个实体丢弃
			graphNode("4:2", "李四", "doc-1", "c-missing"),
			// 属于另一篇文档 → 关系两端不同文档，关系丢弃
			graphNode("4:3", "王五", "doc-2", "c3"),
		},
		Relation: []*types.GraphRelation{
			{ID: "5:1", SourceID: "4:1", TargetID: "4:2", Type: "同事"},
			{ID: "5:2", SourceID: "4:1", TargetID: "4:3", Type: "跨文档"},
		},
	}}
	evidence := &fakeGraphEvidenceRepo{rows: []types.GraphSearchSourceChunk{
		evidenceRow("c1", "doc-1", 7, "kb-1"),
		evidenceRow("c3", "doc-2", 7, "kb-1"),
	}}
	svc := NewGraphSearchService(&fakeGraphSearchRepo{candidates: candidates}, evidence)

	result, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: "张"})
	if err != nil {
		t.Fatalf("Search() = %v, want nil", err)
	}
	if len(result.Entities) != 2 {
		t.Fatalf("entities = %d, want 2 (the unverifiable one is dropped)", len(result.Entities))
	}
	for _, e := range result.Entities {
		if e.Name == "李四" {
			t.Fatal("an entity citing a dead chunk must not be returned")
		}
	}
	if len(result.Relationships) != 0 {
		t.Fatalf("relationships = %+v, want none (one endpoint dropped, the other crosses documents)", result.Relationships)
	}
}

// 内容与标题被 SQL 截断时必须如实上报 source_fields。
func TestGraphSearch_ReportsFieldTruncation(t *testing.T) {
	t.Parallel()
	candidates := &types.GraphSearchCandidates{Graph: &types.GraphData{
		Node: []*types.GraphNode{graphNode("4:1", "张三", "doc-1", "c1")},
	}}
	row := evidenceRow("c1", "doc-1", 7, "kb-1")
	row.ContentTruncated = true
	svc := NewGraphSearchService(&fakeGraphSearchRepo{candidates: candidates},
		&fakeGraphEvidenceRepo{rows: []types.GraphSearchSourceChunk{row}})

	result, err := svc.Search(context.Background(), graphEnabledKB(), types.GraphSearchRequest{Query: "张"})
	if err != nil {
		t.Fatalf("Search() = %v, want nil", err)
	}
	if !result.Meta.Truncated || len(result.Meta.TruncationReasons) != 1 ||
		result.Meta.TruncationReasons[0] != "source_fields" {
		t.Fatalf("truncation not reported faithfully: %+v", result.Meta)
	}
	if !result.SourceChunks[0].ContentTruncated {
		t.Fatal("the row-level truncation flag must survive")
	}
}
