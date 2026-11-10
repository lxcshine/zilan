package reranking

import (
	"context"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// LogOutcome 把一次重排的诊断信息落成一条结构化日志。重排阶段“降级但可用”
// 的分支（阈值降级、只保留 top1、模型失败退回检索顺序）都不该是静默的，
// 否则线上只能看到结果变差而看不到原因。
func LogOutcome(ctx context.Context, d types.RerankDiagnostics) {
	switch d.Outcome {
	case types.RerankOutcomeModelError, types.RerankOutcomeModelUnavailable,
		types.RerankOutcomeThresholdDegraded, types.RerankOutcomeFallbackTop1,
		types.RerankOutcomeAllBelowThreshold:
		fields := logger.Fields{
			"outcome":             string(d.Outcome),
			"applied":             d.Applied,
			"candidate_count":     d.CandidateCount,
			"result_count":        d.ResultCount,
			"threshold":           d.Threshold,
			"effective_threshold": d.EffectiveThreshold,
			"top_score":           d.TopScore,
			"trimmed_passages":    d.TrimmedPassages,
		}
		if d.ModelID != "" {
			fields["model_id"] = d.ModelID
		}
		if d.Error != "" {
			fields["error"] = d.Error
		}
		logger.WarnWithFields(ctx, fields, "rerank degraded: results are not fully reranked")
	default:
		logger.Infof(ctx, "[Rerank] completed: outcome=%s applied=%v candidates=%d results=%d "+
			"threshold=%.3f effective=%.3f top=%.4f trimmed_passages=%d",
			d.Outcome, d.Applied, d.CandidateCount, d.ResultCount,
			d.Threshold, d.EffectiveThreshold, d.TopScore, d.TrimmedPassages)
	}
	for _, w := range d.Warnings {
		logger.Warnf(ctx, "[Rerank] %s", w)
	}
}
