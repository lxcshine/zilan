// Package chunker - parent_child_offsets_test.go pins the child-offset contract
// that FR-05 citation tracing depends on: a child chunk's Start/End must point
// at the *source* text even when its parent carries a synthetic (source-less)
// continued table header. knowledge_process.go feeds these offsets straight
// into sourceIndex.Locators(child.Start, child.End).
package chunker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/sourceloc"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestParentChildTableSourceOffsets(t *testing.T) {
	splitters := []struct {
		name  string
		split func(string, SplitterConfig, SplitterConfig) ParentChildResult
	}{
		{"legacy", SplitTextParentChild},
		{"adaptive", SplitParentChild},
		{"diagnostics", func(text string, parent, child SplitterConfig) ParentChildResult {
			result, _ := SplitParentChildWithDiagnostics(text, parent, child)
			return result
		}},
	}
	for _, fixture := range []struct {
		name, newline, row string
	}{
		{"ascii", "\n", "| apple | red |"},
		{"unicode", "\n", "| 苹果🍎 | 北京 |"},
		{"crlf", "\r\n", "| 苹果🍎 | 北京 |"},
	} {
		header := "| Item | City |" + fixture.newline + "| --- | --- |" + fixture.newline
		// Repeated rows also catch attempts to recover positions with a text search.
		text := "Intro" + fixture.newline + header +
			strings.Repeat(fixture.row+fixture.newline, 12) + "| last | row |" + fixture.newline
		parentCfg := SplitterConfig{
			ChunkSize: 80, ChunkOverlap: 5, Separators: []string{"\n\n", "\n"}, Strategy: StrategyLegacy,
		}
		// Ensure this exercises a parent with a synthetic header, not merely
		// children with headers inside a single source-backed parent.
		parents := SplitText(text, parentCfg)
		sawSyntheticParent := false
		for _, parent := range parents {
			if len([]rune(parent.Content)) > parent.End-parent.Start {
				sawSyntheticParent = true
			}
		}
		if !sawSyntheticParent {
			t.Fatalf("%s fixture did not produce a parent with a synthetic header", fixture.name)
		}
		for _, splitter := range splitters {
			// 12 isolates the virtual header as a child; 50 re-splits table
			// rows; 160 keeps the parent intact (ParentIndex == -1).
			for _, childSize := range []int{12, 50, 160} {
				t.Run(fmt.Sprintf("%s/%s/child=%d", fixture.name, splitter.name, childSize), func(t *testing.T) {
					childCfg := parentCfg
					childCfg.ChunkSize = childSize
					result := splitter.split(text, parentCfg, childCfg)
					if len(result.Children) == 0 {
						t.Fatal("expected children")
					}
					textRunes := []rune(text)
					covered := make([]bool, len(textRunes))
					var chunks []Chunk
					for i, child := range result.Children {
						if child.Seq != i {
							t.Errorf("child %d has sequence %d", i, child.Seq)
						}
						if child.Start < 0 || child.End > len(textRunes) || child.Start >= child.End {
							t.Fatalf("child %d has invalid source span [%d, %d), document length %d",
								i, child.Start, child.End, len(textRunes))
						}
						content := []rune(child.Content)
						span := child.End - child.Start
						if span > len(content) {
							t.Fatalf("child %d source span exceeds content length", i)
						}
						want := string(textRunes[child.Start:child.End])
						if got := string(content[len(content)-span:]); got != want {
							t.Fatalf("child %d source span [%d, %d): got suffix %q, want %q",
								i, child.Start, child.End, got, want)
						}
						if child.ParentIndex >= 0 {
							if child.ParentIndex >= len(result.Parents) {
								t.Fatalf("child %d has invalid parent index %d", i, child.ParentIndex)
							}
							parent := result.Parents[child.ParentIndex]
							if child.Start < parent.Start || child.End > parent.End {
								t.Errorf("child %d source span escapes its parent", i)
							}
						} else if childSize == 160 && strings.Contains(child.Content, fixture.row) {
							if !strings.Contains(child.Content, "| Item | City |") {
								t.Errorf("child %d lost table header context", i)
							}
						}
						for pos := child.Start; pos < child.End; pos++ {
							covered[pos] = true
						}
						chunks = append(chunks, child.Chunk)
					}
					for pos, ok := range covered {
						if !ok {
							t.Fatalf("source rune %d is missing from children", pos)
						}
					}
					if got := restoreTextFromChunks(chunks); got != text {
						t.Fatalf("source reconstruction mismatch: got %q, want %q", got, text)
					}
				})
			}
		}
	}
}

// A citation resolved from child offsets must name the row the child actually
// holds — with the old additive shift it named the neighbouring rows instead.
func TestParentChildTableCitationUsesSourceRow(t *testing.T) {
	prefix := "| Item | City |\n| --- | --- |\n" + strings.Repeat("| 苹果🍎 | 北京 |\n", 12)
	lastRow := "| last | row |\n"
	text := prefix + lastRow
	index := sourceloc.NewIndex(text, []types.SourceBlock{
		{Start: 0, End: len([]rune(prefix)), Locator: types.SourceLocator{
			Type: types.SourceLocatorSheet, Sheet: "Sheet1", RowStart: 1, RowEnd: 13,
		}},
		{Start: len([]rune(prefix)), End: len([]rune(text)), Locator: types.SourceLocator{
			Type: types.SourceLocatorSheet, Sheet: "Sheet1", RowStart: 14, RowEnd: 14,
		}},
	})
	parentCfg := SplitterConfig{
		ChunkSize: 80, ChunkOverlap: 5, Separators: []string{"\n\n", "\n"}, Strategy: StrategyLegacy,
	}
	childCfg := parentCfg
	childCfg.ChunkSize = 12
	result := SplitParentChild(text, parentCfg, childCfg)
	for _, child := range result.Children {
		if child.Content != lastRow {
			continue
		}
		// ProcessDocument assigns source evidence with these exact offsets.
		locators := index.Locators(child.Start, child.End)
		if len(locators) != 1 {
			t.Fatalf("last row has %d source locators, want one", len(locators))
		}
		loc := locators[0]
		if loc.Sheet != "Sheet1" || loc.RowStart != 14 || loc.RowEnd != 14 || loc.Quote != "| last | row |" {
			t.Fatalf("last row points to the wrong source: %+v", loc)
		}
		return
	}
	t.Fatal("last table row was not retained as a child")
}

// mapChildrenToSource is a no-op shift for parents without a synthetic prefix
// and must never emit an empty or reversed span.
func TestMapChildrenToSource(t *testing.T) {
	plain := Chunk{Content: "abcdef", Start: 100, End: 106}
	got := mapChildrenToSource(plain, []Chunk{
		{Content: "abc", Start: 0, End: 3},
		{Content: "def", Start: 3, End: 6},
	})
	if len(got) != 2 {
		t.Fatalf("expected both children, got %+v", got)
	}
	if got[0].Start != 100 || got[0].End != 103 || got[1].Start != 103 || got[1].End != 106 {
		t.Errorf("plain parent should shift additively: %+v", got)
	}

	// Parent whose Content starts with a 10 rune synthetic header.
	withHeader := Chunk{Content: "0123456789abcdef", Start: 100, End: 106}
	got = mapChildrenToSource(withHeader, []Chunk{
		{Content: "01234", Start: 0, End: 5},    // header only → dropped
		{Content: "56789ab", Start: 5, End: 12}, // straddles the seam
		{Content: "cdef", Start: 12, End: 16},
	})
	if len(got) != 2 {
		t.Fatalf("header-only child should be dropped, got %+v", got)
	}
	if got[0].Start != 100 || got[0].End != 102 {
		t.Errorf("straddling child should clamp to the parent start: %+v", got[0])
	}
	if got[1].Start != 102 || got[1].End != 106 {
		t.Errorf("trailing child should map onto the source tail: %+v", got[1])
	}
}
