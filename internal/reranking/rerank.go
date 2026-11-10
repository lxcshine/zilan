// Package reranking 是聊天流水线、Agent 检索工具与检索 API 共用的重排阶段：
// passage 构造、模型打分、阈值过滤与降级、综合打分、MMR 选择。
//
// 核心契约（FR-19③）：本包**绝不改写入参行**。所有带重排分的结果都是
// 入参 *types.SearchResult 的副本，原始检索分与原始 metadata 因此得以保留，
// 下游对 Content / Score / Metadata 的逐层覆盖不会毁掉“重排看到了什么”。
package reranking

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	// DefaultThreshold 是没有任何配置时的最低重排分，
	// 与 RetrievalConfig.GetEffectiveRerankThreshold 的默认值一致。
	DefaultThreshold = 0.2
	// DefaultFallbackMinScore 是“全部候选都没过阈值”时，最高分候选
	// 想要被保留所需达到的分数。低于它就宁可返回空，也不硬塞不相关内容。
	DefaultFallbackMinScore = 0.15

	// 高于 degradeFloor 的阈值若拒绝了全部候选，会按 degradeFactor 降一档
	// 重试一次，但不会低于 degradeFloor。
	degradeFloor  = 0.3
	degradeFactor = 0.7

	// DefaultMaxCandidates 限制送入重排模型的候选数。查询扩展、按文档/标签
	// 拆分检索都会各自贡献一整份召回列表，而每条候选都是一次计费 passage
	// 和一段延迟。
	DefaultMaxCandidates = 200

	// CompositeScoreKey 是 metadata 中保存“加 boost 之前的综合分”的键。
	// 后续 FAQ / wiki / memory 等 boost 会改写 Score，绝对阈值应当与本键比较。
	CompositeScoreKey = "composite_score"
)

// Candidates 是一次重排的输入：非空 passage 的候选行、它们在在原始切片中的
// 下标，以及模型实际打分的文本。三者按下标严格对齐。
type Candidates struct {
	// Rows 是参与重排的原始行指针（只读，不会被修改）。
	Rows []*types.SearchResult
	// Indices[i] 是 Rows[i] 在调用方输入切片中的位置。
	Indices []int
	// Passages[i] 是送给模型的文本，即 Rows[i] 的重排原文片段。
	Passages []string
}

// Len 返回候选数。
func (c *Candidates) Len() int {
	if c == nil {
		return 0
	}
	return len(c.Rows)
}

// BuildCandidates 为 results 构造模型输入：跳过空行与清洗后为空的 passage，
// 并在候选数超过 maxCandidates 时只保留检索分最高的那一批（maxCandidates
// 非正数表示不设上限）。入参行只读，不做任何修改。
func BuildCandidates(ctx context.Context, results []*types.SearchResult, maxCandidates int) *Candidates {
	keep := topByScore(results, maxCandidates)
	out := &Candidates{
		Rows:     make([]*types.SearchResult, 0, len(results)),
		Indices:  make([]int, 0, len(results)),
		Passages: make([]string, 0, len(results)),
	}
	for i, r := range results {
		if r == nil || (keep != nil && !keep[i]) {
			continue
		}
		passage := ModelPassage(ctx, r)
		if strings.TrimSpace(passage) == "" {
			continue
		}
		out.Rows = append(out.Rows, r)
		out.Indices = append(out.Indices, i)
		out.Passages = append(out.Passages, passage)
	}
	return out
}

// topByScore 返回检索分最高的 limit 行的下标集合；limit 非正数或全部行都
// 放得下时返回 nil（表示不做裁剪）。同分保留靠前的行。
func topByScore(results []*types.SearchResult, limit int) map[int]bool {
	if limit <= 0 || len(results) <= limit {
		return nil
	}
	order := make([]int, 0, len(results))
	for i, r := range results {
		if r != nil {
			order = append(order, i)
		}
	}
	if len(order) <= limit {
		return nil
	}
	sort.SliceStable(order, func(a, b int) bool { return results[order[a]].Score > results[order[b]].Score })
	keep := make(map[int]bool, limit)
	for _, i := range order[:limit] {
		keep[i] = true
	}
	return keep
}

// ValidScores 丢弃下标越界的模型返回项，使后续逻辑不必反复判界。
func ValidScores(scores []rerank.RankResult, candidateCount int) []rerank.RankResult {
	out := make([]rerank.RankResult, 0, len(scores))
	for _, s := range scores {
		if s.Index >= 0 && s.Index < candidateCount {
			out = append(out, s)
		}
	}
	return out
}

// ApplyThreshold 保留分数不低于 threshold 的候选。若一条都没有，高于
// degradeFloor 的阈值会降一档重试；仍为空时，最高分候选在达到 fallbackMin
// 的前提下被单独保留。整个过程只对已有的分数重新过滤，不会二次调用模型。
func ApplyThreshold(
	scores []rerank.RankResult,
	threshold, fallbackMin float64,
) ([]rerank.RankResult, types.RerankOutcome, float64) {
	effective := threshold
	if passing := filterScores(scores, effective); len(passing) > 0 {
		return passing, types.RerankOutcomeOK, effective
	}
	if effective > degradeFloor {
		effective = math.Max(effective*degradeFactor, degradeFloor)
		if passing := filterScores(scores, effective); len(passing) > 0 {
			return passing, types.RerankOutcomeThresholdDegraded, effective
		}
	}
	if top, ok := BestScore(scores); ok && top.RelevanceScore >= fallbackMin {
		return []rerank.RankResult{top}, types.RerankOutcomeFallbackTop1, effective
	}
	return nil, types.RerankOutcomeAllBelowThreshold, effective
}

func filterScores(scores []rerank.RankResult, threshold float64) []rerank.RankResult {
	var out []rerank.RankResult
	for _, s := range scores {
		if s.RelevanceScore >= threshold {
			out = append(out, s)
		}
	}
	return out
}

// BestScore 返回最高分。厂商通常按分数有序返回，但没有任何保证。
func BestScore(scores []rerank.RankResult) (rerank.RankResult, bool) {
	if len(scores) == 0 {
		return rerank.RankResult{}, false
	}
	top := scores[0]
	for _, s := range scores[1:] {
		if s.RelevanceScore > top.RelevanceScore {
			top = s
		}
	}
	return top, true
}

// ScoredCopy 返回 r 的**副本**：Score 换成模型分与检索分的综合分，
// base_score / model_score / composite_score 写入副本自己的 metadata，
// passage 记到副本的 RerankPassage 上作为重排溯源。入参 r 不被修改。
//
// faqBoost > 1 时 FAQ 行的综合分会被放大并以 1.0 封顶，保证分数留在
// [0, 1] 区间；封顶后同分的 FAQ 之间靠 PreBoostScore 保持原有相对顺序。
func ScoredCopy(r *types.SearchResult, modelScore, faqBoost float64, passage string) *types.SearchResult {
	if r == nil {
		return nil
	}
	c := *r
	c.Metadata = make(map[string]string, len(r.Metadata)+4)
	for k, v := range r.Metadata {
		c.Metadata[k] = v
	}
	base := r.Score
	c.Metadata["base_score"] = strconv.FormatFloat(base, 'f', 4, 64)
	c.Metadata["model_score"] = strconv.FormatFloat(modelScore, 'f', 4, 64)
	c.Score = CompositeScore(&c, modelScore, base)
	c.Metadata[CompositeScoreKey] = strconv.FormatFloat(c.Score, 'f', 4, 64)
	c.RerankPassage = passage
	if faqBoost > 1.0 && c.ChunkType == string(types.ChunkTypeFAQ) {
		c.Metadata["faq_boosted"] = "true"
		c.Metadata["faq_original_score"] = strconv.FormatFloat(c.Score, 'f', 4, 64)
		c.Score = math.Min(c.Score*faqBoost, 1.0)
	}
	return &c
}

// CompositeScore 把重排模型分与检索分按固定权重融合到 [0, 1]，并叠加一个
// 很轻的位置先验（文档靠前的分块略微加分）。图谱命中没有检索分（来自实体
// 查询而非相似度检索），用模型分代替，而不是塞一个臆造常数。
func CompositeScore(r *types.SearchResult, modelScore, baseScore float64) float64 {
	sourceWeight := 1.0
	if r != nil && strings.EqualFold(r.KnowledgeSource, "web_search") {
		sourceWeight = 0.95
	}
	if r != nil && r.MatchType == types.MatchTypeGraph {
		baseScore = modelScore
	}
	composite := 0.6*modelScore + 0.3*baseScore + 0.1*sourceWeight
	if r != nil && r.StartAt >= 0 {
		positionPrior := 1.0 + searchutil.ClampFloat(
			1.0-float64(r.StartAt)/float64(r.EndAt+1), -0.05, 0.05)
		composite *= positionPrior
	}
	return math.Min(math.Max(composite, 0), 1)
}

// PreBoostScore 返回与绝对阈值（例如 FAQ 直答阈值）比较时应当使用的分数：
// 重排过的行返回加 boost 之前的综合分，其余行返回自身 Score。
func PreBoostScore(r *types.SearchResult) float64 {
	if r == nil {
		return 0
	}
	if v, ok := r.Metadata[CompositeScoreKey]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return r.Score
}

// FallbackMinScore 返回“阈值拒绝了全部候选”时最高分候选的保留门槛。
// 调用方显式把检索限定到某个标签/文档范围时，宁可保留它最好的一条，
// 也不要让一个全局阈值把整个权威范围清空。
func FallbackMinScore(explicitScope bool) float64 {
	if explicitScope {
		return 0
	}
	return DefaultFallbackMinScore
}
