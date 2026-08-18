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
func AutoRoute(
	fileType string,
	fileSize int64,
	overrides map[string]string,
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
