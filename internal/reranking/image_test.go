package reranking

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func imgRow(id string, vectorScore float64) *types.SearchResult {
	return &types.SearchResult{
		ID:            id,
		ChunkType:     types.ChunkTypeImageVector,
		MatchType:     types.MatchTypeEmbedding,
		VectorScore:   vectorScore,
		Metadata:      map[string]string{},
		Content:       "a product photo",
	}
}

func textRow(id string) *types.SearchResult {
	return &types.SearchResult{
		ID:       id,
		MatchType: types.MatchTypeEmbedding,
		Metadata: map[string]string{},
		Content:  "text row",
	}
}

func TestIsPictorialImageHit(t *testing.T) {
	if !IsPictorialImageHit(imgRow("a", 0.9)) {
		t.Fatal("image_vector embedding hit should be pictorial")
	}
	if IsPictorialImageHit(textRow("b")) {
		t.Fatal("text row must not be pictorial")
	}
	// 图片行但非向量命中（如父块路径）不算图片腿命中。
	parent := imgRow("c", 0.9)
	parent.MatchType = types.MatchTypeParentChunk
	if IsPictorialImageHit(parent) {
		t.Fatal("non-embedding image row must not be pictorial")
	}
}

func TestKeepImages(t *testing.T) {
	rows := []*types.SearchResult{
		textRow("t1"),
		imgRow("img-keep", 0.9), // 高向量分：应兜底保留
		textRow("t2"),
		imgRow("img-drop", 0.2), // 低向量分：不保留
	}
	candidates := BuildCandidates(context.Background(), rows, 3)
	reranked := []*types.SearchResult{candidates.Rows[0], candidates.Rows[2]}
	diag := types.RerankDiagnostics{}
	out := KeepImages(context.Background(), candidates, reranked, 0.45, &diag)
	if len(out) != 3 {
		t.Fatalf("expected 3 rows after keep, got %d", len(out))
	}
	if diag.ImagesKept != 1 {
		t.Fatalf("ImagesKept = %d, want 1", diag.ImagesKept)
	}
	kept := out[2]
	if kept.ID != "img-keep" {
		t.Fatalf("kept row = %s", kept.ID)
	}
	if kept.Metadata[types.MetadataKeptBy] != types.KeptByImageVector {
		t.Fatal("kept row must carry KeptByImageVector marker")
	}
	// 入参行保持只读：原行不应被改写。
	if candidates.Rows[1].Metadata[types.MetadataKeptBy] != "" {
		t.Fatal("source row must not be mutated")
	}
}

func TestKeepImagesRespectsCap(t *testing.T) {
	rows := []*types.SearchResult{
		imgRow("i1", 0.9), imgRow("i2", 0.9), imgRow("i3", 0.9),
		textRow("t1"),
	}
	candidates := BuildCandidates(context.Background(), rows, 4)
	diag := types.RerankDiagnostics{}
	out := KeepImages(context.Background(), candidates, []*types.SearchResult{candidates.Rows[3]}, 0.45, &diag)
	if len(out) != 1+MaxKeptImages {
		t.Fatalf("expected %d rows, got %d", 1+MaxKeptImages, len(out))
	}
}

func TestImageKeepScoreFor(t *testing.T) {
	if got := ImageKeepScoreFor(0.4); got < 0.44 || got > 0.46 {
		t.Fatalf("ImageKeepScoreFor(0.4) = %f", got)
	}
}

func TestRestoreKeptImages(t *testing.T) {
	rows := []*types.SearchResult{textRow("t1"), textRow("t2"), textRow("t3")}
	marked := ScoredCopy(imgRow("img", 0.9), 0.01, 1.0, "p")
	marked.Metadata[types.MetadataKeptBy] = types.KeptByImageVector
	scored := []*types.SearchResult{rows[0], rows[1], rows[2], marked}
	final := []*types.SearchResult{rows[0], rows[1]} // top-2 丢了 marked
	out := RestoreKeptImages(final, scored)
	if len(out) != 3 {
		t.Fatalf("expected kept image restored, got %d rows", len(out))
	}
	if out[len(out)-1].ID != "img" {
		t.Fatal("restored row should be the marked image hit")
	}
}
