// Package chunker - strategy_chain_test.go covers FR-15 的策略解析与回退链：
// 空策略必须保持存量 legacy 行为（PRD §5.4），某一档被 ValidateChunks 拒绝后
// 必须链式回退，且诊断信息要能说明"命中哪一档、其它档为什么被拒"。
package chunker

import (
	"fmt"
	"strings"
	"testing"
)

func headingRichDoc() string {
	var sb strings.Builder
	sb.WriteString("# 第一章 总览\n\n")
	for i := 0; i < 6; i++ {
		sb.WriteString(fmt.Sprintf("## 小节 %d\n\n", i))
		sb.WriteString(strings.Repeat("这一段的正文内容用来撑满一个小节。", 12))
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// A document whose heading tier output is rejected by the validator: many
// sibling H1 sections that are each far below the tiny-chunk floor.
func tinyHeadingDoc() string {
	var sb strings.Builder
	for i := 0; i < 12; i++ {
		sb.WriteString(fmt.Sprintf("# H%d\nshort body %d.\n\n", i, i))
	}
	return sb.String()
}

func tiersEqual(got, want []StrategyTier) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestResolveChainWithProfile_ExplicitStrategiesSkipProfiling(t *testing.T) {
	doc := headingRichDoc()
	cases := []struct {
		strategy string
		want     []StrategyTier
	}{
		{"", []StrategyTier{TierLegacy}},                               // 存量知识库：保持原有切分行为
		{StrategyLegacy, []StrategyTier{TierLegacy}},                   //
		{StrategyRecursive, []StrategyTier{TierLegacy}},                // recursive 是 legacy 的公开别名
		{StrategyHeading, []StrategyTier{TierHeading, TierLegacy}},     //
		{StrategyHeuristic, []StrategyTier{TierHeuristic, TierLegacy}}, //
	}
	for _, tc := range cases {
		t.Run("strategy="+tc.strategy, func(t *testing.T) {
			chain, profile := resolveChainWithProfile(doc, SplitterConfig{Strategy: tc.strategy})
			if !tiersEqual(chain, tc.want) {
				t.Errorf("chain = %v, want %v", chain, tc.want)
			}
			if profile != nil {
				t.Error("explicit strategies must not pay for a profiling pass")
			}
		})
	}
}

func TestResolveChainWithProfile_AutoProfilesDocument(t *testing.T) {
	chain, profile := resolveChainWithProfile(headingRichDoc(), SplitterConfig{Strategy: StrategyAuto})
	if profile == nil {
		t.Fatal("auto strategy must return the profile that drove selection")
	}
	if len(chain) == 0 || chain[0] != TierHeading {
		t.Errorf("heading-rich document should start at the heading tier: %v", chain)
	}
	if chain[len(chain)-1] != TierLegacy {
		t.Errorf("chain must end with the legacy safety net: %v", chain)
	}
}

// An unknown stored value is treated as auto rather than failing closed.
func TestResolveChainWithProfile_UnknownStrategyFallsBackToAuto(t *testing.T) {
	chain, profile := resolveChainWithProfile(headingRichDoc(), SplitterConfig{Strategy: "not-a-strategy"})
	if profile == nil {
		t.Error("unknown strategy should profile like auto")
	}
	if len(chain) == 0 || chain[0] != TierHeading {
		t.Errorf("unknown strategy should behave like auto: %v", chain)
	}
}

// PRD §5.4 存量兼容：空策略的知识库即便文档结构非常适合标题档，也必须逐块等价于
// legacy SplitText，绝不能因为升级而改变切分行为。
func TestSplit_EmptyStrategyKeepsLegacyBehaviourOnHeadingDoc(t *testing.T) {
	doc := headingRichDoc()
	cfg := SplitterConfig{ChunkSize: 300, ChunkOverlap: 30, Separators: []string{"\n\n", "\n"}}

	legacyCfg := cfg
	legacyCfg.Strategy = StrategyLegacy
	empty := Split(doc, cfg)
	legacy := Split(doc, legacyCfg)
	recursive := SplitText(doc, cfg)

	if len(empty) != len(legacy) || len(empty) != len(recursive) {
		t.Fatalf("chunk counts differ: empty=%d legacy=%d SplitText=%d", len(empty), len(legacy), len(recursive))
	}
	for i := range empty {
		if empty[i].Content != legacy[i].Content || empty[i].Content != recursive[i].Content {
			t.Fatalf("chunk %d differs between empty/legacy strategy and SplitText", i)
		}
		if empty[i].Start != recursive[i].Start || empty[i].End != recursive[i].End {
			t.Errorf("chunk %d offsets differ: %+v vs %+v", i, empty[i], recursive[i])
		}
		if empty[i].ContextHeader != "" {
			t.Errorf("legacy behaviour must not attach heading breadcrumbs, chunk %d got %q", i, empty[i].ContextHeader)
		}
	}

	// Sanity check on the fixture: the same document does route to the heading
	// tier under auto, so the equality above is meaningful.
	autoCfg := cfg
	autoCfg.Strategy = StrategyAuto
	if _, diag := SplitWithDiagnostics(doc, autoCfg); diag.SelectedTier != TierHeading {
		t.Fatalf("fixture should select the heading tier under auto, got %s", diag.SelectedTier)
	}
}

func TestSplitWithDiagnostics_RejectedTierFallsBackToLegacy(t *testing.T) {
	doc := tinyHeadingDoc()
	cfg := SplitterConfig{ChunkSize: 400, ChunkOverlap: 20, Separators: []string{"\n\n", "\n"}, Strategy: StrategyAuto}
	chunks, diag := SplitWithDiagnostics(doc, cfg)

	if diag.SelectedTier != TierLegacy {
		t.Errorf("selected tier = %s, want legacy after rejection", diag.SelectedTier)
	}
	if len(diag.Rejected) == 0 {
		t.Fatal("expected at least one rejected tier")
	}
	if diag.Rejected[0].Tier != TierHeading || diag.Rejected[0].Reason == "" {
		t.Errorf("heading tier should be rejected with a reason, got %+v", diag.Rejected[0])
	}
	if !tiersEqual(diag.TierChain, []StrategyTier{TierHeading, TierLegacy}) {
		t.Errorf("tier chain = %v, want [heading legacy]", diag.TierChain)
	}

	// The returned chunks must be exactly the legacy output — the fallback is
	// not allowed to re-shape the document.
	legacy := SplitText(doc, cfg)
	if len(chunks) != len(legacy) {
		t.Fatalf("fallback produced %d chunks, SplitText gives %d", len(chunks), len(legacy))
	}
	for i := range chunks {
		if chunks[i].Content != legacy[i].Content {
			t.Errorf("chunk %d differs from the legacy output", i)
		}
	}

	// Split (no diagnostics) must agree with SplitWithDiagnostics.
	plain := Split(doc, cfg)
	if len(plain) != len(chunks) {
		t.Fatalf("Split returned %d chunks, SplitWithDiagnostics %d", len(plain), len(chunks))
	}
	for i := range plain {
		if plain[i].Content != chunks[i].Content || plain[i].Start != chunks[i].Start {
			t.Errorf("chunk %d differs between Split and SplitWithDiagnostics", i)
		}
	}
}

// Even when every tier is rejected, Split must never return an empty result.
func TestSplit_AlwaysReturnsChunks(t *testing.T) {
	docs := map[string]string{
		"tiny":       "# H\nx",
		"one-liner":  strings.Repeat("a", 5000),
		"table-only": fr15HeaderRow + "\n" + fr15SeparatorRow + "\n" + strings.Repeat("| a | b |\n", 20),
	}
	for name, doc := range docs {
		for _, strategy := range []string{"", StrategyAuto, StrategyHeading, StrategyHeuristic, StrategyRecursive} {
			cfg := SplitterConfig{ChunkSize: 64, ChunkOverlap: 8, Separators: []string{"\n\n", "\n"}, Strategy: strategy}
			if chunks := Split(doc, cfg); len(chunks) == 0 {
				t.Errorf("%s/%s produced no chunks", name, strategy)
			}
		}
	}
}

func TestSplitParentChildWithDiagnostics_MatchesSplitParentChild(t *testing.T) {
	doc := headingRichDoc()
	parentCfg := SplitterConfig{ChunkSize: 600, ChunkOverlap: 60, Separators: []string{"\n\n", "\n"}, Strategy: StrategyAuto}
	childCfg := SplitterConfig{ChunkSize: 150, ChunkOverlap: 15, Separators: []string{"\n\n", "\n"}, Strategy: StrategyAuto}

	want := SplitParentChild(doc, parentCfg, childCfg)
	got, diag := SplitParentChildWithDiagnostics(doc, parentCfg, childCfg)

	if diag == nil {
		t.Fatal("diagnostics must not be nil")
	}
	if diag.SelectedTier != TierHeading {
		t.Errorf("selected tier = %s, want heading", diag.SelectedTier)
	}
	if diag.Profile == nil {
		t.Error("auto strategy should carry the document profile")
	}
	if len(got.Parents) != len(want.Parents) || len(got.Children) != len(want.Children) {
		t.Fatalf("shape mismatch: %d/%d parents, %d/%d children",
			len(got.Parents), len(want.Parents), len(got.Children), len(want.Children))
	}
	for i := range got.Children {
		if got.Children[i].Content != want.Children[i].Content ||
			got.Children[i].Start != want.Children[i].Start ||
			got.Children[i].End != want.Children[i].End {
			t.Errorf("child %d differs between the two entry points", i)
		}
	}
}

func TestSplitParentChild_EmptyText(t *testing.T) {
	cfg := SplitterConfig{ChunkSize: 100, ChunkOverlap: 10}
	if res := SplitParentChild("", cfg, cfg); len(res.Parents) != 0 || len(res.Children) != 0 {
		t.Errorf("empty text should yield an empty result, got %+v", res)
	}
	res, diag := SplitParentChildWithDiagnostics("", cfg, cfg)
	if len(res.Children) != 0 {
		t.Error("empty text should yield no children")
	}
	if diag == nil {
		t.Error("diagnostics must be non-nil even for empty input")
	}
}
