package docparser

import (
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// RouteDecision describes which engine to use for a document and why.
type RouteDecision struct {
	Engine        string
	Reason        string
	FallbackChain []string
	// OCRPipeline 记录本次决策生效的 OCR 流水线（FR-13），仅用于日志与诊断：
	// 真正的按页路由在 docreader 侧依据页面分类完成。
	OCRPipeline string
}

// OCRPipelineOverrideKey 是 OCR 流水线 id 在 parser_engine_overrides 里的键名，
// 与 service.OCRPipelineOverrideKey 保持一致（此处不能反向依赖 service 包）。
const OCRPipelineOverrideKey = "ocr_pipeline"

// OCRPipelineFromOverrides 从 parser_engine_overrides 里读出 OCR 流水线 id。
// 缺失或未知一律回落到手动流水线，因此 FR-13 之前的调用方行为不变。
func OCRPipelineFromOverrides(overrides map[string]string) types.OCRPipelineID {
	if overrides == nil {
		return types.OCRPipelineManual
	}
	return types.NormalizeOCRPipelineID(types.OCRPipelineID(overrides[OCRPipelineOverrideKey]))
}

// AutoRoute decides the optimal engine for a document based on its
// characteristics and tenant configuration. It is called when
// parser_engine is empty or "auto".
//
// Routing rules (aligned with §5.1):
//  1. Scanned PDFs (>50% image pages) → paddleocr_vl
//  2. Large complex docs (>50 pages + tables) → mineru
//  3. Formula-heavy academic → mineru
//  4. Small simple docs → builtin
//  5. Default → builtin with mineru fallback
//
// FR-13：引擎选择之外，这里还读取 overrides 里的 OCR 流水线 id 并写进决策，
// 使「这次解析用的是手动还是智能流水线」在日志与 metadata 里可追溯。智能流水
// 线会把 OCR 专用引擎在回退链里提前 —— 因为它会把图片页/混合页/扫描页渲染出
// 来交给 OCR，一旦质量不达标，最该先重试的是 OCR 专用引擎而不是版面引擎。
func AutoRoute(
	fileType string,
	fileSize int64,
	overrides map[string]string,
	availableEngines []types.ParserEngineInfo,
) RouteDecision {
	pipeline := OCRPipelineFromOverrides(overrides)
	return applyOCRPipeline(autoRouteBySize(fileType, fileSize, availableEngines), pipeline, fileType)
}

// ocrFirstEngines 是「以 OCR 见长」的引擎。
var ocrFirstEngines = map[string]struct{}{
	"paddleocr_vl":       {},
	"paddleocr_vl_cloud": {},
}

// applyOCRPipeline 把 OCR 流水线信息盖到一次引擎决策上。手动流水线只补记 id，
// 不改变任何既有结论，因此 FR-13 之前的行为逐字保持。
func applyOCRPipeline(
	decision RouteDecision,
	pipeline types.OCRPipelineID,
	fileType string,
) RouteDecision {
	decision.OCRPipeline = pipeline.String()
	decision.Reason = fmt.Sprintf("%s; ocr_pipeline=%s", decision.Reason, pipeline)
	if pipeline != types.OCRPipelineSmart || strings.ToLower(fileType) != "pdf" {
		return decision
	}
	reordered := preferOCREngines(decision.FallbackChain)
	if !sameEngineOrder(reordered, decision.FallbackChain) {
		decision.FallbackChain = reordered
		decision.Reason += ", smart pipeline prefers OCR-capable engines on retry"
	}
	return decision
}

// preferOCREngines 稳定重排回退链：OCR 专用引擎提到前面，其余保持原有相对顺序。
// **只改顺序不改成员**，因此既有的重试语义不会丢（例如大 PDF 的 builtin 兜底、
// 或链里本来就没有 OCR 引擎时的原样返回）。
func preferOCREngines(chain []string) []string {
	if len(chain) < 2 {
		return chain
	}
	out := make([]string, 0, len(chain))
	for _, e := range chain {
		if _, ok := ocrFirstEngines[e]; ok {
			out = append(out, e)
		}
	}
	for _, e := range chain {
		if _, ok := ocrFirstEngines[e]; !ok {
			out = append(out, e)
		}
	}
	return out
}

func sameEngineOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// autoRouteBySize 是 AutoRoute 原有的按文件类型/大小选引擎逻辑，保持不变。
func autoRouteBySize(
	fileType string,
	fileSize int64,
	availableEngines []types.ParserEngineInfo,
) RouteDecision {
	ft := strings.ToLower(fileType)

	// Non-PDF: always builtin (docreader handles docx/xlsx/etc.)
	if ft != "pdf" {
		return RouteDecision{
			Engine: "builtin",
			Reason: fmt.Sprintf("non-PDF file type: %s", ft),
		}
	}

	// Check which heavy engines are available
	mineruAvail := isEngineAvailable(availableEngines, "mineru")
	mineruCloudAvail := isEngineAvailable(availableEngines, "mineru_cloud")
	paddleAvail := isEngineAvailable(availableEngines, "paddleocr_vl")
	paddleCloudAvail := isEngineAvailable(availableEngines, "paddleocr_vl_cloud")

	// If no heavy engines are available, use builtin
	if !mineruAvail && !mineruCloudAvail && !paddleAvail && !paddleCloudAvail {
		return RouteDecision{
			Engine: "builtin",
			Reason: "no advanced engines available, using builtin",
		}
	}

	// For now, without reading the PDF content in Go, we use file size
	// as a proxy for complexity. The Python side does detailed per-page
	// profiling; Go just picks the initial engine.
	//
	// Heuristic: large PDFs (>10MB) are likely complex (many pages, tables,
	// formulas, scanned content). Route to mineru if available.
	if fileSize > 10*1024*1024 {
		if mineruAvail {
			return RouteDecision{
				Engine:        "mineru",
				Reason:        fmt.Sprintf("large PDF (%d bytes), routing to mineru", fileSize),
				FallbackChain: []string{"builtin"},
			}
		}
		if mineruCloudAvail {
			return RouteDecision{
				Engine:        "mineru_cloud",
				Reason:        fmt.Sprintf("large PDF (%d bytes), routing to mineru_cloud", fileSize),
				FallbackChain: []string{"builtin"},
			}
		}
	}

	// Default: builtin (the Python side will auto-route internally if needed)
	return RouteDecision{
		Engine:        "builtin",
		Reason:        "default routing, Python side will auto-route",
		FallbackChain: buildFallbackChain(mineruAvail, mineruCloudAvail, paddleAvail, paddleCloudAvail),
	}
}

// ShouldRetryWithHeavierEngine decides whether to retry parsing after a
// quality failure, returning the next engine to try.
func ShouldRetryWithHeavierEngine(
	currentEngine string,
	fallbackChain []string,
	qualityScore float64,
) (string, bool) {
	// Only retry if quality is below threshold
	if qualityScore >= 0.70 {
		return "", false
	}

	// Find next engine in fallback chain. If the current engine is in the
	// chain but is already the last entry, there is nothing heavier to try.
	found := false
	for i, e := range fallbackChain {
		if e == currentEngine {
			found = true
			if i+1 < len(fallbackChain) {
				return fallbackChain[i+1], true
			}
			// Last in chain: no further fallback.
			return "", false
		}
	}

	// Current engine not in chain: try first fallback if any.
	if !found && len(fallbackChain) > 0 {
		return fallbackChain[0], true
	}

	return "", false
}

// ParseQualityFromMetadata extracts quality metrics from docreader metadata.
func ParseQualityFromMetadata(metadata map[string]string) (score float64, shouldRetry bool, reason string) {
	if metadata == nil {
		return 1.0, false, ""
	}

	// Quality fields are set by the Python pdf_quality module
	if v, ok := metadata["quality_score"]; ok {
		var s float64
		if _, err := fmt.Sscanf(v, "%f", &s); err == nil {
			score = s
		}
	}
	if v, ok := metadata["quality_should_retry"]; ok {
		shouldRetry = strings.ToLower(v) == "true"
	}
	if v, ok := metadata["quality_retry_reason"]; ok {
		reason = v
	}

	if score == 0 {
		score = 1.0 // Default: no quality data = assume good
	}

	return score, shouldRetry, reason
}

func isEngineAvailable(engines []types.ParserEngineInfo, name string) bool {
	for _, e := range engines {
		if e.Name == name && e.Available {
			return true
		}
	}
	return false
}

func buildFallbackChain(mineru, mineruCloud, paddle, paddleCloud bool) []string {
	chain := []string{}
	if mineru {
		chain = append(chain, "mineru")
	}
	if paddle {
		chain = append(chain, "paddleocr_vl")
	}
	if mineruCloud {
		chain = append(chain, "mineru_cloud")
	}
	if paddleCloud {
		chain = append(chain, "paddleocr_vl_cloud")
	}
	return chain
}
