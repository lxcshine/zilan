package docparser

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestAutoRouteNonPDF(t *testing.T) {
	decision := AutoRoute("docx", 5000, nil, nil)
	if decision.Engine != "builtin" {
		t.Errorf("expected builtin, got %s", decision.Engine)
	}
}

func TestAutoRouteNoHeavyEngines(t *testing.T) {
	engines := []types.ParserEngineInfo{
		{Name: "builtin", Available: true},
	}
	decision := AutoRoute("pdf", 5*1024*1024, nil, engines)
	if decision.Engine != "builtin" {
		t.Errorf("expected builtin, got %s", decision.Engine)
	}
}

func TestAutoRouteLargePDFWithMinerU(t *testing.T) {
	engines := []types.ParserEngineInfo{
		{Name: "builtin", Available: true},
		{Name: "mineru", Available: true},
	}
	decision := AutoRoute("pdf", 15*1024*1024, nil, engines)
	if decision.Engine != "mineru" {
		t.Errorf("expected mineru, got %s", decision.Engine)
	}
}

func TestAutoRouteSmallPDFDefaultsBuiltin(t *testing.T) {
	engines := []types.ParserEngineInfo{
		{Name: "builtin", Available: true},
		{Name: "mineru", Available: true},
		{Name: "paddleocr_vl", Available: true},
	}
	decision := AutoRoute("pdf", 2*1024*1024, nil, engines)
	if decision.Engine != "builtin" {
		t.Errorf("expected builtin, got %s", decision.Engine)
	}
	if len(decision.FallbackChain) == 0 {
		t.Error("expected fallback chain to be non-empty")
	}
}

func TestShouldRetryWithHeavierEngine(t *testing.T) {
	chain := []string{"builtin", "mineru"}

	// Good quality - no retry
	engine, ok := ShouldRetryWithHeavierEngine("builtin", chain, 0.85)
	if ok || engine != "" {
		t.Error("expected no retry for good quality")
	}

	// Bad quality - retry with next engine
	engine, ok = ShouldRetryWithHeavierEngine("builtin", chain, 0.45)
	if !ok || engine != "mineru" {
		t.Errorf("expected retry with mineru, got engine=%s ok=%v", engine, ok)
	}

	// Already on last engine - no more retries
	engine, ok = ShouldRetryWithHeavierEngine("mineru", chain, 0.30)
	if ok || engine != "" {
		t.Error("expected no retry when already on last fallback")
	}
}

func TestParseQualityFromMetadata(t *testing.T) {
	// Good quality metadata
	score, retry, reason := ParseQualityFromMetadata(map[string]string{
		"quality_score":        "0.9500",
		"quality_should_retry": "False",
		"quality_retry_reason": "",
	})
	if score != 0.95 {
		t.Errorf("expected score 0.95, got %f", score)
	}
	if retry {
		t.Error("expected no retry")
	}

	// Bad quality metadata
	score, retry, reason = ParseQualityFromMetadata(map[string]string{
		"quality_score":        "0.4500",
		"quality_should_retry": "True",
		"quality_retry_reason": "garble_rate=0.25",
	})
	if score != 0.45 {
		t.Errorf("expected score 0.45, got %f", score)
	}
	if !retry {
		t.Error("expected retry")
	}
	if reason != "garble_rate=0.25" {
		t.Errorf("unexpected reason: %s", reason)
	}

	// No metadata - defaults to perfect score
	score, retry, reason = ParseQualityFromMetadata(nil)
	if score != 1.0 {
		t.Errorf("expected default score 1.0, got %f", score)
	}
}
