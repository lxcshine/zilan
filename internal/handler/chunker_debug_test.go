package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/infrastructure/chunker"
	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestComputeChunkSizeStats_Empty(t *testing.T) {
	stats := computeChunkSizeStats(nil)
	if stats.Count != 0 || stats.AvgChars != 0 || stats.MaxChars != 0 {
		t.Errorf("empty input should yield zero stats, got %+v", stats)
	}
}

func TestComputeChunkSizeStats_SingleChunk(t *testing.T) {
	stats := computeChunkSizeStats([]int{500})
	if stats.Count != 1 {
		t.Errorf("count: got %d want 1", stats.Count)
	}
	if stats.AvgChars != 500 || stats.MinChars != 500 || stats.MaxChars != 500 {
		t.Errorf("single-chunk stats should all equal 500, got %+v", stats)
	}
	if stats.StddevChars != 0 {
		t.Errorf("stddev for one element should be 0, got %d", stats.StddevChars)
	}
}

func TestComputeChunkSizeStats_VaryingSizes(t *testing.T) {
	// 100, 200, 300, 400, 500 → avg 300, stddev ≈ 141
	stats := computeChunkSizeStats([]int{100, 200, 300, 400, 500})
	if stats.Count != 5 {
		t.Errorf("count: got %d want 5", stats.Count)
	}
	if stats.AvgChars != 300 {
		t.Errorf("avg: got %d want 300", stats.AvgChars)
	}
	if stats.MinChars != 100 || stats.MaxChars != 500 {
		t.Errorf("min/max: got %d/%d want 100/500", stats.MinChars, stats.MaxChars)
	}
	if stats.StddevChars < 130 || stats.StddevChars > 150 {
		t.Errorf("stddev: got %d, want ~141", stats.StddevChars)
	}
}

func TestComputeChunkSizeStats_NoVarianceUnderflow(t *testing.T) {
	// All identical — variance must clamp to 0 not flip negative on
	// float-precision rounding.
	stats := computeChunkSizeStats([]int{1234, 1234, 1234, 1234})
	if stats.StddevChars != 0 {
		t.Errorf("identical values must yield stddev=0, got %d", stats.StddevChars)
	}
}

// --- PreviewChunking httptest -------------------------------------------------

func newPreviewRouter() *gin.Engine {
	r := gin.New()
	r.POST("/chunker/preview", PreviewChunking)
	return r
}

func postPreview(t *testing.T, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := newPreviewRouter()
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/chunker/preview", buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var parsed map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &parsed)
	}
	return w, parsed
}

func TestPreviewChunking_HappyPath_AutoStrategy(t *testing.T) {
	body := PreviewChunkingRequest{
		Text: "# Top\nintro paragraph here.\n\n## Section A\nbody A.\n\n## Section B\nbody B.",
		ChunkingConfig: PreviewChunkingPayload{
			ChunkSize:    200,
			ChunkOverlap: 20,
			Separators:   []string{"\n\n", "\n"},
			Strategy:     "auto",
		},
	}
	w, parsed := postPreview(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	if parsed["success"] != true {
		t.Fatalf("success flag missing or false: %v", parsed)
	}
	data, ok := parsed["data"].(map[string]any)
	if !ok {
		t.Fatalf("data missing: %v", parsed)
	}
	if data["selected_tier"] == "" {
		t.Errorf("selected_tier must be set, got %v", data["selected_tier"])
	}
	if _, ok := data["chunks"].([]any); !ok {
		t.Errorf("chunks must be an array, got %T", data["chunks"])
	}
	stats, ok := data["stats"].(map[string]any)
	if !ok {
		t.Fatalf("stats must be an object, got %T", data["stats"])
	}
	if c, _ := stats["count"].(float64); c <= 0 {
		t.Errorf("stats.count should be > 0, got %v", stats["count"])
	}
}

func TestPreviewChunking_RejectsEmptyText(t *testing.T) {
	w, parsed := postPreview(t, PreviewChunkingRequest{Text: "   \n\t   "})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
	if errStr, _ := parsed["error"].(string); !strings.Contains(errStr, "empty") {
		t.Errorf("error should mention 'empty', got %q", errStr)
	}
}

func TestPreviewChunking_RejectsOversizedText(t *testing.T) {
	body := PreviewChunkingRequest{Text: strings.Repeat("a", previewMaxChars+1)}
	w, parsed := postPreview(t, body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d want 413", w.Code)
	}
	if parsed["limit"] == nil {
		t.Errorf("response should include limit hint, got %v", parsed)
	}
}

func TestPreviewChunking_LegacyStrategy_NoProfile(t *testing.T) {
	// Auto-strategy is the only path that produces a profile inside
	// SplitWithDiagnostics. For explicit strategies the handler
	// materializes one itself so the UI always sees stats.
	body := PreviewChunkingRequest{
		Text: "para one.\n\npara two.\n\npara three.\n\npara four.",
		ChunkingConfig: PreviewChunkingPayload{
			ChunkSize:    100,
			ChunkOverlap: 10,
			Separators:   []string{"\n\n"},
			Strategy:     "legacy",
		},
	}
	w, parsed := postPreview(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	data := parsed["data"].(map[string]any)
	if data["profile"] == nil {
		t.Error("profile should be materialized for explicit strategy too")
	}
	if string(chunker.StrategyTier(data["selected_tier"].(string))) != string(chunker.TierLegacy) {
		t.Errorf("selected_tier: got %v want %s", data["selected_tier"], chunker.TierLegacy)
	}
}

func TestPreviewChunking_ChunkTruncation(t *testing.T) {
	// Build text that produces > previewMaxChunks chunks.
	body := PreviewChunkingRequest{
		Text: strings.Repeat("x.\n\n", previewMaxChunks+50),
		ChunkingConfig: PreviewChunkingPayload{
			ChunkSize:    3,
			ChunkOverlap: 0,
			Separators:   []string{"\n\n"},
			Strategy:     "legacy",
		},
	}
	w, parsed := postPreview(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	data := parsed["data"].(map[string]any)
	chunks := data["chunks"].([]any)
	if len(chunks) > previewMaxChunks {
		t.Errorf("chunks should be truncated to ≤%d, got %d", previewMaxChunks, len(chunks))
	}
	stats := data["stats"].(map[string]any)
	if truncated, _ := stats["truncated_to"].(float64); int(truncated) <= previewMaxChunks {
		t.Errorf("stats.truncated_to should reflect ORIGINAL count > %d, got %v", previewMaxChunks, truncated)
	}
}

// ---------------------------------------------------------------------------
// FR-15 规则④：切分策略与结果必须在知识库详情页可见。预览接口是这条规则的
// 数据来源，它必须回传命中的策略档、尝试过的策略链、每档被拒的原因以及文档画像。
// ---------------------------------------------------------------------------

func fr15HeadingSample() string {
	var sb strings.Builder
	sb.WriteString("# 第一章 总览\n\n")
	for i := 0; i < 5; i++ {
		sb.WriteString("## 小节 " + string(rune('A'+i)) + "\n\n")
		sb.WriteString(strings.Repeat("这一段的正文内容用来撑满一个小节。", 20))
		sb.WriteString("\n\n")
	}
	return sb.String()
}

func TestPreviewChunking_FR15_ExposesTierChainAndProfile(t *testing.T) {
	w, parsed := postPreview(t, PreviewChunkingRequest{
		Text: fr15HeadingSample(),
		ChunkingConfig: PreviewChunkingPayload{
			ChunkSize:    600,
			ChunkOverlap: 60,
			Separators:   []string{"\n\n", "\n"},
			Strategy:     "auto",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	data := parsed["data"].(map[string]any)

	if got, _ := data["selected_tier"].(string); got != string(chunker.TierHeading) {
		t.Errorf("selected_tier = %v, want %s", data["selected_tier"], chunker.TierHeading)
	}
	chain, ok := data["tier_chain"].([]any)
	if !ok || len(chain) == 0 {
		t.Fatalf("tier_chain must list the attempted tiers, got %v", data["tier_chain"])
	}
	if chain[0] != string(chunker.TierHeading) {
		t.Errorf("tier_chain[0] = %v, want %s", chain[0], chunker.TierHeading)
	}
	if chain[len(chain)-1] != string(chunker.TierLegacy) {
		t.Errorf("tier_chain must end with the legacy fallback, got %v", chain[len(chain)-1])
	}
	// rejected is present (nil/null when nothing was rejected) so the UI can
	// always render the row.
	if _, present := data["rejected"]; !present {
		t.Error("rejected must be part of the response contract")
	}

	profile, ok := data["profile"].(map[string]any)
	if !ok {
		t.Fatalf("profile must be an object, got %T", data["profile"])
	}
	if total, _ := profile["md_heading_total"].(float64); total < 3 {
		t.Errorf("profile.md_heading_total = %v, want >= 3", profile["md_heading_total"])
	}
	for _, key := range []string{"total_chars", "total_lines", "form_feed_count", "chinese_chapter_count", "detected_langs"} {
		if _, present := profile[key]; !present {
			t.Errorf("profile is missing %s — the debug panel renders it", key)
		}
	}
}

func TestPreviewChunking_FR15_RejectionReasonsVisible(t *testing.T) {
	// Twelve sibling H1 sections that are each far below the tiny-chunk floor:
	// the heading tier is rejected and the chain falls back to legacy.
	var sb strings.Builder
	for i := 0; i < 12; i++ {
		sb.WriteString("# H" + string(rune('0'+i)) + "\nshort body.\n\n")
	}
	w, parsed := postPreview(t, PreviewChunkingRequest{
		Text: sb.String(),
		ChunkingConfig: PreviewChunkingPayload{
			ChunkSize:    400,
			ChunkOverlap: 20,
			Separators:   []string{"\n\n", "\n"},
			Strategy:     "auto",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	data := parsed["data"].(map[string]any)
	if got, _ := data["selected_tier"].(string); got != string(chunker.TierLegacy) {
		t.Errorf("selected_tier = %v, want %s after rejection", data["selected_tier"], chunker.TierLegacy)
	}
	rejected, ok := data["rejected"].([]any)
	if !ok || len(rejected) == 0 {
		t.Fatalf("rejected should explain the fallback, got %v", data["rejected"])
	}
	first, ok := rejected[0].(map[string]any)
	if !ok {
		t.Fatalf("rejected[0] must be an object, got %T", rejected[0])
	}
	if first["tier"] != string(chunker.TierHeading) {
		t.Errorf("rejected[0].tier = %v, want %s", first["tier"], chunker.TierHeading)
	}
	if reason, _ := first["reason"].(string); reason == "" {
		t.Errorf("rejected[0].reason must be non-empty: %v", first)
	}
}

// 存量兼容（PRD §5.4）：知识库没有显式选择策略时前端传空字符串，预览必须显示
// legacy 档，且 tier_chain 不包含任何结构感知档位。
func TestPreviewChunking_FR15_EmptyStrategyStaysLegacy(t *testing.T) {
	w, parsed := postPreview(t, PreviewChunkingRequest{
		Text: fr15HeadingSample(),
		ChunkingConfig: PreviewChunkingPayload{
			ChunkSize:    600,
			ChunkOverlap: 60,
			Separators:   []string{"\n\n", "\n"},
			Strategy:     "",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	data := parsed["data"].(map[string]any)
	if got, _ := data["selected_tier"].(string); got != string(chunker.TierLegacy) {
		t.Errorf("selected_tier = %v, want %s for an empty (存量) strategy", data["selected_tier"], chunker.TierLegacy)
	}
	chain, ok := data["tier_chain"].([]any)
	if !ok || len(chain) != 1 || chain[0] != string(chunker.TierLegacy) {
		t.Errorf("tier_chain = %v, want [legacy] only", data["tier_chain"])
	}
	if data["profile"] == nil {
		t.Error("profile should still be materialized so the panel shows document stats")
	}
}
