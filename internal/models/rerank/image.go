// Package rerank 的图片扩展（FR-17 图文共空间检索）。
//
// 照抄上游 WeKnora v0.8 internal/models/rerank/image.go 的接口语义：
// Reranker 接口保持不变，支持图文混合重排的厂商额外实现 ImageReranker；
// 当前 Zilan 已接入的重排模型均不支持图片输入，AsImageReranker 一律返回
// ErrImagesUnsupported，重排侧（internal/reranking）据此走“图片命中行仅
// 凭 caption/OCR 文本打分 + 向量分兜底保留”的降级路径（§5.3）。
// 接口先落地，后续接入多模态重排模型时无需再动调用方。
package rerank

import (
	"context"
	"errors"

	"github.com/Tencent/WeKnora/internal/models/imageprep"
)

// ErrImagesUnsupported 表示当前重排模型不接受图片输入。
var ErrImagesUnsupported = errors.New("rerank model does not accept images")

// ImageReranker 是可感知图片内容的重排模型：文本候选照常按 passage 打分，
// 图片候选连同其图片数据一起送入模型（query-document-image 三方交互）。
type ImageReranker interface {
	Reranker

	// RerankImages 对齐 Rerank 的输入约定：passages 与 images 按 index
	// 一一对应（image 为 nil 表示该候选没有可用图片，仅按文本打分），
	// 返回的 RankResult.Index 指向候选下标。
	RerankImages(ctx context.Context, query string, passages []string, images []imageprep.Image) ([]RankResult, error)
}

// AsImageReranker 报告 m 是否支持图片重排。m 为 nil 时返回
// ErrImagesUnsupported（调用方按降级处理：全部候选走纯文本重排）。
func AsImageReranker(m Reranker) (ImageReranker, error) {
	if m == nil {
		return nil, ErrImagesUnsupported
	}
	if ir, ok := m.(ImageReranker); ok {
		return ir, nil
	}
	return nil, ErrImagesUnsupported
}
