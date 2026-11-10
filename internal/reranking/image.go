// image.go —— 重排阶段的图片感知（FR-17③：图片参与重排）。
//
// 照抄上游 WeKnora v0.8 internal/reranking/image.go 的核心逻辑，适配 Zilan
// FR-19 的分步重排 API（BuildCandidates → Rerank → ApplyThreshold →
// ScoredCopy）：
//   - RerankCandidates：多模态重排模型可用且调用方提供 LoadImage 时，
//     image_vector 命中行连同图片一起送入模型（文本候选照常）；否则整批
//     走纯文本重排——模型不支持图片 / 读图失败 / 图片超限均只降级该图，
//     绝不阻断文本重排（§5.3）；
//   - KeepImages：文本重排器拒绝的图片命中行，只要向量分达到
//     ImageKeepScore 就保留（至多 MaxKeptImages 条），并在副本 metadata 里
//     标记 MetadataKeptBy=KeptByImageVector——文本重排器只看 caption，
//     “查询词与图片内容强相关但 caption 措辞不同”的高分图片行不能因为
//     文本分低而全军覆没；
//   - diag.ImagesScored / diag.ImagesKept 填实归因（FR-19④）。
//
// 与包契约一致：本文件绝不改写入参行，保留行都是 ScoredCopy 副本。
package reranking

import (
	"context"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/imageprep"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
)

// MaxKeptImages 是“被文本重排器拒绝后凭向量分兜底保留”的图片行上限
// （上游同值）：兜底保留是对文本信号的补偿，不是第二检索腿。
const MaxKeptImages = 2

// DefaultImageRerankLimits 是送入多模态重排模型的图片约束（上游 rerank
// 包 ImageLimits 的取值：10MB / 最长边 4096）。
var DefaultImageRerankLimits = imageprep.Limits{
	MaxBytes:     10 * 1024 * 1024,
	MaxDimension: 4096,
	MIMETypes:    []string{"image/png", "image/jpeg", "image/webp", "image/gif"},
}

// ImageKeepScoreFor 返回图片兜底保留门槛：向量阈值加 0.05（上游同式）。
// 图片腿本身已按 ImageVectorThreshold 过滤，兜底只在“文本拒绝但向量很强”
// 时出手，门槛应略高于图片腿阈值。
func ImageKeepScoreFor(vectorThreshold float64) float64 {
	return vectorThreshold + 0.05
}

// IsPictorialImageHit 报告该行是否为图片腿命中行（image_vector 行经向量
// 检索命中）。上游 IsPictorial 进一步排除“带 OCR 文本锚点”的行；Zilan 的
// SearchResult 不携带独立 OCRText 字段，此处对所有 image_vector 向量命中
// 统一适用兜底规则（比上游略宽：多保留的只是高向量分的图片行）。
func IsPictorialImageHit(r *types.SearchResult) bool {
	return r != nil &&
		r.ChunkType == types.ChunkTypeImageVector &&
		r.MatchType == types.MatchTypeEmbedding
}

// RerankCandidates 对候选执行模型打分：图片感知分支见文件头。opts 为零值
// （无 LoadImage）时行为与 FR-19 的直接 Rerank 完全一致。
func RerankCandidates(
	ctx context.Context,
	model rerank.Reranker,
	query string,
	candidates *Candidates,
	opts types.RerankImageOptions,
	diag *types.RerankDiagnostics,
) ([]rerank.RankResult, error) {
	if candidates == nil || candidates.Len() == 0 {
		return model.Rerank(ctx, query, nil)
	}
	if opts.LoadImage == nil || !hasImageHit(candidates) {
		// 无图片命中或调用方未提供读图能力：纯文本重排（存量路径）。
		return model.Rerank(ctx, query, candidates.Passages)
	}
	imageReranker, err := rerank.AsImageReranker(model)
	if err != nil {
		// 当前重排模型不支持图片：全部按文本打分（§5.3 降级）。
		logger.Infof(ctx, "[Rerank] model does not accept images, scoring %d candidates as text", candidates.Len())
		return model.Rerank(ctx, query, candidates.Passages)
	}

	// 为图片命中行读取并预处理图片；读不到的行按“无图”降级（passage 照旧）。
	images := make([]imageprep.Image, candidates.Len())
	scored := 0
	for i, row := range candidates.Rows {
		if !IsPictorialImageHit(row) {
			continue
		}
		data, err := opts.LoadImage(ctx, row)
		if err != nil {
			logger.Warnf(ctx, "[Rerank] load image for chunk %s failed, scoring it as text: %v", row.ID, err)
			continue
		}
		img, err := imageprep.Prepare(data, DefaultImageRerankLimits)
		if err != nil {
			logger.Warnf(ctx, "[Rerank] image of chunk %s does not fit rerank model limits, scoring it as text: %v", row.ID, err)
			continue
		}
		images[i] = img
		scored++
	}
	if scored == 0 {
		return model.Rerank(ctx, query, candidates.Passages)
	}

	scores, err := imageReranker.RerankImages(ctx, query, candidates.Passages, images)
	if err != nil {
		// 图片重排失败不阻断文本重排：整批退回纯文本打分（§5.3）。
		logger.Warnf(ctx, "[Rerank] image-aware rerank failed, falling back to text-only rerank: %v", err)
		return model.Rerank(ctx, query, candidates.Passages)
	}
	if diag != nil {
		diag.ImagesScored = scored
	}
	return scores, nil
}

// hasImageHit 报告候选中是否存在图片腿命中行。
func hasImageHit(candidates *Candidates) bool {
	for _, row := range candidates.Rows {
		if IsPictorialImageHit(row) {
			return true
		}
	}
	return false
}

// KeepImages 把“被阈值过滤拒绝、但向量分达到 imageKeepScore”的图片命中行
// 以副本形式追加回结果（至多 MaxKeptImages 条），副本 metadata 标记
// MetadataKeptBy=KeptByImageVector；下游 TopK 裁剪必须保留带该标记的行
// （见 searchutil.TopKKeepingKept）。imageKeepScore <= 0 时为 no-op。
func KeepImages(
	ctx context.Context,
	candidates *Candidates,
	reranked []*types.SearchResult,
	imageKeepScore float64,
	diag *types.RerankDiagnostics,
) []*types.SearchResult {
	if imageKeepScore <= 0 || candidates == nil || len(candidates.Rows) == 0 {
		return reranked
	}
	survived := make(map[string]struct{}, len(reranked))
	for _, r := range reranked {
		survived[r.ID] = struct{}{}
	}
	kept := 0
	for i, row := range candidates.Rows {
		if kept >= MaxKeptImages {
			break
		}
		if _, ok := survived[row.ID]; ok {
			continue
		}
		if !IsPictorialImageHit(row) || row.VectorScore < imageKeepScore {
			continue
		}
		passage := ""
		if i < len(candidates.Passages) {
			passage = candidates.Passages[i]
		}
		copy := ScoredCopy(row, row.VectorScore, 1.0, passage)
		copy.Metadata[types.MetadataKeptBy] = types.KeptByImageVector
		reranked = append(reranked, copy)
		kept++
		logger.Infof(ctx, "[Rerank] kept image hit %s by vector score %.4f (threshold for keep: %.4f)",
			row.ID, row.VectorScore, imageKeepScore)
	}
	if diag != nil {
		diag.ImagesKept += kept
	}
	return reranked
}

// RestoreKeptImages 把带 KeptByImageVector 标记、却被后续 top-k 裁剪
// （MMR 等）丢出的行补回结果尾部（上游 TopKKeepingKept 的意图：显式
// 保留的图片命中不能被只看文本分的截断丢掉）。至多补回 MaxKeptImages 条。
func RestoreKeptImages(final, scored []*types.SearchResult) []*types.SearchResult {
	if len(scored) == 0 {
		return final
	}
	inFinal := make(map[string]struct{}, len(final))
	for _, r := range final {
		inFinal[r.ID] = struct{}{}
	}
	restored := 0
	for _, r := range scored {
		if restored >= MaxKeptImages {
			break
		}
		if r.Metadata == nil || r.Metadata[types.MetadataKeptBy] != types.KeptByImageVector {
			continue
		}
		if _, ok := inFinal[r.ID]; ok {
			continue
		}
		final = append(final, r)
		restored++
	}
	return final
}
