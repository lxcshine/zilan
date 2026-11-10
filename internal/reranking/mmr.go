package reranking

import (
	"context"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

// SelectMMR 用最大边际相关（MMR）在“相关性”和“多样性”之间做权衡选择，
// 返回被选中的行（沿用入参指针，不复制也不修改）。
//
// lambda 越接近 1 越看重相关性，越接近 0 越看重去冗余。k 非正数或入参为空
// 时返回 nil。
func SelectMMR(
	ctx context.Context,
	results []*types.SearchResult,
	k int,
	lambda float64,
) []*types.SearchResult {
	if k <= 0 || len(results) == 0 {
		return nil
	}

	// 并行预计算 token 集合（纯 CPU 的分词），避免在 O(k*n) 的选择循环里
	// 反复分词。
	allTokenSets := tokenizeAll(ctx, results)

	selected := make([]*types.SearchResult, 0, k)
	selectedTokenSets := make([]map[string]struct{}, 0, k)
	selectedIndices := make(map[int]struct{})

	for len(selected) < k && len(selectedIndices) < len(results) {
		bestIdx := -1
		bestScore := -1.0

		for i, r := range results {
			if r == nil {
				continue
			}
			if _, isSelected := selectedIndices[i]; isSelected {
				continue
			}

			redundancy := 0.0
			for _, selTokens := range selectedTokenSets {
				if sim := searchutil.Jaccard(allTokenSets[i], selTokens); sim > redundancy {
					redundancy = sim
				}
			}

			mmr := lambda*r.Score - (1.0-lambda)*redundancy
			if mmr > bestScore {
				bestScore = mmr
				bestIdx = i
			}
		}

		if bestIdx < 0 {
			break
		}

		selected = append(selected, results[bestIdx])
		selectedTokenSets = append(selectedTokenSets, allTokenSets[bestIdx])
		selectedIndices[bestIdx] = struct{}{}
	}

	logger.Debugf(ctx, "[Rerank][MMR] selected %d/%d rows, lambda=%.2f, avg_redundancy=%.4f",
		len(selected), len(results), lambda, avgRedundancy(selectedTokenSets))
	return selected
}

// tokenizeAll 并行分词，返回与 results 等长、下标对齐的 token 集合。
func tokenizeAll(ctx context.Context, results []*types.SearchResult) []map[string]struct{} {
	sets := make([]map[string]struct{}, len(results))
	var wg sync.WaitGroup
	for i := range results {
		r := results[i]
		if r == nil {
			sets[i] = nil
			continue
		}
		wg.Add(1)
		go func(idx int, row *types.SearchResult) {
			defer wg.Done()
			sets[idx] = searchutil.TokenizeSimple(passageText(row))
		}(i, r)
	}
	wg.Wait()
	return sets
}

// passageText 优先使用重排阶段真正看到的片段（副本上的 RerankPassage），
// 保证 MMR 的冗余度量与模型打分依据一致；没有该字段时回落到实时构造。
func passageText(r *types.SearchResult) string {
	if r == nil {
		return ""
	}
	if p := strings.TrimSpace(r.RerankPassage); p != "" {
		return p
	}
	return ModelPassage(context.Background(), r)
}

// avgRedundancy 计算被选中行之间的平均 Jaccard 冗余度，仅用于观测。
func avgRedundancy(sets []map[string]struct{}) float64 {
	if len(sets) <= 1 {
		return 0
	}
	sum, pairs := 0.0, 0
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			sum += searchutil.Jaccard(sets[i], sets[j])
			pairs++
		}
	}
	if pairs == 0 {
		return 0
	}
	return sum / float64(pairs)
}
