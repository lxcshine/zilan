package types

import "context"

// RerankOutcome 描述重排阶段对一次请求实际做了什么，便于把“结果为空”
// 或“排序与预期不符”解释给调用方（FR-19③）。
type RerankOutcome string

const (
	// RerankOutcomeOK 表示有候选通过了阈值并按重排分排序。
	RerankOutcomeOK RerankOutcome = "ok"
	// RerankOutcomeThresholdDegraded 表示没有候选达到请求阈值，
	// 于是自动降级到一个更低的阈值。
	RerankOutcomeThresholdDegraded RerankOutcome = "threshold_degraded"
	// RerankOutcomeFallbackTop1 表示所有候选都未过阈值，只保留了最高分的一条。
	RerankOutcomeFallbackTop1 RerankOutcome = "fallback_top1"
	// RerankOutcomeAllBelowThreshold 表示模型拒绝了全部候选，结果为空。
	RerankOutcomeAllBelowThreshold RerankOutcome = "all_below_threshold"
	// RerankOutcomeModelError 表示重排模型调用失败，退回检索原始顺序。
	RerankOutcomeModelError RerankOutcome = "model_error"
	// RerankOutcomeModelUnavailable 表示配置的重排模型加载失败，退回检索原始顺序。
	RerankOutcomeModelUnavailable RerankOutcome = "model_unavailable"
	// RerankOutcomeNoModel 表示希望重排但租户没有可用重排模型，退回检索原始顺序。
	RerankOutcomeNoModel RerankOutcome = "no_model"
	// RerankOutcomeDisabled 表示调用方显式关闭了重排。
	RerankOutcomeDisabled RerankOutcome = "disabled"
	// RerankOutcomeNoCandidates 表示检索没有返回任何可重排的内容。
	RerankOutcomeNoCandidates RerankOutcome = "no_candidates"
)

// RerankDiagnostics 汇总一次重排的可观测信息。它同时也是检索链路“降级
// 但未失败”这类警告的落点：ChatManage 由其他改动独占，检索侧的诊断信息
// 统一挂在这里，避免为观测再加一个跨模块的可变状态。
type RerankDiagnostics struct {
	// Applied 为 true 表示返回顺序确实由重排分决定。
	Applied bool `json:"applied"`
	// Outcome 说明重排阶段走了哪条分支。
	Outcome RerankOutcome `json:"outcome"`
	// ModelID 是实际使用的重排模型 ID。
	ModelID string `json:"model_id,omitempty"`
	// Threshold 是请求的最低分；EffectiveThreshold 是降级后实际生效的最低分。
	Threshold          float64 `json:"threshold"`
	EffectiveThreshold float64 `json:"effective_threshold"`
	// TopScore 是候选中最高的模型分。
	TopScore float64 `json:"top_score"`
	// CandidateCount 是送入模型的候选数，ResultCount 是最终返回数。
	CandidateCount int `json:"candidate_count"`
	ResultCount    int `json:"result_count"`
	// TrimmedPassages 是被裁剪到模型长度上限的 passage 数。
	TrimmedPassages int `json:"trimmed_passages,omitempty"`
	// ImagesScored / ImagesKept 图片感知重排的归因字段（FR-17③）：
	// ImagesScored 为实际送入多模态重排模型的图片数；ImagesKept 为被
	// 文本重排器拒绝后凭向量分保留的图片命中数（KeptByImageVector）。
	ImagesScored int `json:"images_scored,omitempty"`
	ImagesKept   int `json:"images_kept,omitempty"`
	// Error 记录 model_error / model_unavailable 的原因。
	Error string `json:"error,omitempty"`
	// Warnings 收集检索链路“降级但可用”的告警，例如某个检索通道失败后
	// 仍用其余通道的结果继续。键为来源（如 channel:web_search），值为原因。
	Warnings []string `json:"warnings,omitempty"`
}

// RerankImageOptions 配置一次重排的图片感知行为（FR-17③）。零值表示
// 纯文本重排，行为与 FR-19 完全一致；仅当 LoadImage 与 ImageKeepScore
// 均非零时图片命中行才参与图片感知分支。
type RerankImageOptions struct {
	// LoadImage 读取 image_vector 命中行所指向图片的字节（data URI 的
	// 素材）。返回错误时该图片按“不可读”降级：跳过图片、仅用其
	// caption/OCR 文本打分，不阻断整批重排（§5.3）。
	LoadImage func(ctx context.Context, r *SearchResult) ([]byte, error)
	// ImageKeepScore 为正时，被文本重排器拒绝的图片命中行（pictorial，
	// 见 reranking.IsPictorialImageHit）只要 VectorScore 达到该值即保留
	// （至多 reranking.MaxKeptImages 条），并标记 MetadataKeptBy =
	// KeptByImageVector。参考上游取值为向量阈值加 0.05。
	ImageKeepScore float64
}
