package chatpipeline

import (
	"context"
	"math"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// tenantCtxWithRetrievalConfig 构造一个带工作区级检索配置的上下文，
// 与线上 retrievalConfigFromContext 的取值路径一致。
func tenantCtxWithRetrievalConfig(cfg *types.RetrievalConfig) context.Context {
	return context.WithValue(context.Background(),
		types.TenantInfoContextKey, &types.Tenant{RetrievalConfig: cfg})
}

func twoChannelLists() []channelRankedList {
	return []channelRankedList{
		{
			Channel: types.ChannelVector,
			Weight:  0.7,
			Results: []*types.SearchResult{{ID: "a", Score: 0.9}, {ID: "b", Score: 0.8}},
		},
		{
			Channel: types.ChannelKeyword,
			Weight:  0.3,
			Results: []*types.SearchResult{{ID: "b", Score: 0.7}},
		},
	}
}

// FR-19①：融合常数必须来自工作区级配置，而不是写死的 60。
func TestFusionUsesRRFKFromTenantConfig(t *testing.T) {
	t.Parallel()
	ctx := tenantCtxWithRetrievalConfig(&types.RetrievalConfig{RRFK: 10})

	k := retrievalConfigFromContext(ctx).GetEffectiveRRFK()
	if k != 10 {
		t.Fatalf("RRF k from config = %d, want 10", k)
	}

	fused := fuseChannelsWithWeightedRRF(ctx, twoChannelLists(), k)
	if len(fused) != 2 {
		t.Fatalf("fused count = %d, want 2", len(fused))
	}
	// b: vector 第 2 名 + keyword 第 1 名；k=10 时的期望值。
	want := 0.7/12.0 + 0.3/11.0
	if math.Abs(fused[0].Score-want) > 1e-9 {
		t.Fatalf("fused score = %v, want %v (k=10)", fused[0].Score, want)
	}

	// 同一份输入用历史默认值 60 融合，分数必须不同 —— 证明 k 真的生效了。
	defaultCtx := context.Background()
	defaultFused := fuseChannelsWithWeightedRRF(defaultCtx, twoChannelLists(),
		retrievalConfigFromContext(defaultCtx).GetEffectiveRRFK())
	wantDefault := 0.7/62.0 + 0.3/61.0
	if math.Abs(defaultFused[0].Score-wantDefault) > 1e-9 {
		t.Fatalf("default fused score = %v, want %v (k=60)", defaultFused[0].Score, wantDefault)
	}
	if math.Abs(fused[0].Score-defaultFused[0].Score) < 1e-9 {
		t.Fatal("configured k produced the same scores as the default k")
	}
}

// 未配置检索上下文时必须回落到 60，保证既有会话排序不变。
func TestFusionFallsBackToDefaultRRFKWithoutTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if cfg := retrievalConfigFromContext(ctx); cfg != nil {
		t.Fatalf("expected no tenant config, got %+v", cfg)
	}
	if got := retrievalConfigFromContext(ctx).GetEffectiveRRFK(); got != 60 {
		t.Fatalf("fallback RRF k = %d, want 60", got)
	}
	fused := fuseChannelsWithWeightedRRF(ctx, twoChannelLists(), 0)
	want := 0.7/62.0 + 0.3/61.0
	if math.Abs(fused[0].Score-want) > 1e-9 {
		t.Fatalf("k<=0 fallback score = %v, want %v", fused[0].Score, want)
	}
}
