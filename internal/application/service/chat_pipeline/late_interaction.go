package chatpipeline

import (
	"context"
	"math"
	"strings"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

// lateInteractionScore computes a ColBERT-inspired late-interaction score
// between a query and a passage at token granularity.
//
// The full ColBERT model encodes each token into a dense vector and uses
// MaxSim over dot products. Here we provide a deterministic, CPU-only
// approximation that preserves the core intuition:
//
//   1. Tokenize query and passage into normalized term sets.
//   2. For every query term, find its best match inside the passage terms
//      (exact match > prefix match > substring match > no match).
//   3. Sum the per-term best scores and normalize by query length.
//
// This produces a fine-grained relevance signal in [0, 1] that is especially
// useful for long chunks where the embedding vector may dilute local matches.
//
// Design notes (section 2.4 of the retrieval optimization):
//   - Runs after the external rerank API, before MMR diversification.
//   - Only applied to the top-N candidates to keep latency low.
//   - Weight is controlled by the retrieval plan / tenant config.
func lateInteractionScore(query, passage string) float64 {
	// TokenizeSimple returns normalized term sets (map[string]struct{}).
	queryTokens := searchutil.TokenizeSimple(query)
	if len(queryTokens) == 0 {
		return 0
	}
	passageSet := searchutil.TokenizeSimple(passage)
	if len(passageSet) == 0 {
		return 0
	}

	var total float64
	for qt := range queryTokens {
		best := 0.0
		// 1. Exact match.
		if _, ok := passageSet[qt]; ok {
			best = 1.0
		} else {
			// 2. Prefix / substring match (cheaper than full edit distance).
			for pt := range passageSet {
				if score := tokenAffinity(qt, pt); score > best {
					best = score
				}
			}
		}
		total += best
	}

	// Normalize by query length so longer queries are not penalized.
	return total / float64(len(queryTokens))
}

// tokenAffinity returns a fine-grained match score between two tokens in
// [0, 1]. Exact equality is handled by the caller; this function only scores
// partial overlaps.
func tokenAffinity(queryTerm, passageTerm string) float64 {
	if queryTerm == passageTerm {
		return 1.0
	}
	ql, pl := len(queryTerm), len(passageTerm)
	if ql == 0 || pl == 0 {
		return 0
	}
	// Prefix match (e.g. "optim" vs "optimization").
	if strings.HasPrefix(passageTerm, queryTerm) || strings.HasPrefix(queryTerm, passageTerm) {
		shorter := math.Min(float64(ql), float64(pl))
		longer := math.Max(float64(ql), float64(pl))
		return 0.7 + 0.3*(shorter/longer)
	}
	// Substring match.
	if strings.Contains(passageTerm, queryTerm) || strings.Contains(queryTerm, passageTerm) {
		shorter := math.Min(float64(ql), float64(pl))
		longer := math.Max(float64(ql), float64(pl))
		return 0.5 + 0.3*(shorter/longer)
	}
	return 0
}

// applyLateInteraction re-scores the top-N reranked results with the
// late-interaction signal and blends it into the composite score.
//
// blendWeight controls how much the late-interaction score contributes to the
// final score. The remaining weight comes from the existing composite score
// (rerank model + retrieval base score).
func applyLateInteraction(
	ctx context.Context,
	query string,
	results []*types.SearchResult,
	topN int,
	blendWeight float64,
) {
	if topN <= 0 || blendWeight <= 0 || len(results) == 0 {
		return
	}
	if topN > len(results) {
		topN = len(results)
	}

	pipelineInfo(ctx, "LateInteraction", "start", map[string]interface{}{
		"top_n":        topN,
		"blend_weight": blendWeight,
	})

	for i := 0; i < topN; i++ {
		r := results[i]
		passage := getEnrichedPassage(ctx, r)
		if passage == "" {
			continue
		}
		liScore := lateInteractionScore(query, passage)
		base := r.Score
		r.Score = (1.0-blendWeight)*base + blendWeight*liScore
		r.Metadata = ensureMetadata(r.Metadata)
		r.Metadata["late_interaction_score"] = formatFloat(liScore)
		r.Metadata["late_interaction_blend"] = formatFloat(blendWeight)
	}

	pipelineInfo(ctx, "LateInteraction", "done", map[string]interface{}{
		"rescored": topN,
	})
}
