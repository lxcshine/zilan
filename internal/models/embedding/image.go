// Package embedding 的图片扩展（FR-17 图文共空间检索）。
//
// 照抄上游 WeKnora v0.8 internal/models/embedding 的图片嵌入接口语义：
// Embedder 接口保持不变，支持图片的厂商额外实现 ImageEmbedder；
// AsImageEmbedder 做能力协商，不支持图片的模型返回 ErrImagesUnsupported，
// 调用方（图片向量索引）据此降级跳过（§5.3），绝不阻断文档入库。
package embedding

import (
	"context"
	"errors"

	"github.com/Tencent/WeKnora/internal/models/imageprep"
)

// ErrImagesUnsupported 表示当前嵌入模型不接受图片输入。能力协商失败时
// 返回该哨兵错误而非硬失败，让调用方走“跳过该图”的降级路径。
var ErrImagesUnsupported = errors.New("embedding model does not accept images")

// ImageEmbedder 是支持把图片编码进与文本同一向量空间的嵌入模型。
// 图片向量与文本向量共享维度与索引，检索侧无需区分空间。
type ImageEmbedder interface {
	Embedder

	// EmbedImages 对一批图片各产生一个向量，顺序与输入一致。
	// 实现方负责遵守 DashScope 等服务端的批大小限制（当前 10）。
	EmbedImages(ctx context.Context, images []imageprep.Image) ([][]float32, error)
}

// AsImageEmbedder 报告 m 是否支持图片嵌入。m 为 nil 时返回
// ErrImagesUnsupported（调用方按降级处理）。
func AsImageEmbedder(m Embedder) (ImageEmbedder, error) {
	if m == nil {
		return nil, ErrImagesUnsupported
	}
	if ie, ok := m.(ImageEmbedder); ok {
		return ie, nil
	}
	return nil, ErrImagesUnsupported
}

// ImageLimits 是图片嵌入服务端普遍接受的输入约束（DashScope
// multimodal-embedding：最长边 4096、10MB 字节、png/jpeg/webp/gif）。
// 上游 WeKnora v0.8 同名常量的取值与此一致。
var ImageLimits = imageprep.Limits{
	MaxBytes:     10 * 1024 * 1024,
	MaxDimension: 4096,
	MIMETypes:    []string{"image/png", "image/jpeg", "image/webp", "image/gif"},
}
