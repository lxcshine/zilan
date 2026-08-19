package contextx

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/embedding"
)

// ---------------------------------------------------------------------------
// Relevance tiering — different rendering for high/mid/low score chunks
// ---------------------------------------------------------------------------

// TierThresholds define the score boundaries for high/mid/low tiers.
type TierThresholds struct {
	High float64 // score >= High => full content
	Mid  float64 // score >= Mid  => first half content (paragraph boundary)
}

// DefaultTierThresholds: tuned for typical cosine-similarity rerank output
// where ~0.8 is high, ~0.55 is mid.
func DefaultTierThresholds() TierThresholds {
	return TierThresholds{High: 0.80, Mid: 0.55}
}

// TierResult carries a relevance-tuned rendering plus metadata.
type TierResult struct {
	ID          string
	KnowledgeID string
	Tier        string // "high" | "mid" | "low"
	Content     string // the rendered context text
	Title       string // knowledge title
	HeadingPath string // e.g. "1.2 安装指南 > 1.2.3 Docker部署"
	Score       float64
}

// TieredRender maps a flat search-result list into tiered renderings:
//
//	high — full content, kept intact
//	mid  — first half of content (paragraph boundary)
//	low  — title + first 1-2 sentences (summary teaser)
func TieredRender(
	results []*TierResult,
	thresholds TierThresholds,
) []*TierResult {
	out := make([]*TierResult, 0, len(results))
	for _, r := range results {
		if r == nil {
			continue
		}
		clone := *r
		score := r.Score
		switch {
		case score >= thresholds.High:
			clone.Tier = "high"
			clone.Content = r.Content
		case score >= thresholds.Mid:
			clone.Tier = "mid"
			clone.Content = firstHalfAtParagraph(r.Content)
		default:
			clone.Tier = "low"
			clone.Content = teaser(r.Content)
		}
		out = append(out, &clone)
	}
	return out
}

func firstHalfAtParagraph(s string) string {
	// Split on \n\n (paragraph); fallback on \n; fallback on sentences.
	half := len(s) / 2
	if idx := strings.LastIndex(s[:half], "\n\n"); idx > half/2 {
		return strings.TrimSpace(s[:idx])
	}
	if idx := strings.LastIndex(s[:half], "\n"); idx > half/2 {
		return strings.TrimSpace(s[:idx])
	}
	if idx := strings.LastIndexAny(s[:half], "。.!?"); idx > half/2 {
		return strings.TrimSpace(s[:idx+1])
	}
	return s[:half]
}

func teaser(s string) string {
	// First sentence(s) up to ~120 runes.
	const limit = 120
	if len([]rune(s)) <= limit {
		return s
	}
	if idx := strings.IndexAny(s, "。.!?"); idx > 20 && idx < limit {
		return strings.TrimSpace(s[:idx+1])
	}
	if idx := strings.IndexByte(s, '\n'); idx > 20 && idx < limit {
		return strings.TrimSpace(s[:idx])
	}
	runes := []rune(s)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Semantic deduplication using embedding cosine similarity
// ---------------------------------------------------------------------------

// SemanticDedupConfig controls deduplication behavior.
type SemanticDedupConfig struct {
	Threshold float64 // cosine similarity threshold for considering two chunks duplicates
}

// DefaultSemanticDedupConfig uses 0.92 cosine similarity.
func DefaultSemanticDedupConfig() SemanticDedupConfig {
	return SemanticDedupConfig{Threshold: 0.92}
}

// SemanticDedup removes near-duplicate chunks by embedding cosine similarity.
// Higher-score chunks are retained; lower-score duplicates are dropped.
// The embedder is reused when non-nil to save an API call on already-embedded
// chunks (when the caller has already generated embeddings upstream).
func SemanticDedup(
	ctx context.Context,
	results []*TierResult,
	cfg SemanticDedupConfig,
	embedder embedding.Embedder,
) ([]*TierResult, error) {
	if len(results) < 2 {
		return results, nil
	}
	if embedder == nil {
		return results, nil
	}

	texts := make([]string, len(results))
	for i, r := range results {
		texts[i] = r.Content
	}
	vecs, err := embedder.BatchEmbed(ctx, texts)
	if err != nil {
		return results, fmt.Errorf("semantic dedup embedding failed: %w", err)
	}

	kept := make([]bool, len(results))
	for i := range kept {
		kept[i] = true
	}

	for i := 0; i < len(vecs)-1; i++ {
		if !kept[i] {
			continue
		}
		vi := vecs[i]
		for j := i + 1; j < len(vecs); j++ {
			if !kept[j] {
				continue
			}
			if cosSim(vi, vecs[j]) >= cfg.Threshold {
				// Drop the lower-scored duplicate.
				if results[i].Score >= results[j].Score {
					kept[j] = false
				} else {
					kept[i] = false
					break // i is gone, move to next outer
				}
			}
		}
	}

	out := make([]*TierResult, 0, len(results))
	for i, k := range kept {
		if k {
			out = append(out, results[i])
		}
	}
	return out, nil
}

// SemanticDedupAsync is the same dedup run concurrently so callers that
// do NOT need blocking results can fire it in the background.
func SemanticDedupAsync(
	ctx context.Context,
	results []*TierResult,
	cfg SemanticDedupConfig,
	embedder embedding.Embedder,
) <-chan []*TierResult {
	ch := make(chan []*TierResult, 1)
	go func() {
		defer close(ch)
		out, _ := SemanticDedup(ctx, results, cfg, embedder) // errors are best-effort here
		ch <- out
	}()
	return ch
}

// cosSim returns cosine similarity of two vectors (must be same length).
func cosSim(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av := float64(a[i])
		bv := float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---------------------------------------------------------------------------
// Heading-path extraction from chunk content
// ---------------------------------------------------------------------------

// markdownHeadingPattern matches lines like "## 1.2.3 Title"
var markdownHeadingPattern = regexp.MustCompile(`(?m)^#{1,6}\s+([\d.]+)?\s*(.+)$`)

// ExtractHeadingPath scans the document text and returns the smallest
// document-path prefix that encloses the offset range. When chunk metadata
// already carries heading info, use that directly; this is a fallback.
func ExtractHeadingPath(doc string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(doc) {
		end = len(doc)
	}
	var parts []string
	for _, m := range markdownHeadingPattern.FindAllStringIndex(doc, -1) {
		if m[0] >= end {
			break
		}
		line := strings.TrimSpace(doc[m[0]:m[1]])
		if m[0] < start {
			parts = append(parts, headingTitle(line))
		}
	}
	return strings.Join(parts, " > ")
}

func headingTitle(line string) string {
	// Strip leading #
	line = strings.TrimLeft(line, "#")
	line = strings.TrimSpace(line)
	return line
}

// ---------------------------------------------------------------------------
// Citation formatter — turns a tiered result into a citation-enriched string
// ---------------------------------------------------------------------------

// CitationFormat controls how retrieval chunks are rendered into the prompt.
type CitationFormat struct {
	IncludeHeadingPath bool // prepend heading path when available
	IncludeScore       bool // prepend relevance tier label
}

// RenderCitation turns a TierResult into a string ready for embedding
// into L2 (Retrieval) context.
func RenderCitation(r *TierResult, f CitationFormat) string {
	var b strings.Builder
	if f.IncludeHeadingPath && r.HeadingPath != "" {
		b.WriteString("[" + r.HeadingPath + "] ")
	}
	if f.IncludeScore && r.Tier != "" {
		b.WriteString("{" + r.Tier + "} ")
	}
	b.WriteString(r.Content)
	return b.String()
}

// ShrinkRetrievalFromResults is a convenience wrapper that callers can hand
// to Assembler.Input.ShrinkRetrieval: it tier-renders, dedups, then drops
// low-scored items until the budget is met.
type ShrinkRetrievalFromResults struct {
	Results    []*TierResult
	Thresholds TierThresholds
	Deduploop  func() ([]*TierResult, error)
	Format     CitationFormat
}

// Shrink returns a single string fitting the token budget by progressively
// dropping low-tier chunks and then truncating remaining high-tier ones.
func (s *ShrinkRetrievalFromResults) Shrink(ctx context.Context, budgetTokens int, counter *Counter) string {
	ranked := s.Results
	if s.Deduploop != nil {
		deduped, err := s.Deduploop()
		if err == nil {
			ranked = deduped
		}
	}
	// Tier-render first.
	rendered := TieredRender(ranked, s.Thresholds)
	// Sort by score desc.
	for i := 0; i < len(rendered)-1; i++ {
		for j := i + 1; j < len(rendered); j++ {
			if rendered[j].Score > rendered[i].Score {
				rendered[i], rendered[j] = rendered[j], rendered[i]
			}
		}
	}

	var parts []string
	used := 0
	for _, r := range rendered {
		text := RenderCitation(r, s.Format)
		cost := counter.Count(text)
		if used+cost <= budgetTokens {
			parts = append(parts, text)
			used += cost
			continue
		}
		// Budget exhausted: if this is a high-tier chunk try to keep a part.
		if r.Tier == "high" {
			left := budgetTokens - used
			if left > 50 {
				trim := TruncateToTokens(text, left, counter)
				if trim != "" {
					parts = append(parts, trim)
					used += counter.Count(trim)
				}
			}
		}
		break
	}
	return strings.Join(parts, "\n---\n")
}
