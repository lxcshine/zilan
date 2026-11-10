// image_evidence.go —— 图片证据的挑选、继承与 TopK 保留（FR-17）。
//
// 照抄上游 WeKnora v0.8 internal/searchutil/image_evidence.go，适配点：
//   - 上游用 chunk 的 SourceType 列区分“图片编码行”；Zilan 无该列，
//     等价标记 image_vector chunk 的 Metadata["source_type"]="image"，
//     ImageVectorSourceMarked/ImageVectorChunkStale 统一读取该标记；
//   - Zilan 的 SearchResult.ChunkMetadata 为 types.JSON（json.RawMessage）。
package searchutil

import (
	"encoding/json"

	"github.com/Tencent/WeKnora/internal/types"
)

// MaxContextImages 单次对话注入检索图片的上限（上游同值）：图片 token
// 成本远高于文本，证据再多也只注入最有用的几张。
const MaxContextImages = 4

// defaultContextImageMaxBytes 视觉模型上下文图片的默认字节上限（10MB，
// 上游同值）。服务端通常按 base64 计费，裸字节上限即 10MB/4*3。
const defaultContextImageMaxBytes = 10 * 1024 * 1024

// defaultContextImageMaxDimension 视觉上下文图片最长边默认上限（4096，
// 上游同值）。
const defaultContextImageMaxDimension = 4096

// VisionImageLimits 是注入视觉模型上下文的图片约束（FR-17④）。
// Zero value means "no limits" — always call WithDefaults before use.
type VisionImageLimits struct {
	// MaxBytes limits one image after preparation (data-URI decoded size).
	MaxBytes int
	// MaxDimension limits width and height of one image, each independently.
	MaxDimension int
}

// WithDefaults returns the receiver with unset fields replaced by defaults.
func (l VisionImageLimits) WithDefaults() VisionImageLimits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = defaultContextImageMaxBytes
	}
	if l.MaxDimension <= 0 {
		l.MaxDimension = defaultContextImageMaxDimension
	}
	return l
}

// ContextImage is one image injected into the chat context (FR-17④):
// prepared (limits-enforced) data URI plus a short description to keep the
// model anchored on where the image came from.
type ContextImage struct {
	// DataURI is the prepared image as a data: URI.
	DataURI string
	// Description 记录图片来源（chunk/KB/得分），仅用于日志与调试。
	Description string
}

// IsImageEvidence reports whether the row carries image evidence (either it
// is an image_vector row itself, or it inherited image matches — see
// InheritImageEvidence).
func IsImageEvidence(r *types.SearchResult) bool {
	if r == nil {
		return false
	}
	return len(r.MatchedImages) > 0
}

// InheritImageEvidence merges src's image evidence into dst: the winner of a
// text-dedup merge (upstream mergeTextChunks / Zilan 的相邻块合并) inherits
// the image matches of the rows it absorbed, so images never vanish because
// their anchor row lost the merge.
func InheritImageEvidence(dst, src *types.SearchResult) {
	if dst == nil || src == nil || dst == src {
		return
	}
	if len(src.MatchedImages) == 0 {
		return
	}
	// 同一图片（按 URL 去重）只保留一份，来源行号不重复。
	seen := make(map[string]struct{}, len(dst.MatchedImages)+len(src.MatchedImages))
	for _, img := range dst.MatchedImages {
		seen[img.URL] = struct{}{}
	}
	for _, img := range src.MatchedImages {
		if _, ok := seen[img.URL]; ok {
			continue
		}
		seen[img.URL] = struct{}{}
		dst.MatchedImages = append(dst.MatchedImages, img)
	}
}

// IsKeptOutsideTopK reports whether the row was explicitly kept although it
// fell outside the top-k cut (see TopKKeepingKept): KeptByImageVector rows
// are protected from the cut.
func IsKeptOutsideTopK(r *types.SearchResult) bool {
	return r != nil && r.Metadata != nil && r.Metadata[types.MetadataKeptBy] == types.KeptByImageVector
}

// TopKKeepingKept cuts results to top-k but keeps rows explicitly marked as
// kept (MetadataKeptBy=KeptByImageVector, FR-17③): an image hit that survived
// the rerank fallback must not be dropped by a plain top-k cut that only looks
// at text scores.
func TopKKeepingKept(results []*types.SearchResult, k int) []*types.SearchResult {
	if k < 0 || len(results) <= k {
		return results
	}
	kept := make([]*types.SearchResult, 0, k)
	keptOutside := 0
	for _, r := range results {
		if len(kept) < k {
			kept = append(kept, r)
			continue
		}
		if IsKeptOutsideTopK(r) {
			// 超出 TopK 但被显式保留：替换队尾的普通行，保证总量不膨胀。
			if keptOutside < len(kept) {
				kept[len(kept)-1-keptOutside] = r
			}
			keptOutside++
		}
	}
	return kept
}

// CaptureImageEvidence takes the image_vector rows (hits of the FR-17 image
// leg) and turns them into image evidence carried by the surviving rows:
//   - the image_vector row itself keeps its MatchedImages and stays in the
//     list as a pictorial hit (content = caption/OCR);
//   - rows from the same knowledge base inherit the image matches so a text
//     hit can also surface the pictures it refers to.
//
// Called at processSearchResults stage（上游注释原文：before any merge
// rewrites ImageInfo）. Caller afterwards filters rows via FilterImageHits.
func CaptureImageEvidence(results []*types.SearchResult) {
	if len(results) == 0 {
		return
	}
	for _, r := range results {
		if r == nil || r.ChunkType != types.ChunkTypeImageVector {
			continue
		}
		r.MatchedImages = []types.MatchedImage{{
			ChunkID:         r.ID,
			KnowledgeBaseID: r.KnowledgeBaseID,
			URL:             firstImageURL(r.ImageInfo),
		}}
	}
	// 同库文本行继承图片证据：合并阶段（相邻块合并等）只会保留一个行，
	// 若图片证据只挂在被吸收的行上就会丢失。
	byKB := make(map[string][]*types.SearchResult)
	for _, r := range results {
		if r.ChunkType != types.ChunkTypeImageVector {
			byKB[r.KnowledgeBaseID] = append(byKB[r.KnowledgeBaseID], r)
		}
	}
	for _, r := range results {
		if r.ChunkType != types.ChunkTypeImageVector || len(r.MatchedImages) == 0 {
			continue
		}
		for _, textRow := range byKB[r.KnowledgeBaseID] {
			InheritImageEvidence(textRow, r)
		}
	}
}

// ImageVectorSourceMarked 报告该 image_vector 行是否带有“由图片编码而来”
// 的标记（上游的 chunk.SourceType == ImageSourceType；Zilan 等价标记在
// chunk Metadata JSON 的 source_type 字段）。
func ImageVectorSourceMarked(r *types.SearchResult) bool {
	if r == nil || r.ChunkType != types.ChunkTypeImageVector {
		return false
	}
	return imageVectorSourceFromMetadata(r.ChunkMetadata) == "image"
}

// ImageVectorChunkStale 报告该 image_vector 行是否为“陈旧行”：chunk
// Metadata 里没有 source_type 标记——无法确认其向量确由图片编码而来
// （上游同规则：无 SourceType 且无 ContentHash 的行按陈旧丢弃）。
func ImageVectorChunkStale(r *types.SearchResult) bool {
	if r == nil || r.ChunkType != types.ChunkTypeImageVector {
		return false
	}
	return imageVectorSourceFromMetadata(r.ChunkMetadata) == ""
}

func imageVectorSourceFromMetadata(metadata types.JSON) string {
	if len(metadata) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(metadata, &m); err != nil {
		return ""
	}
	if v, ok := m["source_type"].(string); ok {
		return v
	}
	return ""
}

// firstImageURL 解析 ImageInfo JSON（数组）的首个图片地址。
func firstImageURL(imageInfoJSON string) string {
	if imageInfoJSON == "" {
		return ""
	}
	var infos []types.ImageInfo
	if err := json.Unmarshal([]byte(imageInfoJSON), &infos); err != nil || len(infos) == 0 {
		return ""
	}
	if infos[0].URL != "" {
		return infos[0].URL
	}
	return infos[0].OriginalURL
}
