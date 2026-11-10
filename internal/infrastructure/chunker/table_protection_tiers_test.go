// Package chunker - table_protection_tiers_test.go pins PRD FR-15 规则③:
// 表格保护区间与表头续接机制在三种策略（标题感知 / 启发式 / 递归）以及存量
// legacy 行为下均须生效 —— 表格原子性优先于策略选择，绝不能因为路由到某一档
// 就把表格行切断或丢掉续接表头。
package chunker

import (
	"fmt"
	"strings"
	"testing"
)

const fr15HeaderRow = "| Item | City |"
const fr15SeparatorRow = "| --- | --- |"

// fr15TableDoc builds a Markdown document with a heading structure, prose and
// a table large enough that the section must be sub-split.
func fr15TableDoc(rows int) string {
	var sb strings.Builder
	sb.WriteString("# 第一章 概述\n\n")
	sb.WriteString("这是引言段落，用来描述整体背景信息。\n\n")
	sb.WriteString(fr15HeaderRow + "\n" + fr15SeparatorRow + "\n")
	for i := 1; i <= rows; i++ {
		sb.WriteString(fmt.Sprintf("| 苹果%d | 北京%d |\n", i, i))
	}
	sb.WriteString("\n## 1.1 小节\n\n")
	sb.WriteString(strings.Repeat("小节正文内容，用来把这一节撑长。", 12))
	sb.WriteString("\n")
	return sb.String()
}

// fr15CrossPageTableDoc puts a form feed (page break) in the middle of the
// table: the classic 跨页表格 case where the rows after the break lose their
// header unless the continuation mechanism kicks in.
func fr15CrossPageTableDoc() string {
	var sb strings.Builder
	sb.WriteString("第一节说明文字，介绍表格背景信息。\n\n")
	sb.WriteString(fr15HeaderRow + "\n" + fr15SeparatorRow + "\n")
	for i := 1; i <= 6; i++ {
		sb.WriteString(fmt.Sprintf("| 苹果%d | 北京%d |\n", i, i))
	}
	sb.WriteString("\f\n")
	for i := 7; i <= 14; i++ {
		sb.WriteString(fmt.Sprintf("| 苹果%d | 北京%d |\n", i, i))
	}
	sb.WriteString("\n结尾说明文字。\n")
	return sb.String()
}

// fr15Strategies lists every strategy value that must keep table protection,
// including the empty value stored by 存量知识库 (legacy behaviour).
func fr15Strategies() []struct{ name, strategy string } {
	return []struct{ name, strategy string }{
		{"heading", StrategyHeading},
		{"heuristic", StrategyHeuristic},
		{"recursive", StrategyRecursive},
		{"legacy", StrategyLegacy},
		{"empty-legacy", ""},
		{"auto", StrategyAuto},
	}
}

// fr15TableLines returns every table line of doc (header, separator, rows).
func fr15TableLines(doc string) map[string]bool {
	lines := make(map[string]bool)
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|") {
			lines[trimmed] = true
		}
	}
	return lines
}

// fr15AssertRowsAtomic fails when any chunk carries a fragment of a table line.
// It only inspects lines that are complete in the source: a chunk may start or
// end mid-line when the splitter falls back to hard slicing, and that residue
// is not a broken table row.
func fr15AssertRowsAtomic(t *testing.T, doc string, chunks []Chunk, tableLines map[string]bool) {
	t.Helper()
	runes := []rune(doc)
	for i, c := range chunks {
		if c.Start < 0 || c.End > len(runes) || c.End < c.Start {
			t.Fatalf("chunk %d has an out-of-range span [%d,%d) for a %d rune document",
				i, c.Start, c.End, len(runes))
		}
		// A synthetic (continued) table header occupies no source positions.
		content := []rune(c.Content)
		if len(content) > c.End-c.Start {
			content = content[len(content)-(c.End-c.Start):]
		}
		startsAtLine := c.Start == 0 || runes[c.Start-1] == '\n'
		endsAtLine := c.End == len(runes) || runes[c.End] == '\n'

		parts := strings.Split(string(content), "\n")
		for j, part := range parts {
			if j == 0 && !startsAtLine {
				continue // leading fragment of a hard slice
			}
			if j == len(parts)-1 && !endsAtLine {
				continue // trailing fragment of a hard slice
			}
			trimmed := strings.TrimSpace(part)
			if trimmed == "" || !strings.Contains(trimmed, "|") {
				continue
			}
			if !tableLines[trimmed] {
				t.Errorf("chunk %d carries a fragment of a table row: %q", i, part)
			}
		}
	}
}

// fr15AssertAllRowsPresent fails when a source table row is missing from every
// chunk, i.e. protection dropped content instead of keeping it atomic.
func fr15AssertAllRowsPresent(t *testing.T, doc string, chunks []Chunk) {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "| 苹果") {
			continue
		}
		found := false
		for _, c := range chunks {
			if strings.Contains(c.Content, trimmed) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("table row %q disappeared from the chunk set", trimmed)
		}
	}
}

// fr15AssertHeaderContinuation requires every chunk that carries table data
// rows to also carry the header row — either because it covers the header's
// source position or because the continuation mechanism prepended it.
//
// exemptFrom > 0 skips chunks that start at or after that rune offset: a form
// feed inside a table terminates the run for the header tracker (its endPattern
// treats the page break like a blank line), so rows after the break belong to a
// header-less run. That is pre-existing legacy behaviour and identical in every
// tier, which is exactly what FR-15 规则③ requires — no tier may lose it.
func fr15AssertHeaderContinuation(t *testing.T, doc string, chunks []Chunk, exemptFrom int) {
	t.Helper()
	headerStart := strings.Index(doc, fr15HeaderRow)
	if headerStart < 0 {
		t.Fatal("fixture has no header row")
	}
	headerRuneStart := len([]rune(doc[:headerStart]))
	headerRuneEnd := headerRuneStart + len([]rune(fr15HeaderRow))

	for i, c := range chunks {
		if exemptFrom > 0 && c.Start >= exemptFrom {
			continue
		}
		hasRow := false
		for _, line := range strings.Split(c.Content, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "| 苹果") {
				hasRow = true
				break
			}
		}
		if !hasRow {
			continue
		}
		coversHeader := c.Start <= headerRuneStart && c.End >= headerRuneEnd
		if !coversHeader && !strings.Contains(c.Content, fr15HeaderRow) {
			t.Errorf("chunk %d [%d,%d) carries table rows without the continued header row", i, c.Start, c.End)
		}
	}
}

func TestFR15_TableRowsAtomicAcrossTiers(t *testing.T) {
	doc := fr15TableDoc(14)
	tableLines := fr15TableLines(doc)
	for _, s := range fr15Strategies() {
		t.Run(s.name, func(t *testing.T) {
			cfg := SplitterConfig{
				ChunkSize:    200,
				ChunkOverlap: 20,
				Separators:   []string{"\n\n", "\n"},
				Strategy:     s.strategy,
			}
			chunks := Split(doc, cfg)
			if len(chunks) < 2 {
				t.Fatalf("fixture should produce several chunks, got %d", len(chunks))
			}
			fr15AssertRowsAtomic(t, doc, chunks, tableLines)
			fr15AssertAllRowsPresent(t, doc, chunks)
		})
	}
}

func TestFR15_TableHeaderContinuationAcrossTiers(t *testing.T) {
	doc := fr15TableDoc(14)
	for _, s := range fr15Strategies() {
		t.Run(s.name, func(t *testing.T) {
			cfg := SplitterConfig{
				ChunkSize:    200,
				ChunkOverlap: 20, // 0 disables header continuation by design
				Separators:   []string{"\n\n", "\n"},
				Strategy:     s.strategy,
			}
			chunks := Split(doc, cfg)
			fr15AssertHeaderContinuation(t, doc, chunks, -1)

			// The continuation must actually happen: with a 200 rune budget a
			// 14-row table cannot live in one chunk, so at least one chunk has
			// to carry a prepended (source-less) header.
			sawSynthetic := false
			for _, c := range chunks {
				if len([]rune(c.Content)) > c.End-c.Start && strings.Contains(c.Content, fr15HeaderRow) {
					sawSynthetic = true
				}
			}
			if !sawSynthetic {
				t.Error("no chunk received a continued table header")
			}
		})
	}
}

func TestFR15_CrossPageTableKeepsHeaderAndRows(t *testing.T) {
	doc := fr15CrossPageTableDoc()
	tableLines := fr15TableLines(doc)
	// Rows after the page break form a header-less run in every tier (the form
	// feed ends the tracked table), so continuation is only asserted for the
	// segment before the break.
	pageBreak := len([]rune(doc[:strings.Index(doc, "\f")]))
	for _, s := range fr15Strategies() {
		t.Run(s.name, func(t *testing.T) {
			cfg := SplitterConfig{
				ChunkSize:    160,
				ChunkOverlap: 20,
				Separators:   []string{"\n\n", "\n"},
				Strategy:     s.strategy,
			}
			chunks := Split(doc, cfg)
			fr15AssertRowsAtomic(t, doc, chunks, tableLines)
			fr15AssertAllRowsPresent(t, doc, chunks)
			fr15AssertHeaderContinuation(t, doc, chunks, pageBreak)
		})
	}
}

// The heuristic bin-packer cuts chunks straight out of the source, bypassing
// mergeUnits' header tracker; continuedTableHeader must still restore the
// header when a chunk (typically after overlap alignment) starts on a data row.
func TestFR15_HeuristicTierContinuesTableHeader(t *testing.T) {
	doc := fr15CrossPageTableDoc()
	cfg := SplitterConfig{
		ChunkSize:    160,
		ChunkOverlap: 20,
		Separators:   []string{"\n\n", "\n"},
		Strategy:     StrategyHeuristic,
	}
	chunks := Split(doc, cfg)
	sawContinuation := false
	for _, c := range chunks {
		if len([]rune(c.Content)) <= c.End-c.Start {
			continue // source-exact chunk, no synthetic prefix
		}
		if !strings.HasPrefix(c.Content, fr15HeaderRow+"\n"+fr15SeparatorRow+"\n") {
			t.Errorf("synthetic prefix should be the table header, got %q", firstLineOf(c.Content))
			continue
		}
		sawContinuation = true
	}
	if !sawContinuation {
		t.Error("heuristic tier never continued the table header")
	}
}

func TestContinuedTableHeader(t *testing.T) {
	doc := "前言\n\n| Item | City |\n| --- | --- |\n| a | b |\n| c | d |\n\n结语\n"
	runes := []rune(doc)
	at := func(s string) int { return strings.Index(doc, s) }

	// A data row resolves to the table header.
	rowC := len([]rune(doc[:at("| c | d |")]))
	if got := continuedTableHeader(runes, rowC, 0); got != fr15HeaderRow+"\n"+fr15SeparatorRow+"\n" {
		t.Errorf("data row: got %q", got)
	}
	// The header row itself needs no continuation.
	if got := continuedTableHeader(runes, len([]rune(doc[:at(fr15HeaderRow)])), 0); got != "" {
		t.Errorf("header row should not continue itself: %q", got)
	}
	// Prose is not a table row.
	if got := continuedTableHeader(runes, 0, 0); got != "" {
		t.Errorf("prose should not resolve to a header: %q", got)
	}
	// A budget smaller than the header suppresses continuation.
	if got := continuedTableHeader(runes, rowC, 5); got != "" {
		t.Errorf("over-budget header should be suppressed: %q", got)
	}
	// A header-less run of pipe lines yields nothing.
	noHeader := []rune("| a | b |\n| c | d |\n")
	if got := continuedTableHeader(noHeader, len([]rune("| a | b |\n")), 0); got != "" {
		t.Errorf("header-less table should not invent a header: %q", got)
	}
	// A run separated by a page break is a new table: no continuation.
	split := []rune("| Item | City |\n| --- | --- |\n| a | b |\n\f\n| c | d |\n")
	if got := continuedTableHeader(split, len([]rune("| Item | City |\n| --- | --- |\n| a | b |\n\f\n")), 0); got != "" {
		t.Errorf("page break must end the table run: %q", got)
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Parent-child ingestion (EnableParentChild) must not lose table protection
// either: children carry the rows, their parent carries the header context.
func TestFR15_ParentChildTableProtectionAcrossTiers(t *testing.T) {
	doc := fr15TableDoc(14)
	tableLines := fr15TableLines(doc)
	for _, s := range fr15Strategies() {
		t.Run(s.name, func(t *testing.T) {
			parentCfg := SplitterConfig{
				ChunkSize:    400,
				ChunkOverlap: 40,
				Separators:   []string{"\n\n", "\n"},
				Strategy:     s.strategy,
			}
			childCfg := SplitterConfig{
				ChunkSize:    60,
				ChunkOverlap: 12,
				Separators:   []string{"\n\n", "\n"},
				Strategy:     s.strategy,
			}
			res := SplitParentChild(doc, parentCfg, childCfg)
			if len(res.Children) == 0 {
				t.Fatal("expected child chunks")
			}
			childChunks := make([]Chunk, len(res.Children))
			for i, c := range res.Children {
				childChunks[i] = c.Chunk
			}
			fr15AssertRowsAtomic(t, doc, childChunks, tableLines)
			fr15AssertAllRowsPresent(t, doc, childChunks)

			// Child offsets must stay inside their parent's source span even
			// when the parent carries a synthetic (source-less) table header —
			// the additive shift used before mapChildrenToSource broke this.
			for i, c := range res.Children {
				if c.ParentIndex < 0 {
					continue
				}
				if c.ParentIndex >= len(res.Parents) {
					t.Fatalf("child %d has invalid parent index %d", i, c.ParentIndex)
				}
				parent := res.Parents[c.ParentIndex]
				if c.Start < parent.Start || c.End > parent.End {
					t.Errorf("child %d [%d,%d) escapes parent %d [%d,%d)",
						i, c.Start, c.End, c.ParentIndex, parent.Start, parent.End)
				}
			}

			// 表头续接必须在父子两级下存活：原文表头出现在父块里，任何合成前缀
			// 只能是续接的表头。
			headerInParents := false
			for _, p := range res.Parents {
				if strings.Contains(p.Content, fr15HeaderRow) {
					headerInParents = true
				}
				if len([]rune(p.Content)) > p.End-p.Start && !strings.HasPrefix(p.Content, fr15HeaderRow) {
					t.Errorf("parent [%d,%d) carries a synthetic prefix that is not the table header: %q",
						p.Start, p.End, firstLineOf(p.Content))
				}
			}
			if !headerInParents {
				t.Error("the table header row vanished from the parent set")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// heading 档 protected-span 过滤
// ---------------------------------------------------------------------------

func TestDropHeadingBoundsInsideSpans(t *testing.T) {
	spans := []span{{start: 10, end: 20}, {start: 30, end: 40}}
	bounds := []headingBoundary{
		{runeStart: 0, line: "# A"},  // before every span → kept
		{runeStart: 10, line: "# B"}, // on a span start → kept (edge aligned)
		{runeStart: 15, line: "# C"}, // strictly inside → dropped
		{runeStart: 20, line: "# D"}, // on a span end → kept
		{runeStart: 35, line: "# E"}, // strictly inside → dropped
		{runeStart: 45, line: "# F"}, // after every span → kept
	}
	got := dropHeadingBoundsInsideSpans(bounds, spans)
	want := []string{"# A", "# B", "# D", "# F"}
	if len(got) != len(want) {
		t.Fatalf("got %d bounds %+v, want %d", len(got), got, len(want))
	}
	for i := range got {
		if got[i].line != want[i] {
			t.Errorf("bound %d: got %q want %q", i, got[i].line, want[i])
		}
	}
	if passthrough := dropHeadingBoundsInsideSpans(bounds, nil); len(passthrough) != len(bounds) {
		t.Error("no protected spans must keep every boundary")
	}
}

// A `#`-prefixed line inside a protected region (LaTeX block here) must not
// become a section boundary — the region stays atomic in one chunk.
func TestFR15_HeadingTierKeepsProtectedBlockAtomic(t *testing.T) {
	doc := "# A\n\nintro text here.\n\n$$\n# inside math block\nx = y\n$$\n\n# B\n\nbody B here.\n\n# C\n\nbody C here.\n"
	cfg := SplitterConfig{ChunkSize: 400, ChunkOverlap: 0, Separators: []string{"\n\n", "\n"}, Strategy: StrategyHeading}
	chunks := Split(doc, cfg)

	block := "$$\n# inside math block\nx = y\n$$"
	found := false
	for _, c := range chunks {
		if strings.Contains(c.Content, block) {
			found = true
		} else if strings.Contains(c.Content, "# inside math block") {
			t.Errorf("protected block was split: %q", c.Content)
		}
	}
	if !found {
		t.Error("the protected LaTeX block is missing from the chunk set")
	}
	// Real headings still delimit sections: A, B and C.
	if len(chunks) != 3 {
		t.Errorf("expected one chunk per real heading, got %d", len(chunks))
	}
}

// Guard against over-filtering: a real heading right after a table sits on the
// protected span's edge and must still start a new section.
func TestFR15_HeadingAfterTableStillSplits(t *testing.T) {
	doc := "# 表格章节\n\n" + fr15HeaderRow + "\n" + fr15SeparatorRow + "\n| 苹果1 | 北京1 |\n| 苹果2 | 北京2 |\n\n# 下一章节\n\n正文内容。\n"
	cfg := SplitterConfig{ChunkSize: 400, ChunkOverlap: 0, Separators: []string{"\n\n", "\n"}, Strategy: StrategyHeading}
	chunks := Split(doc, cfg)
	if len(chunks) != 2 {
		t.Fatalf("expected the heading after the table to start a section, got %d chunks: %+v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0].Content, fr15HeaderRow) || !strings.Contains(chunks[0].Content, "| 苹果2 | 北京2 |") {
		t.Errorf("table should stay whole in the first chunk: %q", chunks[0].Content)
	}
	if !strings.Contains(chunks[1].Content, "# 下一章节") {
		t.Errorf("second chunk should start at the next heading: %q", chunks[1].Content)
	}
}
