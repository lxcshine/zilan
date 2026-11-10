package docparser

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// FR-13：OCR 流水线 id 从 parser_engine_overrides 读取，缺失/未知一律回落手动。
func TestOCRPipelineFromOverrides(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		want      types.OCRPipelineID
	}{
		{"nil overrides", nil, types.OCRPipelineManual},
		{"empty overrides", map[string]string{}, types.OCRPipelineManual},
		{"explicit manual", map[string]string{OCRPipelineOverrideKey: "manual"}, types.OCRPipelineManual},
		{"explicit smart", map[string]string{OCRPipelineOverrideKey: "smart"}, types.OCRPipelineSmart},
		{"upper case smart", map[string]string{OCRPipelineOverrideKey: "SMART"}, types.OCRPipelineSmart},
		{"unknown falls back", map[string]string{OCRPipelineOverrideKey: "smartocr"}, types.OCRPipelineManual},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OCRPipelineFromOverrides(c.overrides); got != c.want {
				t.Errorf("OCRPipelineFromOverrides = %q, want %q", got, c.want)
			}
		})
	}
}

var ocrRouteEngines = []types.ParserEngineInfo{
	{Name: "builtin", Available: true},
	{Name: "mineru", Available: true},
	{Name: "paddleocr_vl", Available: true},
}

// 手动流水线（含 FR-13 之前的调用方：不传 overrides）必须得到与以前完全相同的
// 引擎与回退链，只在 Reason 里多记一条流水线 id。
func TestAutoRouteManualPipelineKeepsLegacyDecision(t *testing.T) {
	legacy := autoRouteBySize("pdf", 2*1024*1024, ocrRouteEngines)
	manual := AutoRoute("pdf", 2*1024*1024, map[string]string{OCRPipelineOverrideKey: "manual"}, ocrRouteEngines)
	none := AutoRoute("pdf", 2*1024*1024, nil, ocrRouteEngines)

	for _, got := range []RouteDecision{manual, none} {
		if got.Engine != legacy.Engine {
			t.Errorf("engine = %q, legacy %q", got.Engine, legacy.Engine)
		}
		if strings.Join(got.FallbackChain, ",") != strings.Join(legacy.FallbackChain, ",") {
			t.Errorf("fallback = %v, legacy %v", got.FallbackChain, legacy.FallbackChain)
		}
		if got.OCRPipeline != "manual" {
			t.Errorf("OCRPipeline = %q", got.OCRPipeline)
		}
		if !strings.HasPrefix(got.Reason, legacy.Reason) {
			t.Errorf("reason %q should extend legacy %q", got.Reason, legacy.Reason)
		}
		if !strings.Contains(got.Reason, "ocr_pipeline=manual") {
			t.Errorf("reason should record the pipeline: %q", got.Reason)
		}
	}
}

// 智能流水线：回退链里 OCR 专用引擎提前，成员不变。
func TestAutoRouteSmartPipelinePrefersOCREngines(t *testing.T) {
	legacy := autoRouteBySize("pdf", 2*1024*1024, ocrRouteEngines)
	smart := AutoRoute("pdf", 2*1024*1024, map[string]string{OCRPipelineOverrideKey: "smart"}, ocrRouteEngines)

	if smart.OCRPipeline != "smart" {
		t.Errorf("OCRPipeline = %q", smart.OCRPipeline)
	}
	if smart.Engine != legacy.Engine {
		t.Errorf("smart pipeline must not change the primary engine: %q vs %q", smart.Engine, legacy.Engine)
	}
	if len(smart.FallbackChain) != len(legacy.FallbackChain) {
		t.Fatalf("fallback membership changed: %v vs %v", smart.FallbackChain, legacy.FallbackChain)
	}
	if smart.FallbackChain[0] != "paddleocr_vl" {
		t.Errorf("OCR-capable engine should come first, got %v", smart.FallbackChain)
	}
	// 成员集合不变，只是顺序变了。
	for _, e := range legacy.FallbackChain {
		if !strings.Contains(strings.Join(smart.FallbackChain, ","), e) {
			t.Errorf("engine %q lost from the chain", e)
		}
	}
	if !strings.Contains(smart.Reason, "prefers OCR-capable engines") {
		t.Errorf("reason should explain the reorder: %q", smart.Reason)
	}
}

// 大 PDF 的回退链里只有 builtin：智能流水线不得把它换成别的引擎，
// 否则质量重试会失去原有的兜底路径。
func TestAutoRouteSmartPipelineKeepsLargePDFFallback(t *testing.T) {
	legacy := autoRouteBySize("pdf", 15*1024*1024, ocrRouteEngines)
	smart := AutoRoute("pdf", 15*1024*1024, map[string]string{OCRPipelineOverrideKey: "smart"}, ocrRouteEngines)
	if strings.Join(smart.FallbackChain, ",") != strings.Join(legacy.FallbackChain, ",") {
		t.Errorf("fallback changed: %v vs %v", smart.FallbackChain, legacy.FallbackChain)
	}
}

// 非 PDF 文件不做 OCR 重排。
func TestAutoRouteSmartPipelineIgnoresNonPDF(t *testing.T) {
	got := AutoRoute("docx", 5000, map[string]string{OCRPipelineOverrideKey: "smart"}, ocrRouteEngines)
	if got.Engine != "builtin" || len(got.FallbackChain) != 0 {
		t.Errorf("non-PDF decision changed: %+v", got)
	}
	if got.OCRPipeline != "smart" {
		t.Errorf("pipeline should still be recorded, got %q", got.OCRPipeline)
	}
}

// 没有 OCR 专用引擎时，智能流水线不改动回退链。
func TestAutoRouteSmartPipelineWithoutOCREngines(t *testing.T) {
	engines := []types.ParserEngineInfo{
		{Name: "builtin", Available: true},
		{Name: "mineru", Available: true},
	}
	legacy := autoRouteBySize("pdf", 2*1024*1024, engines)
	smart := AutoRoute("pdf", 2*1024*1024, map[string]string{OCRPipelineOverrideKey: "smart"}, engines)
	if strings.Join(smart.FallbackChain, ",") != strings.Join(legacy.FallbackChain, ",") {
		t.Errorf("fallback changed without OCR engines: %v vs %v", smart.FallbackChain, legacy.FallbackChain)
	}
}

func TestPreferOCREnginesIsStable(t *testing.T) {
	in := []string{"mineru", "paddleocr_vl", "mineru_cloud", "paddleocr_vl_cloud"}
	want := []string{"paddleocr_vl", "paddleocr_vl_cloud", "mineru", "mineru_cloud"}
	got := preferOCREngines(in)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("preferOCREngines = %v, want %v", got, want)
	}
	// 单元素与空链原样返回。
	if got := preferOCREngines([]string{"builtin"}); len(got) != 1 || got[0] != "builtin" {
		t.Errorf("single entry changed: %v", got)
	}
	if got := preferOCREngines(nil); len(got) != 0 {
		t.Errorf("nil chain changed: %v", got)
	}
}
