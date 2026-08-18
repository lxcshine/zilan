package chatpipeline

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/Tencent/WeKnora/internal/types"
)

// channelRankedList is a per-channel ranked result slice with an associated
// RRF weight. The fusion layer consumes these to produce a single blended list.
type channelRankedList struct {
	Channel string
	Weight  float64
	Results []*types.SearchResult
}

// fuseChannelsWithWeightedRRF merges multiple per-channel ranked lists into
// one list using weighted Reciprocal Rank Fusion.
//
// For each channel c with weight w_c and ranked list L_c, every document d
// receives score:
//
//	rrf(d) = Σ_c  w_c / (k + rank_c(d))
//
// where k is the RRF smoothing constant (default 60). Documents absent from a
// channel contribute 0 for that channel. The fused list is sorted by rrf desc.
//
// Design notes (section 2.4 of the retrieval optimization):
//   - Weights come from RetrievalPlan.ChannelWeights, which blends tenant
//     config, per-intent profiles and optional learned weights.
//   - Channels with weight <= 0 are skipped.
//   - Empty channels are ignored (they do not dilute the fusion).
//   - The same document appearing in multiple channels accumulates score
//     across all of them, which is exactly the desired behavior for
//     complementary recall paths (vector + keyword + HyDE + graph...).
func fuseChannelsWithWeightedRRF(
	ctx context.Context,
	lists []channelRankedList,
	k int,
) []*types.SearchResult {
	if k <= 0 {
		k = 60
	}

	// Accumulate fused scores per document ID.
	type fusedEntry struct {
		result *types.SearchResult
		score  float64
		// bestRank tracks the highest (smallest) rank across channels so we can
		// use it as a deterministic tie-breaker.
		bestRank int
	}
	fused := make(map[string]*fusedEntry)

	for _, lst := range lists {
		if lst.Weight <= 0 || len(lst.Results) == 0 {
			continue
		}
		for rank, r := range lst.Results {
			if r == nil || r.ID == "" {
				continue
			}
			entry, ok := fused[r.ID]
			if !ok {
				entry = &fusedEntry{result: r, bestRank: rank + 1}
				fused[r.ID] = entry
			}
			entry.score += lst.Weight / float64(k+rank+1)
			if rank+1 < entry.bestRank {
				entry.bestRank = rank + 1
			}
		}
	}

	if len(fused) == 0 {
		return nil
	}

	// Materialize and sort: fused score desc, then best rank asc, then original
	// retrieval score desc for stability.
	out := make([]*fusedEntry, 0, len(fused))
	for _, e := range fused {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].score-out[j].score) > 1e-12 {
			return out[i].score > out[j].score
		}
		if out[i].bestRank != out[j].bestRank {
			return out[i].bestRank < out[j].bestRank
		}
		return out[i].result.Score > out[j].result.Score
	})

	results := make([]*types.SearchResult, 0, len(out))
	for _, e := range out {
		// Preserve the fused score on the result so downstream stages (rerank,
		// filter_top_k) operate on the blended ranking signal.
		e.result.Score = e.score
		e.result.Metadata = ensureMetadata(e.result.Metadata)
		e.result.Metadata["fusion_score"] = formatFloat(e.score)
		e.result.Metadata["fusion_channels"] = collectChannelsForDoc(e.result.ID, lists)
		results = append(results, e.result)
	}
	return results
}

// collectChannelsForDoc returns a comma-separated list of channels that
// contributed to the fused score of the given document. Useful for debugging
// and observability.
func collectChannelsForDoc(docID string, lists []channelRankedList) string {
	seen := make(map[string]struct{})
	for _, lst := range lists {
		if lst.Weight <= 0 {
			continue
		}
		for _, r := range lst.Results {
			if r != nil && r.ID == docID {
				seen[lst.Channel] = struct{}{}
				break
			}
		}
	}
	channels := make([]string, 0, len(seen))
	for ch := range seen {
		channels = append(channels, ch)
	}
	sort.Strings(channels)
	return joinStrings(channels, ",")
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%.4f", v)
}

func joinStrings(parts []string, sep string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	n := len(sep) * (len(parts) - 1)
	for _, p := range parts {
		n += len(p)
	}
	buf := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, sep...)
		}
		buf = append(buf, p...)
	}
	return string(buf)
}
