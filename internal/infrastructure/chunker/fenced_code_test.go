// Package chunker - fenced_code_test.go covers the CommonMark-flavoured fence
// scanner introduced for FR-15. The scanner decides which lines count as code,
// which in turn decides whether `#`-prefixed lines inside code blocks inflate
// the profiler's heading statistics and route a document to the heading tier.
package chunker

import (
	"strings"
	"testing"
)

// scanFence feeds every line to a fresh markdownFence and returns one
// (code, delimiter) pair per line.
func scanFence(lines ...string) [][2]bool {
	var fence markdownFence
	out := make([][2]bool, 0, len(lines))
	for _, line := range lines {
		code, delimiter := fence.consume(line)
		out = append(out, [2]bool{code, delimiter})
	}
	return out
}

func TestMarkdownFence_BacktickOpenClose(t *testing.T) {
	lines := []string{"before", "```go", "x := 1", "```", "after"}
	got := scanFence(lines...)
	want := [][2]bool{{false, false}, {true, true}, {true, false}, {true, true}, {false, false}}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("line %d %q: got %v want %v", i, lines[i], got[i], want[i])
		}
	}
}

func TestMarkdownFence_TildeOpenClose(t *testing.T) {
	lines := []string{"~~~", "# not a heading", "~~~", "prose"}
	got := scanFence(lines...)
	want := [][2]bool{{true, true}, {true, false}, {true, true}, {false, false}}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("line %d %q: got %v want %v", i, lines[i], got[i], want[i])
		}
	}
}

// An unclosed fence stays active to the end of the input (CommonMark) so a
// truncated extract never leaks code lines into the heading statistics.
func TestMarkdownFence_UnclosedStaysActive(t *testing.T) {
	lines := []string{"```", "code", "# still code", "more code"}
	for i, res := range scanFence(lines...) {
		if !res[0] {
			t.Errorf("line %d %q should stay inside the unclosed fence", i, lines[i])
		}
	}
}

// Four or more leading spaces make a line indented code, not a fence. Inside a
// fence the same line is plain code content.
func TestMarkdownFence_IndentedFourSpacesIsNotAFence(t *testing.T) {
	lines := []string{"    ```", "text"}
	got := scanFence(lines...)
	if got[0][0] || got[1][0] {
		t.Errorf("over-indented fence must not open a block: %v", got)
	}

	inside := []string{"```", "    ```", "code"}
	got = scanFence(inside...)
	if !got[1][0] || got[1][1] {
		t.Errorf("over-indented fence inside a block is content, not a delimiter: %v", got[1])
	}
	if !got[2][0] {
		t.Error("the block must still be open after an indented pseudo-fence")
	}
}

// Up to three spaces of indentation are allowed before a fence delimiter.
func TestMarkdownFence_UpToThreeSpacesIndentOpens(t *testing.T) {
	got := scanFence("   ```", "code")
	if !got[0][1] || !got[1][0] {
		t.Errorf("3-space indented fence should open a block: %v", got)
	}
}

func TestMarkdownFence_CloserMustMatchMarkerAndLength(t *testing.T) {
	// A ~~~ line cannot close a ``` block.
	got := scanFence("```", "~~~", "code")
	if !got[2][0] {
		t.Error("mismatched marker must not close the fence")
	}
	// A shorter backtick run cannot close a longer opener.
	got = scanFence("````", "```", "code", "````", "after")
	if !got[1][0] || got[1][1] {
		t.Errorf("shorter run is content, not a delimiter: %v", got[1])
	}
	if !got[2][0] {
		t.Error("block should still be open after the shorter run")
	}
	if !got[3][1] || got[4][0] {
		t.Errorf("longer run must close the block: %v", got)
	}
}

// A closing fence may not carry trailing text; an opening backtick fence may
// not contain a backtick in its info string.
func TestMarkdownFence_InfoStringRules(t *testing.T) {
	got := scanFence("```", "``` trailing text", "code")
	if !got[2][0] {
		t.Error("a fence line with trailing text must not close the block")
	}

	got = scanFence("```a`b", "# heading looking line")
	if got[0][0] || got[1][0] {
		t.Errorf("backtick info string containing a backtick is not a fence: %v", got)
	}
}

func TestMarkdownFence_CRLFSuffixHandled(t *testing.T) {
	got := scanFence("```go\r", "code\r", "```\r", "after\r")
	if !got[0][1] || !got[1][0] || !got[2][1] || got[3][0] {
		t.Errorf("CRLF line endings must not break fence tracking: %v", got)
	}
}

func TestMarkdownFence_EmptyLineIsContent(t *testing.T) {
	got := scanFence("```", "", "```")
	if !got[1][0] || got[1][1] {
		t.Errorf("blank line inside a fence is content: %v", got[1])
	}
}

// ---------------------------------------------------------------------------
// Profiler integration — pseudo headings inside code must not count.
// ---------------------------------------------------------------------------

// A shell script full of `#` comments must not be mistaken for a
// heading-structured document: that would route it to the heading tier and
// split it at comment lines.
func TestProfileDocument_PseudoHeadingsInBacktickFenceIgnored(t *testing.T) {
	doc := "Intro paragraph.\n\n```bash\n# install deps\n# run tests\n# ship it\necho hi\n```\n\nClosing paragraph.\n"
	p := ProfileDocument(doc)
	if p.MdHeadingTotal != 0 {
		t.Errorf("comments inside ``` must not count as headings, got %d", p.MdHeadingTotal)
	}
	if !p.HasCode {
		t.Error("HasCode should be true for a fenced block")
	}
	for _, tier := range SelectStrategy(p) {
		if tier == TierHeading {
			t.Errorf("code-only pseudo headings must not select the heading tier: %v", tier)
		}
	}
}

// Same guarantee for ~~~ fences, which the previous "```"-prefix toggle missed.
func TestProfileDocument_PseudoHeadingsInTildeFenceIgnored(t *testing.T) {
	doc := "Intro paragraph.\n\n~~~\n# install deps\n# run tests\n# ship it\necho hi\n~~~\n\nClosing paragraph.\n"
	p := ProfileDocument(doc)
	if p.MdHeadingTotal != 0 {
		t.Errorf("comments inside ~~~ must not count as headings, got %d", p.MdHeadingTotal)
	}
	if !p.HasCode {
		t.Error("HasCode should be true for a ~~~ fenced block")
	}
	if p.CodeRatio <= 0 {
		t.Errorf("CodeRatio should account for fence content, got %v", p.CodeRatio)
	}
	for _, tier := range SelectStrategy(p) {
		if tier == TierHeading {
			t.Errorf("~~~ pseudo headings must not select the heading tier: %v", tier)
		}
	}
}

// A truncated document whose fence never closes must not have its remaining
// `#` lines counted as headings.
func TestProfileDocument_UnclosedFenceSuppressesHeadings(t *testing.T) {
	doc := "# Real heading\n\n```python\n# comment one\n# comment two\nx = 1\n"
	p := ProfileDocument(doc)
	if p.MdHeadingTotal != 1 {
		t.Errorf("only the real heading should count, got %d", p.MdHeadingTotal)
	}
}

// Fence delimiter lines are not code content, so CodeRatio measures the body.
func TestProfileDocument_CodeRatioExcludesFenceLines(t *testing.T) {
	doc := "```\naaaa\nbbbb\n```\n"
	p := ProfileDocument(doc)
	// Only the two body lines count (8 runes); the fence lines and the
	// trailing newlines do not.
	want := 8.0 / float64(p.TotalChars)
	if p.CodeRatio < want-1e-9 || p.CodeRatio > want+1e-9 {
		t.Errorf("CodeRatio = %v, want %v", p.CodeRatio, want)
	}
}

// The heading splitter shares the scanner: a `#` line inside a ~~~ block must
// not become a section boundary.
func TestFindHeadingBoundaries_SkipsTildeFencedHeadings(t *testing.T) {
	doc := "# Real one\n\nbody text\n\n~~~\n# Fake heading\n~~~\n\n# Real two\n\ntail\n"
	bounds := findHeadingBoundaries(doc, 1)
	// The document opens with a heading, so it reuses the leading boundary at
	// offset 0 instead of adding one; "# Real two" adds the second.
	if len(bounds) != 2 {
		t.Fatalf("expected 2 boundaries, got %d: %+v", len(bounds), bounds)
	}
	if !strings.Contains(bounds[0].line, "Real one") {
		t.Errorf("leading boundary should carry the first heading: %+v", bounds[0])
	}
	if !strings.Contains(bounds[1].line, "Real two") {
		t.Errorf("second boundary should be the real heading after the fence: %+v", bounds[1])
	}
	for _, b := range bounds {
		if strings.Contains(b.line, "Fake") {
			t.Errorf("fenced pseudo heading became a boundary: %+v", b)
		}
	}
}

// An over-indented fence inside a list item must not swallow the rest of the
// document: the old toggle flipped its state on any "```" prefix.
func TestFindHeadingBoundaries_IndentedFenceDoesNotSwallowDocument(t *testing.T) {
	doc := "intro\n\n- item\n\n    ```\n    code\n    ```\n\n# Chapter\n\nbody\n"
	bounds := findHeadingBoundaries(doc, 1)
	found := false
	for _, b := range bounds {
		if strings.Contains(b.line, "# Chapter") {
			found = true
		}
	}
	if !found {
		t.Errorf("real heading after an indented fence should still be a boundary: %+v", bounds)
	}
}

func TestObserveSubHeadings_SkipsFencedContent(t *testing.T) {
	h := NewHeadingHierarchy()
	observeSubHeadings([]rune("# Top\n\n~~~\n### Fake sub\n~~~\n\n### Real sub\n"), 1, h)
	bc := h.BreadcrumbWithHashes()
	if strings.Contains(bc, "Fake sub") {
		t.Errorf("fenced pseudo sub-heading leaked into the breadcrumb: %q", bc)
	}
	if !strings.Contains(bc, "Real sub") {
		t.Errorf("real sub-heading missing from breadcrumb: %q", bc)
	}
}
