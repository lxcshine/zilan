package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"golang.org/x/sync/errgroup"
)

// Default fan-out limits. Tuned for current production sizing: at most 4
// stores per request and 30s per group, matching engine_factory.go defaults.
const (
	defaultMultiStoreRetrieveTimeout = 30 * time.Second
	defaultMultiStoreFanoutLimit     = 4
)

// retrieveFromStores executes Retrieve per storeGroup with bounded
// concurrency and a per-group timeout. When >1 group is present AND the
// collected results span >1 engine type, the normalizer rescales vector
// scores into [0, 1] before fusion.
//
// Failure policy: 一个“完全没有结果”的 store group 失败会让整轮检索失败，
// 并通过 errgroup 取消兄弟分组。但一次 Retrieve 调用可能在返回错误的同时也
// 带回结果——某条检索腿或某个 collection 挂了、另一些成功了。这类结果会被
// retainPartialResults 保留并在 WARN 级别记录：整轮失败会把已经成功取回的
// 证据一起扔掉，而毫无痕迹地返回又会让调用方把残缺答案当成完整答案。
//
// Fast path: len(groups) == 1 → returns the single engine's Retrieve
// directly with zero fan-out overhead. This is the dominant case today
// because every existing KB has vector_store_id = NULL.
//
// Concurrency cap: g.SetLimit(defaultMultiStoreFanoutLimit) bounds
// per-request fan-out concurrency. NOTE: this caps application-level
// goroutine count but does NOT set MaxOpenConns on the shared gorm pool
// (which currently defaults to unlimited for Postgres in
// container.go:541). Operators should still configure SetMaxOpenConns
// per their PG max_connections budget.
func (s *knowledgeBaseService) retrieveFromStores(
	ctx context.Context,
	groups []*storeGroup,
	normalizer retriever.ScoreNormalizer,
) ([]*types.RetrieveResult, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	if len(groups) == 1 {
		return retrieveGroup(ctx, groups[0])
	}

	timeout := multiStoreRetrieveTimeout()

	var (
		mu  sync.Mutex
		all []*types.RetrieveResult
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(defaultMultiStoreFanoutLimit)

	for i := range groups {
		grp := groups[i]
		g.Go(func() error {
			gcCtx, cancel := context.WithTimeout(gctx, timeout)
			defer cancel()
			res, err := retrieveGroup(gcCtx, grp)
			if err != nil {
				logger.WarnWithFields(gctx, logger.Fields{
					"tenant_id":  grp.OwnerTenantID,
					"kb_count":   len(grp.KBIDs),
					"store_kind": storeKindLabel(grp.StoreID),
				}, fmt.Sprintf("multi-store retrieve failed: %v", err))
				return fmt.Errorf("store group retrieve: %w", err)
			}
			mu.Lock()
			all = append(all, res...)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		// Only treat an explicit client cancellation (context.Canceled)
		// as "the client gave up". A parent context whose deadline has
		// expired must still surface as a typed unavailable error so the
		// handler returns a clean 4xx instead of leaking the raw stdlib
		// DeadlineExceeded.
		if isParentCancelled(ctx) {
			return nil, ctx.Err()
		}
		// Any retrieve failure (per-group timeout, transport error,
		// upstream rejection) is collapsed into a single typed
		// unavailable error. The underlying cause is recorded in
		// structured logs above; the response body intentionally exposes
		// no internal detail.
		return nil, apperrors.NewVectorStoreUnavailableError(
			"vector retrieval failed for one or more bound stores")
	}

	// Apply normalizer only when results span >1 distinct engine type.
	// Same-engine results keep their native scale — raw scores produced by
	// the same engine are directly comparable.
	if hasMixedEngineTypes(all) {
		seenUnknown := make(map[types.RetrieverEngineType]struct{})
		for _, rr := range all {
			for _, hit := range rr.Results {
				hit.Score = normalizer.Normalize(
					ctx, hit.Score, rr.RetrieverType, rr.RetrieverEngineType)
			}
			if !isKnownEngineType(rr.RetrieverEngineType) {
				if _, dup := seenUnknown[rr.RetrieverEngineType]; !dup {
					seenUnknown[rr.RetrieverEngineType] = struct{}{}
					// Engine type strings can originate from operator
					// configuration; sanitize before logging to defeat
					// CR/LF/tab log injection.
					logger.WarnWithFields(ctx, logger.Fields{
						"engine_type": secutils.SanitizeForLog(string(rr.RetrieverEngineType)),
					}, "score normalizer: unknown engine type, applying clamp01 fallback")
				}
			}
		}
	}
	return all, nil
}

// retrieveGroup 执行单个 store group 的检索，并把“结果 + 错误”并存的部分失败
// 归一化为“要么有结果、要么有错误”，让单组与多组扇出走完全相同的策略。
func retrieveGroup(ctx context.Context, g *storeGroup) ([]*types.RetrieveResult, error) {
	res, err := g.Engine.Retrieve(ctx, paramsWithTopK(g))
	return retainPartialResults(ctx, g, res, err)
}

// retainPartialResults 保留与错误一并返回的结果。
//
// CompositeRetrieveEngine 在某条检索腿/某个 collection 失败而其他检索成功时，
// 会带着非 nil error 返回结果。丢掉这些结果会让一个降级的存储变成整轮检索
// 失败、并损失已经取回的证据；毫无痕迹地返回又会让调用方把不完整的答案当成
// 完整答案。因此这里在 WARN 级别记录失败（本函数是仍然知道 store group 上下文
// 的最近消费者），并以 nil error 返回结果。没有任何结果的失败仍然是致命的。
func retainPartialResults(
	ctx context.Context, g *storeGroup, res []*types.RetrieveResult, err error,
) ([]*types.RetrieveResult, error) {
	if err == nil || len(res) == 0 {
		return res, err
	}
	logger.WarnWithFields(ctx, logger.Fields{
		"tenant_id":   g.OwnerTenantID,
		"kb_count":    len(g.KBIDs),
		"store_kind":  storeKindLabel(g.StoreID),
		"result_sets": len(res),
	}, fmt.Sprintf("retrieve returned partial results: %v", err))
	return res, nil
}

// paramsWithTopK builds a fresh []RetrieveParams for a group. BaseParams
// stay immutable; only TopK is overridden. This isolates iterative FAQ's
// TopK mutation from the goroutines that read params inside Retrieve,
// so no aliasing assumption is required for the outer slice (and -race
// will not flag this path).
//
// The value-copy of each RetrieveParams shallow-copies its reference
// fields (Embedding, KnowledgeBaseIDs, TagIDs, KnowledgeIDs,
// AdditionalParams). All fan-out goroutines from a single
// retrieveFromStores call therefore share the same backing arrays for
// those fields. Every retriever implementation today treats them as
// read-only, so this is safe — but the immutability of reference
// fields is a caller contract. If a future retriever ever mutates
// Embedding in-place (e.g. to L2-normalize), -race will surface the
// regression.
func paramsWithTopK(g *storeGroup) []types.RetrieveParams {
	out := make([]types.RetrieveParams, len(g.BaseParams))
	for i, p := range g.BaseParams {
		p.TopK = g.TopK
		out[i] = p
	}
	return out
}

// hasMixedEngineTypes returns true if results span 2+ distinct
// RetrieverEngineType values. Empty/zero values count as their own bucket.
func hasMixedEngineTypes(results []*types.RetrieveResult) bool {
	if len(results) < 2 {
		return false
	}
	first := results[0].RetrieverEngineType
	for _, r := range results[1:] {
		if r.RetrieverEngineType != first {
			return true
		}
	}
	return false
}

// isKnownEngineType reports whether the given engine type has a
// hard-coded normalization entry in EngineAwareNormalizer. Used by the
// caller of Normalize to deduplicate "unknown engine" WARN logs per
// request.
func isKnownEngineType(t types.RetrieverEngineType) bool {
	switch t {
	case types.ElasticsearchRetrieverEngineType,
		types.ElasticFaissRetrieverEngineType,
		types.MilvusRetrieverEngineType,
		types.PostgresRetrieverEngineType,
		types.QdrantRetrieverEngineType,
		types.WeaviateRetrieverEngineType,
		types.SQLiteRetrieverEngineType,
		types.InfinityRetrieverEngineType,
		types.TencentVectorDBRetrieverEngineType,
		types.DorisRetrieverEngineType:
		return true
	}
	return false
}

// multiStoreRetrieveTimeout reads MULTI_STORE_RETRIEVE_TIMEOUT_SEC; falls
// back to defaultMultiStoreRetrieveTimeout (30s) on absence, parse error,
// or non-positive values.
func multiStoreRetrieveTimeout() time.Duration {
	raw := os.Getenv("MULTI_STORE_RETRIEVE_TIMEOUT_SEC")
	if raw == "" {
		return defaultMultiStoreRetrieveTimeout
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultMultiStoreRetrieveTimeout
	}
	return time.Duration(n) * time.Second
}

// storeKindLabel returns "env" or "bound" for log fields. Never echoes the
// raw store UUID.
func storeKindLabel(storeID string) string {
	if storeID == "" {
		return "env"
	}
	return "bound"
}

// isParentCancelled reports whether the parent ctx was explicitly
// cancelled by the caller (client hang-up). A parent ctx whose deadline
// merely expired is NOT treated as a client cancel — that path falls
// through to the typed 2201 AppError so middleware emits a clean 4xx
// instead of leaking context.DeadlineExceeded.
func isParentCancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}
