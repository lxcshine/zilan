// Package chunker - profiler_thresholds_test.go pins the FR-15 三档路由阈值：
// 标题感知档要求足够的 Markdown 标题密度，启发式档要求足够的结构信号，两者都不
// 满足时回退递归（legacy）档。边界值本身必须稳定，否则存量文档的归档会在一次
// 无关改动后悄悄漂移。
package chunker

import (
	"fmt"
	"strings"
	"testing"
)

// fr15Filler returns n lines of neutral prose that match no structural pattern.
func fr15Filler(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString("这是一段普通的正文内容。\n")
	}
	return sb.String()
}

func hasTier(chain []StrategyTier, tier StrategyTier) bool {
	for _, t := range chain {
		if t == tier {
			return true
		}
	}
	return false
}

// Doc with exactly `headings` H2s spread over `lines` total lines.
func fr15HeadingDoc(headings, fillerLines int) string {
	var sb strings.Builder
	sb.WriteString("# 文档标题\n\n")
	for i := 0; i < headings; i++ {
		sb.WriteString(fmt.Sprintf("## 小节 %d\n", i))
		sb.WriteString(fr15Filler(fillerLines / (headings + 1)))
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestSelectStrategy_NilProfileUsesLegacy(t *testing.T) {
	if chain := SelectStrategy(nil); !tiersEqual(chain, []StrategyTier{TierLegacy}) {
		t.Errorf("nil profile should fall back to legacy, got %v", chain)
	}
}

func TestSelectStrategy_HeadingThresholdIsThreeHeadings(t *testing.T) {
	// 1 H1 + 1 H2 = 2 headings → below the >= 3 threshold.
	two := ProfileDocument(fr15HeadingDoc(1, 60))
	if two.MdHeadingTotal != 2 {
		t.Fatalf("fixture should have 2 headings, got %d", two.MdHeadingTotal)
	}
	if hasTier(SelectStrategy(two), TierHeading) {
		t.Error("2 headings must not select the heading tier")
	}

	// 1 H1 + 2 H2 = 3 headings → at the threshold.
	three := ProfileDocument(fr15HeadingDoc(2, 60))
	if three.MdHeadingTotal != 3 {
		t.Fatalf("fixture should have 3 headings, got %d", three.MdHeadingTotal)
	}
	if !hasTier(SelectStrategy(three), TierHeading) {
		t.Error("3 headings with sufficient density must select the heading tier")
	}
}

// HeadingDensity must stay above 0.005: a handful of headings in a very long
// document are noise, not structure.
func TestSelectStrategy_HeadingDensityThreshold(t *testing.T) {
	sparse := ProfileDocument(fr15HeadingDoc(3, 1200))
	if sparse.MdHeadingTotal != 4 {
		t.Fatalf("fixture should have 4 headings, got %d", sparse.MdHeadingTotal)
	}
	if sparse.HeadingDensity() > 0.005 {
		t.Fatalf("fixture density should be below the threshold, got %v", sparse.HeadingDensity())
	}
	if hasTier(SelectStrategy(sparse), TierHeading) {
		t.Error("sparse headings must not select the heading tier")
	}
}

func TestSelectStrategy_HeuristicMarkerThresholdIsFive(t *testing.T) {
	numbered := func(n int) string {
		var sb strings.Builder
		for i := 1; i <= n; i++ {
			sb.WriteString(fmt.Sprintf("%d. 小节标题\n", i))
			sb.WriteString(fr15Filler(4))
		}
		return sb.String()
	}

	four := ProfileDocument(numbered(4))
	if four.HeuristicMarkerTotal() != 4 {
		t.Fatalf("fixture should expose 4 markers, got %d", four.HeuristicMarkerTotal())
	}
	if hasTier(SelectStrategy(four), TierHeuristic) {
		t.Error("4 markers must not select the heuristic tier")
	}

	five := ProfileDocument(numbered(5))
	if five.HeuristicMarkerTotal() != 5 {
		t.Fatalf("fixture should expose 5 markers, got %d", five.HeuristicMarkerTotal())
	}
	if !hasTier(SelectStrategy(five), TierHeuristic) {
		t.Error("5 markers must select the heuristic tier")
	}
}

// A single page break or chapter marker is enough: both are unambiguous page
// level structure even when the marker total is below 5.
func TestSelectStrategy_FormFeedAndChapterMarkersAloneQualify(t *testing.T) {
	pageBreak := ProfileDocument(fr15Filler(20) + "\f\n" + fr15Filler(20))
	if pageBreak.FormFeedCount != 1 {
		t.Fatalf("fixture should have one form feed, got %d", pageBreak.FormFeedCount)
	}
	if !hasTier(SelectStrategy(pageBreak), TierHeuristic) {
		t.Error("a form feed alone must select the heuristic tier")
	}

	chinese := ProfileDocument(fr15Filler(10) + "第一章 绪论\n" + fr15Filler(10))
	if chinese.ChineseChapterCount != 1 {
		t.Fatalf("fixture should have one Chinese chapter marker, got %d", chinese.ChineseChapterCount)
	}
	if !hasTier(SelectStrategy(chinese), TierHeuristic) {
		t.Error("a chapter marker alone must select the heuristic tier")
	}
}

// A document with neither heading structure nor heuristic signals must land on
// the recursive (legacy) tier — PRD FR-15 的第三档回退。
func TestSelectStrategy_PlainProseFallsBackToRecursive(t *testing.T) {
	plain := ProfileDocument(strings.Repeat("这是一段没有任何结构信号的正文内容。\n", 40))
	chain := SelectStrategy(plain)
	if !tiersEqual(chain, []StrategyTier{TierLegacy}) {
		t.Errorf("plain prose should resolve to [legacy], got %v", chain)
	}
}

// When both tiers qualify the chain keeps the documented order and always ends
// with legacy so callers never run out of fallbacks.
func TestSelectStrategy_ChainOrderAndLegacyTail(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("# 标题\n\n")
	for i := 0; i < 4; i++ {
		sb.WriteString(fmt.Sprintf("## 小节 %d\n", i))
		sb.WriteString("第一章 绪论\n")
		sb.WriteString(fr15Filler(6))
	}
	profile := ProfileDocument(sb.String())
	chain := SelectStrategy(profile)
	if !tiersEqual(chain, []StrategyTier{TierHeading, TierHeuristic, TierLegacy}) {
		t.Errorf("chain = %v, want [heading heuristic legacy]", chain)
	}
	if chain[len(chain)-1] != TierLegacy {
		t.Error("the chain must always end with legacy")
	}
}

// DominantHeadingLevel drives which heading depth becomes a section boundary.
func TestDominantHeadingLevel_PrefersShallowBackbone(t *testing.T) {
	doc := "# A\n\n## A1\n\n### A1a\n\n## A2\n\n### A2a\n\n## A3\n\n### A3a\n"
	p := ProfileDocument(doc)
	if got := p.DominantHeadingLevel(); got != 2 {
		t.Errorf("dominant level = %d, want 2 (first level with >= 3 occurrences)", got)
	}

	small := ProfileDocument("# A\n\n## A1\n\ntext\n")
	if got := small.DominantHeadingLevel(); got != 2 {
		t.Errorf("small doc dominant level = %d, want the deepest present level 2", got)
	}

	none := ProfileDocument("plain text only\n")
	if got := none.DominantHeadingLevel(); got != 0 {
		t.Errorf("document without headings should report 0, got %d", got)
	}
}
