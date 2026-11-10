package searchutil

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func imageRow(id, kb, url string) *types.SearchResult {
	return &types.SearchResult{
		ID:              id,
		KnowledgeBaseID: kb,
		ChunkType:       types.ChunkTypeImageVector,
		ImageInfo:       `[{"url":"` + url + `"}]`,
		ChunkMetadata:   types.JSON(`{"source_type":"image"}`),
		VectorScore:     0.9,
		Metadata:        map[string]string{},
	}
}

func textRow(id, kb string) *types.SearchResult {
	return &types.SearchResult{
		ID:              id,
		KnowledgeBaseID: kb,
		ChunkType:       types.ChunkTypeText,
		ImageInfo:       "",
		Metadata:        map[string]string{},
	}
}

func TestCaptureImageEvidence(t *testing.T) {
	results := []*types.SearchResult{
		textRow("t1", "kb1"),
		imageRow("img1", "kb1", "http://x/1.png"),
		textRow("t2", "kb2"),
	}
	CaptureImageEvidence(results)
	img := results[1]
	if len(img.MatchedImages) != 1 || img.MatchedImages[0].URL != "http://x/1.png" {
		t.Fatalf("image row should carry its own evidence: %+v", img.MatchedImages)
	}
	if len(results[0].MatchedImages) != 1 {
		t.Fatal("same-KB text row should inherit image evidence")
	}
	if len(results[2].MatchedImages) != 0 {
		t.Fatal("other-KB text row must not inherit")
	}
	if !IsImageEvidence(results[0]) {
		t.Fatal("IsImageEvidence should be true after inheritance")
	}
}

func TestInheritImageEvidenceDedups(t *testing.T) {
	dst := textRow("t1", "kb1")
	dst.MatchedImages = []types.MatchedImage{{ChunkID: "a", URL: "u1"}}
	src := imageRow("img1", "kb1", "u1")
	src.MatchedImages = []types.MatchedImage{{ChunkID: "a", URL: "u1"}, {ChunkID: "b", URL: "u2"}}
	InheritImageEvidence(dst, src)
	if len(dst.MatchedImages) != 2 {
		t.Fatalf("dedup failed: %+v", dst.MatchedImages)
	}
}

func TestTopKKeepingKept(t *testing.T) {
	plain := textRow("t", "kb")
	marked := textRow("m", "kb")
	marked.Metadata[types.MetadataKeptBy] = types.KeptByImageVector
	results := []*types.SearchResult{plain, marked}
	out := TopKKeepingKept(results, 1)
	if len(out) != 1 || !IsKeptOutsideTopK(out[0]) {
		t.Fatalf("marked row must survive top-k cut: %+v", out)
	}
}

func TestImageVectorSourceMarked(t *testing.T) {
	marked := imageRow("a", "kb", "u")
	if !ImageVectorSourceMarked(marked) {
		t.Fatal("source_type=image row should be marked")
	}
	stale := imageRow("b", "kb", "u")
	stale.ChunkMetadata = nil
	if !ImageVectorChunkStale(stale) {
		t.Fatal("row without metadata is stale")
	}
	if ImageVectorSourceMarked(textRow("t", "kb")) {
		t.Fatal("text row must never be marked")
	}
}

func TestVisionImageLimitsDefaults(t *testing.T) {
	l := VisionImageLimits{}.WithDefaults()
	if l.MaxBytes <= 0 || l.MaxDimension <= 0 {
		t.Fatalf("defaults missing: %+v", l)
	}
}
