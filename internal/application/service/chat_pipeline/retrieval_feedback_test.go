package chatpipeline

import (
	"math"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestDetectRetrievalFailure(t *testing.T) {
	positives := []string{
		"根据现有信息无法回答该问题。",
		"抱歉，知识库中没有相关内容。",
		"Sorry, I cannot answer this based on the context.",
		"There is not enough information to respond.",
	}
	for _, p := range positives {
		if !DetectRetrievalFailure(p) {
			t.Fatalf("expected failure detected for %q", p)
		}
	}

	negatives := []string{
		"",
		"这是根据文档得出的完整答案。",
		"The answer is 42, based on the retrieved context.",
	}
	for _, n := range negatives {
		if DetectRetrievalFailure(n) {
			t.Fatalf("unexpected failure detected for %q", n)
		}
	}
}

func TestApplyRetrievalFeedbackBroadensRecall(t *testing.T) {
	cfg := &types.RetrievalConfig{
		VectorThreshold:  0.20,
		KeywordThreshold: 0.30,
		EmbeddingTopK:    50,
	}
	out := ApplyRetrievalFeedback(cfg, "", nil)

	if out.VectorThreshold >= cfg.VectorThreshold {
		t.Fatalf("vector threshold not lowered: %v >= %v", out.VectorThreshold, cfg.VectorThreshold)
	}
	if out.KeywordThreshold >= cfg.KeywordThreshold {
		t.Fatalf("keyword threshold not lowered: %v >= %v", out.KeywordThreshold, cfg.KeywordThreshold)
	}
	if out.EmbeddingTopK <= cfg.EmbeddingTopK {
		t.Fatalf("embedding top-k not increased: %v <= %v", out.EmbeddingTopK, cfg.EmbeddingTopK)
	}
	// Input must not be mutated.
	if cfg.VectorThreshold != 0.20 || cfg.EmbeddingTopK != 50 {
		t.Fatalf("input config mutated: %+v", cfg)
	}
}

func TestApplyRetrievalFeedbackBounds(t *testing.T) {
	cfg := &types.RetrievalConfig{
		VectorThreshold:  0.051,
		KeywordThreshold: 0.050,
		EmbeddingTopK:    199,
	}
	out := ApplyRetrievalFeedback(cfg, "", nil)
	if out.VectorThreshold < 0.05 {
		t.Fatalf("vector threshold below floor: %v", out.VectorThreshold)
	}
	if out.KeywordThreshold < 0.05 {
		t.Fatalf("keyword threshold below floor: %v", out.KeywordThreshold)
	}
	if out.EmbeddingTopK > 200 {
		t.Fatalf("embedding top-k above ceiling: %v", out.EmbeddingTopK)
	}
}

func TestApplyRetrievalFeedbackChannelHedgeUpdate(t *testing.T) {
	cfg := &types.RetrievalConfig{
		EmbeddingTopK: 50,
		ChannelWeights: map[string]map[string]float64{
			"*": {types.ChannelVector: 0.6, types.ChannelKeyword: 0.4},
		},
	}
	channelResults := map[string][]*types.SearchResult{
		types.ChannelVector:  {{ID: "a"}, {ID: "b"}, {ID: "c"}},
		types.ChannelKeyword: {}, // weakest channel gets boosted
	}
	out := ApplyRetrievalFeedback(cfg, string(types.RetrievalIntentFactual), channelResults)

	row := out.ChannelWeights["*"]
	if row == nil {
		t.Fatalf("wildcard channel row missing after feedback")
	}
	if row[types.ChannelKeyword] <= 0.4 {
		t.Fatalf("weakest channel not boosted: keyword weight %v", row[types.ChannelKeyword])
	}
	// Row mass must be preserved by renormalization.
	total := row[types.ChannelVector] + row[types.ChannelKeyword]
	if math.Abs(total-1.0) > 1e-9 {
		t.Fatalf("channel weight row not renormalized: total %v", total)
	}
	// Intent-specific row must be seeded and boosted too.
	intentRow := out.ChannelWeights[string(types.RetrievalIntentFactual)]
	if intentRow == nil {
		t.Fatalf("intent channel row missing after feedback")
	}
	if intentRow[types.ChannelKeyword] <= 0 {
		t.Fatalf("intent row weakest channel not boosted: %v", intentRow[types.ChannelKeyword])
	}
}

func TestApplyRetrievalFeedbackSeedsMissingWeights(t *testing.T) {
	out := ApplyRetrievalFeedback(nil, "", map[string][]*types.SearchResult{
		types.ChannelVector: {{ID: "a"}},
		types.ChannelGraph:  {},
	})
	if out.ChannelWeights["*"] == nil {
		t.Fatalf("wildcard row not seeded from defaults")
	}
	if out.ChannelWeights["*"][types.ChannelGraph] <= 0 {
		t.Fatalf("weakest channel (graph) not boosted in seeded row")
	}
}
