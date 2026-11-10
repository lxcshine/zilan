// knowledgebase_search_image.go —— 图片向量检索腿（FR-17 图文共空间检索）。
//
// 照抄上游 WeKnora v0.8 internal/application/service/knowledgebase_search_image.go
// 与 internal/reranking/image.go 的过滤/保留规则，适配 Zilan 的检索管线
// （HybridSearch → buildRetrievalParams → retrieveFromStores →
// processSearchResults）：
//   - buildRetrievalParams：组内任一 KB 开启 ImageVector 时，向量腿扩池
//     （图片行的 caption 文本信号弱于正文，不扩池进不了过采样候选）并
//     把向量阈值提到图片腿下限；
//   - processSearchResults：image_vector 命中行进入 SearchResult 后，
//     filterImageHits 按图片腿阈值过滤（文本行不受影响）；低分/陈旧图片
//     行被移除时只记日志，绝不阻断文本检索（§5.3）；
//   - ReadChunkImage：把 image_vector 行指向的图片读成字节，供图片感知
//     重排与视觉模型上下文注入复用。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// ImageVectorThreshold 图片命中行的最低向量相似度（FR-17）：图片向量与
// 文本向量同空间，但 caption 行的语义密度低于正文，需要更高的下限防止
// 弱相关图片行混进候选。与上游取值一致。
const ImageVectorThreshold = 0.4

// imageRecallExtra 图片腿的过采样扩池量：图片行的 caption 文本与查询的
// 字面重叠通常很小，不扩池时图片行很难挤进 top-N 候选。
const imageRecallExtra = 15

// withImageVectorRecall 计算图片腿启用时向量检索参数的扩池 TopK 与
// 阈值。hasImageKB 为 false 时原样返回（存量库零变化，§5.4）。
func withImageVectorRecall(topK int, threshold float64, hasImageKB bool) (int, float64) {
	if !hasImageKB {
		return topK, threshold
	}
	if threshold < ImageVectorThreshold {
		threshold = ImageVectorThreshold
	}
	return topK + imageRecallExtra, threshold
}

// filterImageHits 对 SearchResult 应用图片腿过滤规则（上游 filterImageHits
// 的 SearchResult 适配版）：
//   - image_vector 行只有在“本行由图片编码而来”（Metadata.source_type=image，
//     等价上游的 SourceType 列）且向量分达到 max(vectorThreshold,
//     ImageVectorThreshold) 时才保留；
//   - 陈旧行（无 source_type 标记的 image_vector 行）与低分行被移除，
//     移除时记 WARN 日志；
//   - 文本行一律保留，任何图片行被移除都不影响文本检索结果（§5.3）。
//
// 返回过滤后的列表与保留的图片行数。
func filterImageHits(
	ctx context.Context, results []*types.SearchResult, vectorThreshold float64,
) ([]*types.SearchResult, int) {
	if len(results) == 0 {
		return results, 0
	}
	threshold := max(vectorThreshold, ImageVectorThreshold)
	kept := make([]*types.SearchResult, 0, len(results))
	keptImages := 0
	for _, r := range results {
		if reason := imageHitDropReason(r, threshold); reason != "" {
			logger.Warnf(ctx, "[ImageVector] image hit dropped: chunk=%s kb=%s score=%.4f reason=%s",
				r.ID, r.KnowledgeBaseID, r.VectorScore, reason)
			continue
		}
		kept = append(kept, r)
		if r.ChunkType == types.ChunkTypeImageVector {
			keptImages++
		}
	}
	return kept, keptImages
}

// imageHitDropReason 返回图片命中行应被丢弃的原因；空串表示保留。
// 文本行（非 image_vector）恒为空串。
func imageHitDropReason(r *types.SearchResult, threshold float64) string {
	if r.ChunkType != types.ChunkTypeImageVector {
		return ""
	}
	// 只有“本行由图片编码而来”的行才是图片腿命中（上游 SourceType ==
	// ImageSourceType；Zilan 无该列，等价标记在 chunk Metadata 里）。
	if searchutil.ImageVectorSourceMarked(r) {
		if r.VectorScore < threshold {
			return "image hit below vector threshold"
		}
		return ""
	}
	if searchutil.ImageVectorChunkStale(r) {
		return "image hit not marked as an image source (missing metadata); stored before the image leg learned to write it"
	}
	return ""
}

// ReadChunkImage 读取检索结果所指向图片的字节（FR-17③④）：图片感知重排
// 与视觉模型上下文注入共用。r.ImageInfo 携带图片地址（image_vector 行
// 或经富化的文本行均可）。读取失败返回错误，调用方按“跳过该图”降级。
func (s *knowledgeBaseService) ReadChunkImage(ctx context.Context, r *types.SearchResult) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("nil search result")
	}
	url := chunkImageURL(r)
	if url == "" {
		return nil, fmt.Errorf("search result %s has no image URL", r.ID)
	}

	// provider:// 与 resource:// 引用必须走 FileService（绝不能交给 HTTP
	// 下载器，issue #1282）；解析链路照抄 image_multimodal 的
	// resolveFileServiceForPayload：租户存储解析失败时退回全局默认服务。
	if _, isResourceRef := types.ParseResourcePath(url); isResourceRef || types.ParseProviderScheme(url) != "" {
		tenantID := types.MustTenantIDFromContext(ctx)
		fileSvc := s.fileSvc
		if s.tenantRepo != nil && s.storageResolver != nil {
			if tenant, err := s.tenantRepo.GetTenantByID(ctx, tenantID); err == nil && tenant != nil {
				backendID, provider, _ := types.ParseStorageBackendPath(url)
				if p := types.ParseProviderScheme(url); p != "" {
					provider = p
				}
				if provider == "" {
					if kb, kbErr := s.GetKnowledgeBaseByIDOnly(ctx, r.KnowledgeBaseID); kbErr == nil && kb != nil {
						provider = strings.ToLower(strings.TrimSpace(kb.GetStorageProvider()))
						if backendID == "" && kb.StorageBackendID != nil {
							backendID = *kb.StorageBackendID
						}
					}
				}
				baseDir := strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR"))
				if resolved, _, svcErr := s.storageResolver.ResolveFileService(ctx, tenant, backendID, provider, baseDir); svcErr == nil {
					fileSvc = resolved
				} else {
					logger.Warnf(ctx, "[ImageVector] resolve file service failed (falling back to default): provider=%s err=%v", provider, svcErr)
				}
			}
		}
		if fileSvc == nil {
			return nil, fmt.Errorf("no file service available for %s", url)
		}
		reader, err := fileSvc.GetFile(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("file service get %s: %w", url, err)
		}
		defer reader.Close()
		return io.ReadAll(reader)
	}

	// 普通 http(s) URL：SSRF 安全下载。
	data, err := secutils.DownloadBytes(url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	return data, nil
}

// chunkImageURL 解析 SearchResult.ImageInfo（JSON 数组）的首个图片地址。
func chunkImageURL(r *types.SearchResult) string {
	var infos []types.ImageInfo
	if err := json.Unmarshal([]byte(r.ImageInfo), &infos); err != nil || len(infos) == 0 {
		return ""
	}
	if infos[0].URL != "" {
		return infos[0].URL
	}
	return infos[0].OriginalURL
}
