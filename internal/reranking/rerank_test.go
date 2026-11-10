package reranking

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
)

// 守护测试（FR-19③）：重排阶段绝不改写入参行。原始检索分、原始 metadata 与
// 原始 Content 都必须原封不动，重排结果只能是带综合分的副本。
func TestScoredCopyNeverMutatesInputRow(t *testing.T) {
	t.Parallel()
	original := &types.SearchResult{
		ID:       "chunk-1",
		Content:  "原始正文，必须保持不变",
		Score:    0.42,
		Metadata: map[string]string{"fusion_score": "0.0123"},
		StartAt:  0,
		EndAt:    100,
	}
	passage := "原始正文，必须保持不变\n\n图片描述"

	got := ScoredCopy(original, 0.9, 1.0, passage)

	if got == original {
		t.Fatal("ScoredCopy must return a copy, not the input pointer")
	}
	if original.Score != 0.42 {
		t.Fatalf("input Score was mutated: %v", original.Score)
	}
	if original.Content != "原始正文，必须保持不变" {
		t.Fatalf("input Content was mutated: %q", original.Content)
	}
	if original.RerankPassage != "" {
		t.Fatalf("input RerankPassage was mutated: %q", original.RerankPassage)
	}
	if _, leaked := original.Metadata["base_score"]; leaked {
		t.Fatal("input Metadata was mutated: base_score leaked into the original row")
	}
	if original.Metadata["fusion_score"] != "0.0123" {
		t.Fatalf("input Metadata lost its original entry: %#v", original.Metadata)
	}

	if got.Metadata["base_score"] != "0.4200" {
		t.Fatalf("copy base_score = %q, want 0.4200", got.Metadata["base_score"])
	}
	if got.Metadata["model_score"] != "0.9000" {
		t.Fatalf("copy model_score = %q, want 0.9000", got.Metadata["model_score"])
	}
	if got.RerankPassage != passage {
		t.Fatalf("copy RerankPassage = %q, want the passage the model saw", got.RerankPassage)
	}
	if got.Score <= original.Score {
		t.Fatalf("composite score %v should exceed the raw retrieval score %v", got.Score, original.Score)
	}
	if got.Score < 0 || got.Score > 1 {
		t.Fatalf("composite score %v out of [0,1]", got.Score)
	}

	// 副本的 metadata 必须是独立实例，否则下游写副本等于写原始行。
	got.Metadata["downstream"] = "written-later"
	if _, leaked := original.Metadata["downstream"]; leaked {
		t.Fatal("copy shares the metadata map with the input row")
	}
}

// FAQ 加成只作用在副本上，并且封顶在 1.0。
func TestScoredCopyFAQBoostIsCappedAndRecorded(t *testing.T) {
	t.Parallel()
	// StartAt = -1 关掉位置先验，让断言只针对 FAQ 加成本身。
	original := &types.SearchResult{
		ID: "faq-1", Score: 0.2, ChunkType: string(types.ChunkTypeFAQ), StartAt: -1,
	}

	got := ScoredCopy(original, 0.5, 1.5, "passage")

	if got.Metadata["faq_boosted"] != "true" {
		t.Fatalf("faq_boosted metadata missing: %#v", got.Metadata)
	}
	if PreBoostScore(got) >= got.Score {
		t.Fatalf("PreBoostScore %v should stay below the boosted Score %v", PreBoostScore(got), got.Score)
	}
	if original.Score != 0.2 || original.Metadata != nil {
		t.Fatalf("input FAQ row was mutated: %#v", original)
	}

	// 加成后仍必须封顶在 1.0。
	capped := ScoredCopy(&types.SearchResult{
		ID: "faq-2", Score: 0.9, ChunkType: string(types.ChunkTypeFAQ), StartAt: -1,
	}, 1.0, 2.0, "passage")
	if capped.Score > 1.0 {
		t.Fatalf("boosted score %v must be capped at 1.0", capped.Score)
	}
}

// 图谱命中没有检索分，综合分用模型分代替，不会因为 base=0 被系统性压低。
func TestCompositeScoreGraphHitsUseModelScore(t *testing.T) {
	t.Parallel()
	graph := &types.SearchResult{ID: "g1", MatchType: types.MatchTypeGraph, StartAt: -1}
	plain := &types.SearchResult{ID: "p1", StartAt: -1}

	graphScore := CompositeScore(graph, 0.8, 0)
	plainScore := CompositeScore(plain, 0.8, 0)
	if math.Abs(graphScore-plainScore) < 1e-9 {
		t.Fatalf("graph hit should not be penalised for having no retrieval score: %v vs %v", graphScore, plainScore)
	}
	if graphScore < 0 || graphScore > 1 {
		t.Fatalf("composite score %v out of [0,1]", graphScore)
	}
}

// BuildCandidates 跳过空 passage，并把候选数裁到上限（保留检索分最高的那批）。
func TestBuildCandidatesSkipsEmptyAndRespectsCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	results := []*types.SearchResult{
		{ID: "blank", Content: "   ", Score: 0.1},
		nil,
		{ID: "b", Content: "second body", Score: 0.5},
		{ID: "c", Content: "first body", Score: 0.8},
	}

	all := BuildCandidates(ctx, results, 0)
	if all.Len() != 2 {
		t.Fatalf("candidates = %d, want 2 (blank passage and nil row skipped)", all.Len())
	}
	if all.Rows[0].ID != "b" || all.Rows[1].ID != "c" {
		t.Fatalf("unexpected candidate order: %q, %q", all.Rows[0].ID, all.Rows[1].ID)
	}
	if all.Indices[0] != 2 || all.Indices[1] != 3 {
		t.Fatalf("indices = %v, want [2 3]", all.Indices)
	}
	if all.Passages[1] != "first body" {
		t.Fatalf("passage = %q, want the cleaned content", all.Passages[1])
	}

	capped := BuildCandidates(ctx, results, 1)
	if capped.Len() != 1 || capped.Rows[0].ID != "c" {
		t.Fatalf("cap kept the wrong row: %#v", capped.Rows)
	}

	// 入参行不被修改
	if results[2].RerankPassage != "" || results[3].RerankPassage != "" {
		t.Fatal("BuildCandidates must not write to the input rows")
	}
}

// 阈值降级的四条分支都必须可解释，并且绝不重复调用模型。
func TestApplyThresholdOutcomes(t *testing.T) {
	t.Parallel()
	scores := []rerank.RankResult{
		{Index: 0, RelevanceScore: 0.05},
		{Index: 1, RelevanceScore: 0.35},
		{Index: 2, RelevanceScore: 0.9},
	}

	passing, outcome, effective := ApplyThreshold(scores, 0.2, DefaultFallbackMinScore)
	if outcome != types.RerankOutcomeOK || effective != 0.2 || len(passing) != 2 {
		t.Fatalf("ok branch = (%d, %v, %v)", len(passing), outcome, effective)
	}

	passing, outcome, effective = ApplyThreshold(scores, 0.95, DefaultFallbackMinScore)
	if outcome != types.RerankOutcomeThresholdDegraded || len(passing) != 1 {
		t.Fatalf("degrade branch = (%d, %v, %v)", len(passing), outcome, effective)
	}
	if effective >= 0.95 || effective < degradeFloor {
		t.Fatalf("degraded threshold %v should sit in [%v, 0.95)", effective, degradeFloor)
	}

	// 阈值已经不高于 degradeFloor，不再降级；最高分够到 fallback 线时只留一条。
	low := []rerank.RankResult{
		{Index: 0, RelevanceScore: 0.2},
		{Index: 1, RelevanceScore: 0.05},
	}
	passing, outcome, effective = ApplyThreshold(low, 0.3, DefaultFallbackMinScore)
	if outcome != types.RerankOutcomeFallbackTop1 || len(passing) != 1 || passing[0].Index != 0 {
		t.Fatalf("fallback branch = (%d, %v, %v)", len(passing), outcome, effective)
	}
	if effective != 0.3 {
		t.Fatalf("threshold at the degrade floor must not be lowered, got %v", effective)
	}

	weak := []rerank.RankResult{{Index: 0, RelevanceScore: 0.01}}
	passing, outcome, _ = ApplyThreshold(weak, 0.3, DefaultFallbackMinScore)
	if outcome != types.RerankOutcomeAllBelowThreshold || len(passing) != 0 {
		t.Fatalf("reject-all branch = (%d, %v)", len(passing), outcome)
	}

	// 显式限定检索范围时，最高分候选无条件保留。
	passing, outcome, _ = ApplyThreshold(weak, 0.3, FallbackMinScore(true))
	if outcome != types.RerankOutcomeFallbackTop1 || len(passing) != 1 {
		t.Fatalf("explicit-scope branch = (%d, %v)", len(passing), outcome)
	}
}

// 显式范围下保留门槛为 0，其余情况沿用历史默认值 0.15。
func TestFallbackMinScore(t *testing.T) {
	t.Parallel()
	if got := FallbackMinScore(false); got != DefaultFallbackMinScore {
		t.Fatalf("FallbackMinScore(false) = %v, want %v", got, DefaultFallbackMinScore)
	}
	if got := FallbackMinScore(true); got != 0 {
		t.Fatalf("FallbackMinScore(true) = %v, want 0", got)
	}
}

// 超长 passage 必须被裁到模型声明的上限，而不是让整批请求失败。
func TestFitPassagesTrimsToLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	passages := []string{strings.Repeat("字", 100), "short"}

	if trimmed := FitPassages(ctx, passages, 0); trimmed != 0 {
		t.Fatalf("limit 0 must mean no trimming, got %d", trimmed)
	}
	trimmed := FitPassages(ctx, passages, 10)
	if trimmed != 1 {
		t.Fatalf("trimmed = %d, want 1", trimmed)
	}
	if got := len([]rune(passages[0])); got != 10 {
		t.Fatalf("passage length = %d runes, want 10", got)
	}
	if passages[1] != "short" {
		t.Fatalf("short passage must be untouched, got %q", passages[1])
	}
}

// MMR 只选择、不改写：返回的必须还是入参指针，且分数字段不变。
func TestSelectMMRDoesNotMutateRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rows := []*types.SearchResult{
		{ID: "a", Content: "alpha beta gamma", Score: 0.9},
		{ID: "b", Content: "alpha beta gamma", Score: 0.8},
		{ID: "c", Content: "completely different tokens here", Score: 0.7},
	}
	before := []float64{rows[0].Score, rows[1].Score, rows[2].Score}

	picked := SelectMMR(ctx, rows, 2, 0.7)

	if len(picked) != 2 {
		t.Fatalf("picked = %d, want 2", len(picked))
	}
	for i, r := range rows {
		if r.Score != before[i] {
			t.Fatalf("row %s score mutated: %v -> %v", r.ID, before[i], r.Score)
		}
	}
	if picked[0] != rows[0] {
		t.Fatal("MMR must return the input pointers, not copies")
	}
	if picked[1].ID == "b" {
		t.Fatal("MMR should prefer the diverse row over a near-duplicate")
	}
	if got := SelectMMR(ctx, rows, 0, 0.7); got != nil {
		t.Fatalf("k=0 must select nothing, got %d", len(got))
	}
}

// ModelPassage 是只读投影：合并图片描述与生成问题，但绝不回写 Content。
func TestModelPassageIsReadOnlyProjection(t *testing.T) {
	t.Parallel()
	row := &types.SearchResult{
		ID:        "c1",
		Content:   "# 标题\n\n**正文** ![img](http://example.com/a.png)",
		ImageInfo: `[{"caption":"一张架构图","ocr_text":"OCR 文本"}]`,
	}

	passage := ModelPassage(context.Background(), row)

	if !strings.Contains(passage, "一张架构图") || !strings.Contains(passage, "OCR 文本") {
		t.Fatalf("passage lost image enrichments: %q", passage)
	}
	if strings.Contains(passage, "![](") || strings.Contains(passage, "**") {
		t.Fatalf("passage kept markdown noise: %q", passage)
	}
	if !strings.Contains(row.Content, "**正文**") {
		t.Fatalf("ModelPassage mutated Content: %q", row.Content)
	}
	if ModelPassage(context.Background(), nil) != "" {
		t.Fatal("nil row must produce an empty passage")
	}
}
