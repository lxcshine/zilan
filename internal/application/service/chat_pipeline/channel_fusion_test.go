package chatpipeline

import (
	"context"
	"math"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestFuseChannelsWithWeightedRRFMergesAndAccumulates(t *testing.T) {
	lists := []channelRankedList{
		{
			Channel: types.ChannelVector,
			Weight:  0.7,
			Results: []*types.SearchResult{
				{ID: "a", Score: 0.9},
				{ID: "b", Score: 0.8},
			},
		},
		{
			Channel: types.ChannelKeyword,
			Weight:  0.3,
			Results: []*types.SearchResult{
				{ID: "b", Score: 0.7},
				{ID: "c", Score: 0.6},
			},
		},
	}

	fused := fuseChannelsWithWeightedRRF(context.Background(), lists, 60)
	if len(fused) != 3 {
		t.Fatalf("fused count = %d, want 3", len(fused))
	}
	// "b" appears in both channels and must outrank the single-channel docs.
	if fused[0].ID != "b" {
		t.Fatalf("top fused doc = %q, want %q", fused[0].ID, "b")
	}
	wantB := 0.7/62.0 + 0.3/61.0 // rank 2 in vector, rank 1 in keyword
	if math.Abs(fused[0].Score-wantB) > 1e-9 {
		t.Fatalf("fused score for b = %v, want %v", fused[0].Score, wantB)
	}
	if fused[0].Metadata["fusion_channels"] == "" {
		t.Fatalf("fusion_channels metadata missing on fused result")
	}
}

func TestFuseChannelsWithWeightedRRFSkipsZeroWeightAndEmpty(t *testing.T) {
	lists := []channelRankedList{
		{Channel: types.ChannelVector, Weight: 0, Results: []*types.SearchResult{{ID: "x", Score: 1}}},
		{Channel: types.ChannelKeyword, Weight: 0.5, Results: nil},
	}
	if got := fuseChannelsWithWeightedRRF(context.Background(), lists, 60); len(got) != 0 {
		t.Fatalf("fused count = %d, want 0", len(got))
	}
}

func TestFuseChannelsWithWeightedRRFWeightBias(t *testing.T) {
	// Same single doc per channel at the same rank: the higher-weight channel
	// must produce the higher fused score.
	lists := []channelRankedList{
		{Channel: types.ChannelVector, Weight: 0.9, Results: []*types.SearchResult{{ID: "v", Score: 0.5}}},
		{Channel: types.ChannelKeyword, Weight: 0.1, Results: []*types.SearchResult{{ID: "k", Score: 0.5}}},
	}
	fused := fuseChannelsWithWeightedRRF(context.Background(), lists, 60)
	if len(fused) != 2 {
		t.Fatalf("fused count = %d, want 2", len(fused))
	}
	if fused[0].ID != "v" {
		t.Fatalf("top fused doc = %q, want the heavily weighted %q", fused[0].ID, "v")
	}
}
