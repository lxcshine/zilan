// knowledge_process_image_vector.go —— 图片向量索引（FR-17 图文共空间检索）。
//
// 照抄上游 WeKnora v0.8 internal/application/service/image_multimodal.go 的
// indexImageVector/NewImageVectorChunk 逻辑，适配 Zilan 的管线：
//   - 图片向量与文本共享同一向量索引（BatchIndexVectors 复用引擎写入路径）；
//   - §5.3 降级：嵌入模型不支持图片 / 图片超限 / 引擎写入失败，一律记日志
//     跳过该图，绝不阻断文档入库（OCR/caption chunk 已照常落库）；
//   - 扫描页排除（PRD FR-17⑤）：文档级 image_source_type=scanned_pdf 或
//     页级 page_class=scanned 的整页渲染图不生成图片向量——其内容走 OCR
//     文本路径，避免整页扫描图污染图片检索结果。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/models/imageprep"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
)

// imageVectorContentMaxRunes 图片向量行 content 的 OCR 兜底截断长度：
// 该行被文本查询命中时，Content 就是模型看到的图片描述，过长无益。
const imageVectorContentMaxRunes = 160

// indexImageVector 为单张图片生成图文共同空间的向量并写入向量索引。
// 由 indexChunks 在文本 chunk（OCR/caption）索引完成后调用，全部失败
// 路径均只记日志（§5.3：图片嵌入失败绝不阻断文档入库）。
func (s *ImageMultimodalService) indexImageVector(
	ctx context.Context,
	payload types.ImageMultimodalPayload,
	kb *types.KnowledgeBase,
	engine *retriever.CompositeRetrieveEngine,
	embeddingModel embedding.Embedder,
	imgBytes []byte,
	ocrText string,
	captionText string,
) {
	if !kb.IsImageVectorEnabled() {
		return
	}
	// 扫描页排除：整页扫描图的检索价值由 OCR 文本承载，不再进图片索引。
	if isScannedPageImage(payload) {
		logger.Infof(ctx, "[ImageVector] Skip scanned page image %s (source=%s page_class=%s), OCR text path covers it",
			payload.ImageURL, payload.ImageSourceType, payload.PageClass)
		return
	}
	if len(imgBytes) == 0 {
		logger.Warnf(ctx, "[ImageVector] Skip image %s: no image bytes available", payload.ImageURL)
		return
	}
	content := imageVectorChunkContent(captionText, ocrText)
	if content == "" {
		logger.Infof(ctx, "[ImageVector] Skip image %s: no caption/OCR content for image vector chunk", payload.ImageURL)
		return
	}

	imageEmbedder, err := embedding.AsImageEmbedder(embeddingModel)
	if err != nil {
		// 模型不支持图片输入：开关开着但模型能力不匹配，降级跳过并留痕，
		// 配置侧可通过该日志发现“开了开关但嵌入模型非多模态”。
		logger.Warnf(ctx, "[ImageVector] Skip image %s: embedding model %s does not accept images",
			payload.ImageURL, kb.EmbeddingModelID)
		return
	}

	img, err := imageprep.Prepare(imgBytes, embedding.ImageLimits)
	if err != nil {
		logger.Warnf(ctx, "[ImageVector] Skip image %s: does not fit model limits: %v", payload.ImageURL, err)
		return
	}

	vectors, err := imageEmbedder.EmbedImages(ctx, []imageprep.Image{img})
	if err != nil {
		logger.Warnf(ctx, "[ImageVector] Skip image %s: image embedding failed: %v", payload.ImageURL, err)
		return
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		logger.Warnf(ctx, "[ImageVector] Skip image %s: image embedding returned no usable vector", payload.ImageURL)
		return
	}

	chunk := s.NewImageVectorChunk(ctx, payload, content)
	if err := s.chunkService.CreateChunks(ctx, []*types.Chunk{chunk}); err != nil {
		logger.Warnf(ctx, "[ImageVector] Failed to persist image vector chunk for %s: %v", payload.ImageURL, err)
		return
	}

	indexInfo := []*types.IndexInfo{{
		Content:         chunk.Content,
		SourceID:        chunk.ID,
		SourceType:      types.ImageSourceType,
		ChunkID:         chunk.ID,
		KnowledgeID:     chunk.KnowledgeID,
		KnowledgeBaseID: chunk.KnowledgeBaseID,
	}}
	if err := engine.BatchIndexVectors(ctx, embeddingModel, indexInfo, vectors); err != nil {
		// 向量索引失败只跳过该图：DB 中 image_vector chunk 已落库，
		// 后续重解析可重建；OCR/caption 的文本索引不受影响。
		logger.Warnf(ctx, "[ImageVector] Failed to index image vector for %s: %v", payload.ImageURL, err)
		return
	}

	// 与 indexChunks 相同的收尾：落 indexed 状态。
	if dbChunk, gerr := s.chunkService.GetChunkByIDOnly(ctx, chunk.ID); gerr == nil {
		dbChunk.Status = int(types.ChunkStatusIndexed)
		if uerr := s.chunkService.UpdateChunk(ctx, dbChunk); uerr != nil {
			logger.Warnf(ctx, "[ImageVector] Failed to update chunk %s status to indexed: %v", chunk.ID, uerr)
		}
	}

	logger.Infof(ctx, "[ImageVector] Indexed image vector chunk %s for image %s", chunk.ID, payload.ImageURL)
}

// isScannedPageImage 判断该图片是否为扫描页的整页渲染图（PRD FR-17⑤）。
// 文档级：docreader（FR-13）对整本扫描 PDF 标记 image_source_type=scanned_pdf；
// 页级：page_classes 逐页分类中 class=scanned 的渲染页。两类均排除。
func isScannedPageImage(payload types.ImageMultimodalPayload) bool {
	if payload.ImageSourceType == "scanned_pdf" {
		return true
	}
	return payload.PageClass == "scanned"
}

// pageClassForImage 从 docreader 的 page_classes 元数据（逗号分隔、1-based
// 页序）中取图片所在页的分类。page 为 0（无 PDF 定位器/非 PDF）时返回空，
// 此时只有文档级 scanned_pdf 判定生效。
func pageClassForImage(pageClasses string, locators types.SourceLocators) string {
	if pageClasses == "" || len(locators) == 0 {
		return ""
	}
	page := 0
	for _, loc := range locators {
		if loc.Page > 0 {
			page = loc.Page
			break
		}
	}
	if page <= 0 {
		return ""
	}
	classes := strings.Split(pageClasses, ",")
	if page > len(classes) {
		return ""
	}
	return strings.TrimSpace(classes[page-1])
}

// imageVectorChunkContent 返回图片向量行的可读文本：优先 caption，
// 否则截取 OCR 前 imageVectorContentMaxRunes 个字符。两者皆无返回空。
func imageVectorChunkContent(captionText, ocrText string) string {
	if captionText != "" {
		return captionText
	}
	if ocrText != "" {
		runes := []rune(ocrText)
		if len(runes) > imageVectorContentMaxRunes {
			return string(runes[:imageVectorContentMaxRunes])
		}
		return ocrText
	}
	return ""
}

// NewImageVectorChunk 构造图片向量 chunk（FR-17）。content 采用
// caption（或 OCR 截断），该行被文本查询命中时它就是模型看到的图片
// 描述；归属关系沿用 Zilan 的父子建模：ParentChunkID 指向图片占位
// chunk（与 image_ocr/image_caption 一致），Metadata.source_type=image
// 标记“本行由图片编码而来”——检索侧据此应用图片腿的过滤与保留规则
// （上游对应的字段是 chunk 的 SourceType 列，Zilan 无该列，走 Metadata
// 以避免迁移）。
func (s *ImageMultimodalService) NewImageVectorChunk(
	ctx context.Context, payload types.ImageMultimodalPayload, content string,
) *types.Chunk {
	imageInfoJSON := fmt.Sprintf(`[{"url":%q}]`, payload.ImageURL)
	// source_type 标记值与上游 chunk.SourceType 列的取值一致（字符串
	// "image"）；types.ImageSourceType 是 Zilan 侧的 int 枚举，仅用于
	// IndexInfo，元数据里统一写上游字面量以便互操作。
	metadataJSON, _ := json.Marshal(map[string]string{"source_type": "image"})
	now := time.Now()
	return &types.Chunk{
		ID:              uuid.New().String(),
		TenantID:        payload.TenantID,
		KnowledgeID:     payload.KnowledgeID,
		KnowledgeBaseID: payload.KnowledgeBaseID,
		Content:         content,
		ChunkType:       types.ChunkTypeImageVector,
		ParentChunkID:   payload.ChunkID,
		IsEnabled:       true,
		Flags:           types.ChunkFlagRecommended,
		ImageInfo:       imageInfoJSON,
		SourceLocators:  payload.SourceLocators,
		Metadata:        types.JSON(metadataJSON),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}
