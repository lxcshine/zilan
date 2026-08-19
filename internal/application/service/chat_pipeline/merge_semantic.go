package chatpipeline

import (
	"context"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
)

// semanticDedup removes near-duplicate chunks by embedding cosine similarity
// (smart mode only). String-overlap dedup upstream cannot catch paraphrased
// duplicates retrieved via different channels (multi-query / HyDE / step-back
// fusion makes these common). Chunks are grouped by knowledge base so each
// group is embedded with the same model that indexed it; failures are
// best-effort — the group keeps its original chunks on any error.
func (p *PluginMerge) semanticDedup(ctx context.Context, chunks []*types.SearchResult) []*types.SearchResult {
	if len(chunks) < 2 || p.modelService == nil || p.kbService == nil {
		return chunks
	}

	// Group chunk indexes by knowledge base (embedding space is per-KB).
	groups := map[string][]int{}
	var kbOrder []string
	for i, c := range chunks {
		if c == nil {
			continue
		}
		kbID := c.KnowledgeBaseID
		if _, ok := groups[kbID]; !ok {
			kbOrder = append(kbOrder, kbID)
		}
		groups[kbID] = append(groups[kbID], i)
	}

	dropped := map[int]bool{}
	for _, kbID := range kbOrder {
		idxs := groups[kbID]
		if len(idxs) < 2 {
			continue
		}
		embedder := p.resolveEmbedder(ctx, kbID)
		if embedder == nil {
			continue
		}

		tiers := make([]*contextx.TierResult, len(idxs))
		for j, i := range idxs {
			tiers[j] = &contextx.TierResult{
				ID:      chunks[i].ID,
				Content: chunks[i].Content,
				Score:   chunks[i].Score,
			}
		}
		kept, err := contextx.SemanticDedup(ctx, tiers, contextx.DefaultSemanticDedupConfig(), embedder)
		if err != nil {
			pipelineWarn(ctx, "Merge", "semantic_dedup_failed", map[string]interface{}{
				"knowledge_base_id": kbID,
				"error":             err.Error(),
			})
			continue
		}
		keptPtr := map[*contextx.TierResult]bool{}
		for _, k := range kept {
			keptPtr[k] = true
		}
		for j, t := range tiers {
			if !keptPtr[t] {
				dropped[idxs[j]] = true
			}
		}
	}

	if len(dropped) == 0 {
		return chunks
	}
	out := make([]*types.SearchResult, 0, len(chunks)-len(dropped))
	for i, c := range chunks {
		if !dropped[i] {
			out = append(out, c)
		}
	}
	pipelineInfo(ctx, "Merge", "semantic_dedup", map[string]interface{}{
		"before": len(chunks),
		"after":  len(out),
	})
	return out
}

// resolveEmbedder returns the embedding model configured for the knowledge
// base. Returns nil (dedup skipped) when the KB or model cannot be resolved.
func (p *PluginMerge) resolveEmbedder(ctx context.Context, kbID string) embedding.Embedder {
	if kbID == "" {
		return nil
	}
	kb, err := p.kbService.GetKnowledgeBaseByID(ctx, kbID)
	if err != nil || kb == nil || kb.EmbeddingModelID == "" {
		return nil
	}
	embedder, err := p.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		return nil
	}
	return embedder
}
