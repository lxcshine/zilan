// retriever/precomputed.go —— 预计算向量复用 BatchIndex 的接入点（FR-17）。
//
// 照抄自上游 WeKnora v0.8 internal/application/service/retriever/precomputed.go
// 并适配 Zilan 引擎的嵌入调用方式：Zilan 的引擎在 BatchIndex 内部按
// indexInfoList 顺序构造 contentList 并调用 BatchEmbedWithPool/BatchEmbed，
// 因此预计算包装器按“调用顺序弹出”返回向量，与上游按 SourceID 键控的
// 方案等价，且天然支持重复 content 的行（两张同 caption 的图不会互相覆盖）。
package retriever

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
)

// BatchIndexVectors indexes pre-computed vectors into the engine bound to the
// given KB. It reuses BatchIndex by wrapping the embedder so the engine's
// internal embedding step returns the caller's vectors verbatim.
//
// 上游同名方法的忠实移植：model 必须非 nil（调用方负责在图片嵌入不可用
// 时提前降级），vectors 与 indexInfo 一一对应。
func (c *CompositeRetrieveEngine) BatchIndexVectors(
	ctx context.Context,
	model embedding.Embedder,
	indexInfo []*types.IndexInfo,
	vectors [][]float32,
) error {
	if len(indexInfo) == 0 {
		return nil
	}
	if len(vectors) != len(indexInfo) {
		return fmt.Errorf("vector count %d does not match index info count %d", len(vectors), len(indexInfo))
	}
	if model == nil {
		return fmt.Errorf("model must not be nil for vector indexing")
	}
	return c.BatchIndex(ctx, newPrecomputedEmbedder(model, vectors), indexInfo)
}

// precomputedEmbedder swaps the wrapped embedder's embedding calls for the
// caller's vectors. It keeps the original model's identity (model ID,
// dimensions) so engine-side routing and metric selection stay unchanged.
type precomputedEmbedder struct {
	embedding.Embedder
	vectors [][]float32
	next    int
}

func newPrecomputedEmbedder(model embedding.Embedder, vectors [][]float32) embedding.Embedder {
	return &precomputedEmbedder{Embedder: model, vectors: vectors}
}

// BatchEmbedWithPool returns the next len(texts) pre-computed vectors in
// order. The engine builds texts in indexInfoList order, matching the order
// the caller passed vectors in.
func (p *precomputedEmbedder) BatchEmbedWithPool(
	ctx context.Context, embedder embedding.Embedder, texts []string,
) ([][]float32, error) {
	return p.take(ctx, len(texts), func() ([][]float32, error) {
		return p.Embedder.BatchEmbedWithPool(ctx, embedder, texts)
	})
}

// BatchEmbed returns the next len(texts) pre-computed vectors in order.
func (p *precomputedEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	return p.take(ctx, len(texts), func() ([][]float32, error) {
		return p.Embedder.BatchEmbed(ctx, texts)
	})
}

// Embed returns the next single pre-computed vector (single-row Index path).
func (p *precomputedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if p.next+1 <= len(p.vectors) {
		out := p.vectors[p.next]
		p.next++
		return out, nil
	}
	// 兜底委托原模型，宁可多一次真实嵌入也不要错位写入。
	return p.Embedder.Embed(ctx, text)
}

func (p *precomputedEmbedder) take(
	ctx context.Context, n int, fallback func() ([][]float32, error),
) ([][]float32, error) {
	if n >= 0 && p.next+n <= len(p.vectors) {
		out := p.vectors[p.next : p.next+n]
		p.next += n
		return out, nil
	}
	// 不应发生：引擎只对 BatchIndex 传入的 indexInfo 触发嵌入。兜底委托
	// 原模型，宁可多一次真实嵌入也不要错位写入。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return fallback()
}
