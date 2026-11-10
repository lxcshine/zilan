package chatpipeline

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/reranking"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// PluginRerank implements reranking functionality for chat pipeline
type PluginRerank struct {
	modelService interfaces.ModelService // Service to access rerank models
	kbService    interfaces.KnowledgeBaseService // Service to read images for image-aware rerank (FR-17③)
}

// NewPluginRerank creates a new rerank plugin instance.
// kbService 供 FR-17 图片感知重排读取 image_vector 行指向的图片字节，
// 为 nil 时自动退回纯文本重排（§5.3）。
func NewPluginRerank(eventManager *EventManager, modelService interfaces.ModelService, kbService interfaces.KnowledgeBaseService) *PluginRerank {
	res := &PluginRerank{
		modelService: modelService,
		kbService:    kbService,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginRerank) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHUNK_RERANK}
}

// OnEvent handles reranking events in the chat pipeline.
//
// 重排阶段的所有打分都写在**副本**上（见 internal/reranking）：
// chatManage.SearchResult 里的原始行保留原始检索分与原始 metadata，
// chatManage.RerankResult 拿到的是带综合分的副本，副本上还记录了
// “模型实际看到的原文片段”（RerankPassage），因此下游对 Content 的
// 逐层覆盖不会再毁掉重排依据（FR-19③）。
func (p *PluginRerank) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	if !chatManage.NeedsRetrieval() {
		return next()
	}
	pipelineInfo(ctx, "Rerank", "input", map[string]interface{}{
		"session_id":    chatManage.SessionID,
		"candidate_cnt": len(chatManage.SearchResult),
		"rerank_model":  chatManage.RerankModelID,
		"rerank_thresh": chatManage.RerankThreshold,
		"rewrite_query": chatManage.RewriteQuery,
	})
	if len(chatManage.SearchResult) == 0 {
		pipelineInfo(ctx, "Rerank", "skip", map[string]interface{}{
			"reason": "empty_search_result",
		})
		return next()
	}
	if chatManage.RerankModelID == "" {
		pipelineWarn(ctx, "Rerank", "skip", map[string]interface{}{
			"reason": "empty_model_id",
		})
		return next()
	}

	// Get rerank model from service
	rerankModel, err := p.modelService.GetRerankModel(ctx, chatManage.RerankModelID)
	if err != nil {
		pipelineError(ctx, "Rerank", "get_model", map[string]interface{}{
			"model_id": chatManage.RerankModelID,
			"error":    err.Error(),
		})
		return ErrGetRerankModel.WithError(err)
	}

	// 构造模型输入：清洗后的 passage + 与之严格对齐的候选行。
	// 入参行只读；空 passage 的行被跳过（不进模型、也不进结果）。
	candidates := reranking.BuildCandidates(ctx, chatManage.SearchResult, reranking.DefaultMaxCandidates)
	skipped := len(chatManage.SearchResult) - candidates.Len()
	if skipped > 0 {
		pipelineInfo(ctx, "Rerank", "empty_passage_skip", map[string]interface{}{
			"skipped_cnt": skipped,
		})
	}

	passagesPreview := langfuse.SummarizePassagePreviews(candidates.Rows, candidates.Passages, 25)
	rerankCtx, rerankSpan := langfuse.GetManager().StartSpan(ctx, langfuse.SpanOptions{
		Name: "rerank",
		Input: map[string]interface{}{
			"query":            chatManage.RewriteQuery,
			"candidate_count":  candidates.Len(),
			"rerank_model_id":  chatManage.RerankModelID,
			"threshold":        chatManage.RerankThreshold,
			"rerank_top_k":     chatManage.RerankTopK,
			"faq_priority":     chatManage.FAQPriorityEnabled,
			"faq_score_boost":  chatManage.FAQScoreBoost,
			"passages_preview": passagesPreview,
		},
		Metadata: map[string]interface{}{
			"session_id": chatManage.SessionID,
		},
	})
	ctx = rerankCtx
	spanOutput := map[string]interface{}{}
	var spanErr error
	defer func() {
		rerankSpan.Finish(spanOutput, nil, spanErr)
	}()

	pipelineInfo(ctx, "Rerank", "build_passages", map[string]interface{}{
		"total_cnt":     len(chatManage.SearchResult),
		"candidate_cnt": candidates.Len(),
	})

	diag := types.RerankDiagnostics{
		ModelID:            chatManage.RerankModelID,
		Threshold:          chatManage.RerankThreshold,
		EffectiveThreshold: chatManage.RerankThreshold,
		CandidateCount:     candidates.Len(),
	}

	logRerankInputScoreSample(ctx, chatManage.SearchResult)

	var scored []*types.SearchResult
	// 模型原始打分只在本阶段与 langfuse span 之间传递，不进 ChatManage
	// （ChatManage 由其他改动独占），观测统一走 reranking.LogOutcome + span。
	var modelScores []rerank.RankResult

	if candidates.Len() == 0 {
		diag.Outcome = types.RerankOutcomeNoCandidates
	} else {
		// 按模型声明的 passage 长度上限裁剪，避免一条超长候选把整批请求打挂。
		diag.TrimmedPassages = reranking.FitPassages(ctx, candidates.Passages, rerank.MaxPassageRunes(rerankModel))

		pipelineInfo(ctx, "Rerank", "model_call", map[string]interface{}{
			"query_variant": chatManage.RewriteQuery,
			"passages":      candidates.Len(),
		})
		// FR-17③ 图片感知重排：image_vector 命中行连同图片一起送入支持
		// 多模态的重排模型；模型/读图/预处理任一环节不可用都自动退回纯
		// 文本打分（§5.3）。归因写入 diag.ImagesScored。
		imageOpts := p.imageRerankOptions()
		resp, modelErr := reranking.RerankCandidates(ctx, rerankModel, chatManage.RewriteQuery, candidates, imageOpts, &diag)
		if modelErr != nil {
			// 重排 API 失败：退回检索原始顺序，让流水线仍能给出可用结果。
			// 这里交给下游的是**原始行指针**，不做任何改写。
			pipelineWarn(ctx, "Rerank", "api_error_fallback", map[string]interface{}{
				"error":         modelErr.Error(),
				"candidate_cnt": candidates.Len(),
			})
			diag.Outcome = types.RerankOutcomeModelError
			diag.Error = modelErr.Error()
			diag.ResultCount = len(candidates.Rows)
			chatManage.SearchResult = candidates.Rows
			reranking.LogOutcome(ctx, diag)
			spanOutput = map[string]interface{}{
				"stage":           "api_error_fallback",
				"candidate_count": candidates.Len(),
				"error":           modelErr.Error(),
			}
			return next()
		}

		modelScores = reranking.ValidScores(resp, candidates.Len())
		logRerankTopScores(ctx, modelScores, candidates.Rows)
		diag.Applied = true
		if top, ok := reranking.BestScore(modelScores); ok {
			diag.TopScore = top.RelevanceScore
		}

		fallbackMin := reranking.FallbackMinScore(chatManage.SearchTargets.HasRecallThresholdOverride())
		passing, outcome, effective := reranking.ApplyThreshold(modelScores, chatManage.RerankThreshold, fallbackMin)
		diag.Outcome = outcome
		diag.EffectiveThreshold = effective
		pipelineInfo(ctx, "Rerank", "threshold", map[string]interface{}{
			"threshold":           chatManage.RerankThreshold,
			"effective_threshold": effective,
			"outcome":             string(outcome),
			"passing_cnt":         len(passing),
		})

		faqBoost := 1.0
		if chatManage.FAQPriorityEnabled && chatManage.FAQScoreBoost > 1.0 {
			faqBoost = chatManage.FAQScoreBoost
		}
		scored = make([]*types.SearchResult, 0, len(passing))
		for _, rr := range passing {
			copyRow := reranking.ScoredCopy(
				candidates.Rows[rr.Index], rr.RelevanceScore, faqBoost, candidates.Passages[rr.Index])
			if faqBoost > 1.0 && copyRow.Metadata["faq_boosted"] == "true" {
				pipelineInfo(ctx, "Rerank", "faq_boost", map[string]interface{}{
					"chunk_id":       copyRow.ID,
					"original_score": copyRow.Metadata["faq_original_score"],
					"boosted_score":  fmt.Sprintf("%.4f", copyRow.Score),
					"boost_factor":   faqBoost,
				})
			}
			scored = append(scored, copyRow)
		}

		// FR-17③ 图片兜底保留：被文本重排器拒绝、但向量分达标的图片命中行
		// 以副本追加回来（带 KeptByImageVector 标记，归因 diag.ImagesKept）。
		if imageOpts.ImageKeepScore > 0 {
			scored = reranking.KeepImages(ctx, candidates, scored, imageOpts.ImageKeepScore, &diag)
		}
	}

	pipelineInfo(ctx, "Rerank", "model_response", map[string]interface{}{
		"result_cnt": len(scored),
	})

	// Late-interaction re-scoring (ColBERT-style token granularity) runs on
	// the top candidates after the external rerank model and before MMR.
	// 作用对象是副本，原始检索分不受影响。
	if cfg := retrievalConfigFromContext(ctx); cfg != nil && cfg.IsLateInteractionEnabled() {
		applyLateInteraction(ctx, chatManage.RewriteQuery, scored, min(len(scored), 50), 0.25)
	}

	mmrLambda := types.DefaultMMRLambda
	if chatManage.Plan != nil && chatManage.Plan.MMRLambda > 0 {
		mmrLambda = chatManage.Plan.MMRLambda
	}
	pipelineInfo(ctx, "Rerank", "mmr_start", map[string]interface{}{
		"lambda":     mmrLambda,
		"k":          min(len(scored), max(1, chatManage.RerankTopK)),
		"candidates": len(scored),
	})
	final := reranking.SelectMMR(ctx, scored, min(len(scored), max(1, chatManage.RerankTopK)), mmrLambda)
	// FR-17③：MMR/top-k 只看文本相关性，显式保留的图片命中行（KeptBy=
	// image_vector）被裁掉时补回尾部（上游 TopKKeepingKept 语义）。
	final = reranking.RestoreKeptImages(final, scored)
	chatManage.RerankResult = final

	// Log composite top scores for debugging
	for i := 0; i < min(3, len(scored)); i++ {
		pipelineInfo(ctx, "Rerank", "composite_top", map[string]interface{}{
			"rank":        i + 1,
			"chunk_id":    scored[i].ID,
			"base_score":  scored[i].Metadata["base_score"],
			"final_score": fmt.Sprintf("%.4f", scored[i].Score),
		})
	}

	diag.ResultCount = len(chatManage.RerankResult)
	reranking.LogOutcome(ctx, diag)

	if len(chatManage.RerankResult) == 0 {
		pipelineWarn(ctx, "Rerank", "output", map[string]interface{}{
			"filtered_cnt": 0,
		})
		spanOutput = buildRerankSpanOutput(
			candidates,
			modelScores,
			scored,
			nil,
			chatManage,
			diag,
		)
		return ErrSearchNothing
	}

	spanOutput = buildRerankSpanOutput(
		candidates,
		modelScores,
		scored,
		chatManage.RerankResult,
		chatManage,
		diag,
	)
	pipelineInfo(ctx, "Rerank", "output", map[string]interface{}{
		"filtered_cnt": len(chatManage.RerankResult),
	})
	return next()
}

// imageRerankOptions 构造 FR-17 图片感知重排选项：LoadImage 从知识库服务
// 读取 image_vector 行指向的图片（经鸭子类型断言获得，避免把读图能力
// 塞进 KnowledgeBaseService 大接口进而传染 repository 实现）；ImageKeepScore
// 取图片腿阈值（0.4，与 service.ImageVectorThreshold 一致）加 0.05。
// kbService 为 nil 或未实现 ReadChunkImage（单测桩等）时选项退化为零值，
// 行为与纯文本重排完全一致。
func (p *PluginRerank) imageRerankOptions() types.RerankImageOptions {
	if p.kbService == nil {
		return types.RerankImageOptions{}
	}
	reader, ok := p.kbService.(interface {
		ReadChunkImage(ctx context.Context, r *types.SearchResult) ([]byte, error)
	})
	if !ok {
		return types.RerankImageOptions{}
	}
	return types.RerankImageOptions{
		LoadImage: func(ctx context.Context, r *types.SearchResult) ([]byte, error) {
			return reader.ReadChunkImage(ctx, r)
		},
		ImageKeepScore: reranking.ImageKeepScoreFor(0.4),
	}
}

func buildRerankSpanOutput(
	candidates *reranking.Candidates,
	modelScores []rerank.RankResult,
	composite []*types.SearchResult,
	final []*types.SearchResult,
	chatManage *types.ChatManage,
	diag types.RerankDiagnostics,
) map[string]interface{} {
	modelRows := make([]map[string]interface{}, 0, len(modelScores))
	for i, rr := range modelScores {
		row := map[string]interface{}{
			"rank":        i + 1,
			"index":       rr.Index,
			"model_score": rr.RelevanceScore,
		}
		if rr.Index >= 0 && rr.Index < candidates.Len() {
			row["chunk_id"] = candidates.Rows[rr.Index].ID
			row["knowledge_id"] = candidates.Rows[rr.Index].KnowledgeID
			row["knowledge_title"] = candidates.Rows[rr.Index].KnowledgeTitle
			row["match_type"] = candidates.Rows[rr.Index].MatchType
			row["retrieval_score"] = candidates.Rows[rr.Index].Score
			if rr.Index < len(candidates.Passages) {
				row["preview"] = langfuse.TruncateRunes(candidates.Passages[rr.Index], 160)
			}
		}
		modelRows = append(modelRows, row)
	}

	out := map[string]interface{}{
		"candidate_count":    candidates.Len(),
		"model_result_count": len(modelScores),
		"composite_count":    len(composite),
		"final_count":        len(final),
		"threshold":          chatManage.RerankThreshold,
		"rerank_top_k":       chatManage.RerankTopK,
		"threshold_degraded": diag.Outcome == types.RerankOutcomeThresholdDegraded,
		"outcome":            string(diag.Outcome),
		"model_scores":       langfuse.SummarizeRankScores(modelRows, 50),
		"composite_results":  langfuse.SummarizeSearchResults(composite, 25),
		"final_results":      langfuse.SummarizeSearchResults(final, 25),
	}
	if len(modelScores) > 50 {
		out["model_scores_truncated"] = len(modelScores) - 50
	}
	return out
}

// ensureMetadata ensures the metadata is not nil
func ensureMetadata(m map[string]string) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	return m
}

// logRerankTopScores 打印模型返回的前若干条分数，便于线上定位阈值问题。
func logRerankTopScores(ctx context.Context, scores []rerank.RankResult, candidates []*types.SearchResult) {
	logged := min(5, len(scores))
	for i := range logged {
		if scores[i].Index < len(candidates) {
			pipelineInfo(ctx, "Rerank", "top_score", map[string]interface{}{
				"rank":        i + 1,
				"score":       scores[i].RelevanceScore,
				"chunk_id":    candidates[scores[i].Index].ID,
				"match_type":  candidates[scores[i].Index].MatchType,
				"chunk_type":  candidates[scores[i].Index].ChunkType,
				"content_len": len(candidates[scores[i].Index].Content),
			})
		}
	}
	if len(scores) > logged {
		pipelineInfo(ctx, "Rerank", "top_score_summary", map[string]interface{}{
			"total":     len(scores),
			"logged":    logged,
			"truncated": len(scores) - logged,
		})
	}
}

// cleanPassageForRerank 是 reranking.CleanPassage 的包内别名：清洗规则已上移
// 到共享包，聊天流水线与既有单测继续沿用这个入口。
func cleanPassageForRerank(text string) string {
	return reranking.CleanPassage(text)
}

// getEnrichedPassage 是 reranking.ModelPassage 的包内别名（只读投影）。
func getEnrichedPassage(ctx context.Context, result *types.SearchResult) string {
	return reranking.ModelPassage(ctx, result)
}

// rerankFallbackMinScore 返回“阈值拒绝了全部候选”时最高分候选的保留门槛。
func rerankFallbackMinScore(searchTargets types.SearchTargets) float64 {
	return reranking.FallbackMinScore(searchTargets.HasRecallThresholdOverride())
}

func logRerankInputScoreSample(ctx context.Context, results []*types.SearchResult) {
	const maxLogRows = 8
	limit := min(maxLogRows, len(results))
	for i := 0; i < limit; i++ {
		sr := results[i]
		pipelineInfo(ctx, "Rerank", "input_score", map[string]interface{}{
			"index":      i,
			"chunk_id":   sr.ID,
			"score":      fmt.Sprintf("%.4f", sr.Score),
			"match_type": sr.MatchType,
		})
	}
	if len(results) > limit {
		pipelineInfo(ctx, "Rerank", "input_score_summary", map[string]interface{}{
			"total":     len(results),
			"logged":    limit,
			"truncated": len(results) - limit,
		})
	}
}
