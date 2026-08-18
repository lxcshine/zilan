package chatpipeline

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// failureSignals are the phrases that indicate the LLM could not answer
// from the retrieved context. When detected, the retrieval feedback loop
// marks the turn as a retrieval failure and adjusts future recall parameters.
var failureSignals = []string{
	"根据现有信息无法回答",
	"根据已有信息无法回答",
	"根据提供的信息无法回答",
	"无法回答该问题",
	"无法回答这个问题",
	"没有找到相关信息",
	"未找到相关信息",
	"没有相关信息",
	"知识库中没有",
	"知识库中未找到",
	"cannot answer",
	"could not find",
	"no relevant information",
	"insufficient information",
	"not enough information",
	"no information available",
}

// DetectRetrievalFailure inspects the final LLM response and reports whether
// it looks like a retrieval failure (the model explicitly refused to answer
// because the context did not contain the needed information).
//
// Exported so the service layer can run the check on the assembled answer
// when the streaming pipeline completes.
func DetectRetrievalFailure(response string) bool {
	if response == "" {
		return false
	}
	lower := strings.ToLower(response)
	for _, sig := range failureSignals {
		if strings.Contains(lower, strings.ToLower(sig)) {
			return true
		}
	}
	return false
}

// detectRetrievalFailure is the package-internal alias kept for pipeline code.
func detectRetrievalFailure(response string) bool {
	return DetectRetrievalFailure(response)
}

// ApplyRetrievalFeedback adjusts the tenant-level retrieval configuration
// in response to a retrieval failure using a bounded Hedge-style update:
//
//   - vector/keyword thresholds are lowered multiplicatively (floor 0.05)
//     to broaden recall on subsequent queries
//   - embedding top-k is increased additively (ceiling 200) to surface more
//     candidates for the reranker
//   - the channel that produced the fewest results receives a relative
//     weight boost (multiplicative Hedge update on the "*" wildcard row and
//     the failed intent row), then each row is renormalized so weights keep
//     their original total mass — repeated failures cannot push parameters
//     into degenerate ranges
//
// intent is the fine-grained retrieval intent of the failed turn (empty
// string when unknown); channelResults maps each retrieval channel to the
// ranked list it produced and may be nil when only the final answer text is
// available (soft failure). The input config is never mutated; the adjusted
// copy is returned and the caller decides whether to persist it.
//
// Exported for the service layer (session knowledge QA feedback loop).
func ApplyRetrievalFeedback(
	cfg *types.RetrievalConfig,
	intent string,
	channelResults map[string][]*types.SearchResult,
) *types.RetrievalConfig {
	if cfg == nil {
		cfg = &types.RetrievalConfig{}
	}
	out := cfg.Clone()
	eta := cfg.GetEffectiveFeedbackLearnRate()

	// 1. Lower thresholds to broaden recall.
	if out.VectorThreshold > 0.05 {
		out.VectorThreshold *= (1.0 - eta*0.33) // ~5% at the default rate
		if out.VectorThreshold < 0.05 {
			out.VectorThreshold = 0.05
		}
	}
	if out.KeywordThreshold > 0.05 {
		out.KeywordThreshold *= (1.0 - eta*0.33)
		if out.KeywordThreshold < 0.05 {
			out.KeywordThreshold = 0.05
		}
	}

	// 2. Increase top-k to surface more candidates.
	if out.EmbeddingTopK <= 0 {
		out.EmbeddingTopK = out.GetEffectiveEmbeddingTopK()
	}
	if out.EmbeddingTopK < 200 {
		out.EmbeddingTopK = int(float64(out.EmbeddingTopK)*(1.0+eta*0.67) + 0.5) // ~10% at default rate
		if out.EmbeddingTopK > 200 {
			out.EmbeddingTopK = 200
		}
	}

	// 3. Hedge update on channel weights: boost the weakest channel.
	if len(channelResults) > 0 {
		minChannel := ""
		minCount := int(^uint(0) >> 1) // max int
		for ch, res := range channelResults {
			if len(res) < minCount {
				minCount = len(res)
				minChannel = ch
			}
		}
		if minChannel != "" {
			if out.ChannelWeights == nil {
				out.ChannelWeights = make(map[string]map[string]float64)
			}
			// Wildcard row always exists conceptually; seed it from the
			// built-in defaults when missing so the update has a base.
			seedChannelWeightRow(out.ChannelWeights, "*")
			if intent != "" {
				seedChannelWeightRow(out.ChannelWeights, intent)
			}
			boostChannelWeight(out.ChannelWeights["*"], minChannel, eta)
			if intent != "" && intent != "*" {
				boostChannelWeight(out.ChannelWeights[intent], minChannel, eta)
			}
		}
	}
	return out
}

// applyRetrievalFeedback is the package-internal alias kept for pipeline code.
func applyRetrievalFeedback(
	cfg *types.RetrievalConfig,
	intent string,
	channelResults map[string][]*types.SearchResult,
) *types.RetrievalConfig {
	return ApplyRetrievalFeedback(cfg, intent, channelResults)
}

// seedChannelWeightRow ensures a channel-weight row exists, seeded from the
// built-in factual profile weights when absent.
func seedChannelWeightRow(rows map[string]map[string]float64, key string) {
	if _, ok := rows[key]; ok {
		return
	}
	seed := make(map[string]float64)
	for ch, w := range types.DefaultChannelWeights() {
		seed[ch] = w
	}
	rows[key] = seed
}

// boostChannelWeight applies the multiplicative Hedge boost to one channel
// inside a weight row and renormalizes the row to its original total mass.
func boostChannelWeight(row map[string]float64, channel string, eta float64) {
	if len(row) == 0 {
		return
	}
	total := 0.0
	for _, w := range row {
		total += w
	}
	if total <= 0 {
		return
	}
	if row[channel] <= 0 {
		// Channel absent from the row: give it a small absolute mass so the
		// multiplicative update can take hold on subsequent failures.
		row[channel] = total * eta * 0.1
	} else {
		row[channel] *= (1.0 + eta)
	}
	newTotal := 0.0
	for _, w := range row {
		newTotal += w
	}
	if newTotal <= 0 {
		return
	}
	scale := total / newTotal
	for ch := range row {
		row[ch] *= scale
	}
}

// recordRetrievalFeedback logs the failure event for observability and
// offline analysis. Persistence of the adjusted config is handled by the
// service layer (session knowledge QA), which owns the tenant repository.
func recordRetrievalFeedback(
	ctx context.Context,
	chatManage *types.ChatManage,
) {
	if chatManage == nil || !chatManage.RetrievalFailure {
		return
	}
	intent := ""
	if chatManage.Plan != nil {
		intent = string(chatManage.Plan.Intent)
	}
	pipelineWarn(ctx, "RetrievalFeedback", "failure_detected", map[string]interface{}{
		"session_id":    chatManage.SessionID,
		"query":         chatManage.Query,
		"rewrite_query": chatManage.RewriteQuery,
		"intent":        intent,
		"channel_count": len(chatManage.ChannelResults),
		"result_count":  len(chatManage.SearchResult),
	})
}
